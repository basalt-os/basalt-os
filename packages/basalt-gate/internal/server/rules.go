package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/polkit"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/simulate"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// RuleChange is the action that adds or removes a rule. Loosening is a
// gate request like any other (Critical, always asks); tightening from a
// trusted surface takes effect at once (rules.apply).
const RuleChange = "gate.rule.change"

func (s *Server) noAgents(c *conn, op string) *gate.Reply {
	if !c.roles.Agent {
		return nil
	}
	s.refuseOnce(c.owner()+"|"+op, ledger.Record{UID: c.p.UID, Session: c.roles.Session, Event: "gate.rule.change",
		Outcome: "denied", Data: map[string]any{"who": s.peerWho(c), "op": op,
			"reason": "agents do not read or change approval rules"}})
	return &gate.Reply{Error: "refused: agents do not read or change approval rules"}
}

func (s *Server) ruleView(r policy.Rule) gate.RuleView {
	b, _ := json.Marshal(r)
	v := gate.RuleView{Rule: b, Sentence: r.Sentence(), Ref: r.Ref(), Scope: r.Scope, UID: r.UID, Preset: r.Preset}
	if why, ok := s.state.Paused[r.Key()]; ok {
		v.Paused = why
	}
	if t := r.ExpiresAt(); !t.IsZero() {
		v.ExpiresAt = t.UTC().Format(time.RFC3339)
	}
	return v
}

// visibleRules: the system rules and the caller's own (root: all).
func (s *Server) visibleRules(uid int) []policy.Rule {
	var out []policy.Rule
	for _, r := range s.eng.Rules() {
		if r.Scope != policy.ScopeUser || uid == 0 || r.UID == uid {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) rulesList(c *conn) gate.Reply {
	if r := s.noAgents(c, "rules.list"); r != nil {
		return *r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []gate.RuleView{}
	for _, r := range s.visibleRules(c.p.UID) {
		out = append(out, s.ruleView(r))
	}
	return gate.Reply{OK: true, Rules: out}
}

// parseRule reads one rule from JSON and validates it for a scope.
func (s *Server) parseRule(raw json.RawMessage, scope string, uid int, now time.Time) (policy.Rule, error) {
	var r policy.Rule
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("rule: %v", err)
	}
	if r.Scope != "" || r.UID != 0 || r.Temporary || r.Preset != "" {
		return r, fmt.Errorf("scope, uid, temporary and preset are set by the gate")
	}
	if scope == "" {
		scope = policy.ScopeUser
	}
	if err := policy.Validate(&r, s.o.Reg, scope, now); err != nil {
		return r, err
	}
	if scope == policy.ScopeUser {
		r.UID = uid
	}
	return r, nil
}

// history returns the recorded gate requests a caller may replay.
func (s *Server) history(c *conn, req gate.Request, since time.Time) ([]simulate.Record, error) {
	if len(req.Records) > 0 {
		var recs []simulate.Record
		if err := json.Unmarshal(req.Records, &recs); err != nil {
			return nil, fmt.Errorf("records: %v", err)
		}
		if c.p.UID != 0 {
			own := recs[:0]
			for _, r := range recs {
				if r.UID == c.p.UID {
					own = append(own, r)
				}
			}
			recs = own
		}
		return recs, nil
	}
	if s.o.History == nil {
		return nil, nil
	}
	stored, err := s.o.History(c.p.UID, since)
	if err != nil {
		return nil, err
	}
	out := make([]simulate.Record, 0, len(stored))
	for _, r := range stored {
		out = append(out, simulate.Record{Time: r.Time, UID: r.UID, Event: r.Event, Outcome: r.Outcome, Data: r.Data})
	}
	return out, nil
}

func sinceOf(s string, now time.Time) (time.Time, error) {
	if s == "" {
		s = "7d"
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := policy.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("since %q (7d, 24h or a time)", s)
	}
	return now.Add(-d), nil
}

// rulesDraft validates a rule, renders its sentence, and shows what the
// current rules plus this one would have done in the period.
func (s *Server) rulesDraft(c *conn, req gate.Request) gate.Reply {
	if r := s.noAgents(c, "rules.draft"); r != nil {
		return *r
	}
	now := s.now()
	r, err := s.parseRule(req.Rule, req.Scope, c.p.UID, now)
	if err != nil {
		return gate.Reply{OK: true, Rules: []gate.RuleView{{Rule: req.Rule, Error: err.Error()}}}
	}
	since, err := sinceOf(req.Since, now)
	if err != nil {
		return gate.Reply{Error: err.Error()}
	}
	recs, err := s.history(c, req, since)
	if err != nil {
		return gate.Reply{Error: err.Error()}
	}
	s.mu.Lock()
	rules := append(s.eng.Rules(), r)
	v := s.ruleView(r)
	s.mu.Unlock()
	sim := simulate.Run(s.eng, rules, simulate.Requests(recs), since)
	return gate.Reply{OK: true, Rules: []gate.RuleView{v}, Simulation: &sim}
}

// rulesSimulate replays the recorded requests against a whole draft rule
// set (TOML or JSON text, as a rule file holds).
func (s *Server) rulesSimulate(c *conn, req gate.Request) gate.Reply {
	if r := s.noAgents(c, "rules.simulate"); r != nil {
		return *r
	}
	now := s.now()
	var text string
	if err := json.Unmarshal(req.Rules, &text); err != nil {
		return gate.Reply{Error: "rules: the text of a rule file"}
	}
	rules, err := policy.ParseRules(text)
	if err != nil {
		return gate.Reply{Error: err.Error()}
	}
	scope := req.Scope
	if scope == "" {
		scope = policy.ScopeSystem
	}
	for i := range rules {
		if err := policy.Validate(&rules[i], s.o.Reg, scope, now); err != nil {
			return gate.Reply{Error: fmt.Sprintf("rule %s: %v", rules[i].ID, err)}
		}
		if scope == policy.ScopeUser {
			rules[i].UID = c.p.UID
		}
	}
	since, err := sinceOf(req.Since, now)
	if err != nil {
		return gate.Reply{Error: err.Error()}
	}
	recs, err := s.history(c, req, since)
	if err != nil {
		return gate.Reply{Error: err.Error()}
	}
	sim := simulate.Run(s.eng, rules, simulate.Requests(recs), since)
	return gate.Reply{OK: true, Simulation: &sim}
}

// checkRuleChange validates the content of a gate.rule.change call at
// request time, so a broken rule never reaches a person's queue.
func (s *Server) checkRuleChange(args map[string]any, uid int, now time.Time) error {
	op, _ := args["op"].(string)
	scope, _ := args["scope"].(string)
	switch op {
	case "add":
		raw, err := json.Marshal(args["rule"])
		if err != nil || args["rule"] == nil {
			return refuse("gate.rule.change add needs a rule")
		}
		r, err := s.parseRule(raw, scope, uid, now)
		if err != nil {
			return refuse("%v", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, x := range s.eng.Rules() {
			if x.Scope == r.Scope && x.UID == r.UID && x.ID == r.ID {
				return refuse("a %s rule %s already exists", scope, r.ID)
			}
		}
	case "remove":
		id, _ := args["id"].(string)
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.findRule(scope, uid, id); !ok {
			return refuse("no %s rule %s", scope, id)
		}
	}
	return nil
}

func (s *Server) findRule(scope string, uid int, id string) (policy.Rule, bool) {
	for _, r := range s.eng.Rules() {
		if r.ID == id && r.Scope == scope && (scope == policy.ScopeSystem || r.UID == uid) {
			return r, true
		}
	}
	return policy.Rule{}, false
}

// runInternalLocked carries out an approved gate.rule.change (the gate
// is its executor), recording the claim and the result.
func (s *Server) runInternalLocked(e *entry, by string, now time.Time) {
	s.claimLocked(e, "basalt-gate", "gate", now)
	ok, text := true, ""
	if err := s.applyChangeLocked(e.P.Calls[0].Args, e.P.Requester.UID, by, now); err != nil {
		ok, text = false, err.Error()
	} else {
		text = "the rule set was updated"
	}
	e.Result = text
	out := "ok"
	if !ok {
		out = "error"
	}
	s.record(ledger.Record{UID: e.P.Requester.UID, Event: "gate.result", Outcome: out, Data: map[string]any{
		"id": e.P.ID, "executor": "basalt-gate", "result": text, "ok": ok}})
}

func (s *Server) applyChangeLocked(args map[string]any, uid int, by string, now time.Time) error {
	op, _ := args["op"].(string)
	scope, _ := args["scope"].(string)
	rules := s.eng.Rules()
	var event string
	var changed policy.Rule
	switch op {
	case "add":
		raw, _ := json.Marshal(args["rule"])
		r, err := s.parseRule(raw, scope, uid, now)
		if err != nil {
			return err
		}
		r.Created = &policy.Created{By: "person", UID: uid, At: now.UTC().Format(time.RFC3339), Via: by}
		if err := policy.Validate(&r, s.o.Reg, scope, now); err != nil {
			return err
		}
		if scope == policy.ScopeUser {
			r.UID = uid
		}
		for _, x := range rules {
			if x.Key() == r.Key() {
				return fmt.Errorf("rule %s already exists", r.ID)
			}
		}
		rules, event, changed = append(rules, r), "gate.rule.add", r
	case "remove":
		id, _ := args["id"].(string)
		r, ok := s.findRule(scope, uid, id)
		if !ok {
			return fmt.Errorf("no %s rule %s", scope, id)
		}
		kept := rules[:0]
		for _, x := range rules {
			if x.Key() != r.Key() {
				kept = append(kept, x)
			}
		}
		rules, event, changed = kept, "gate.rule.remove", r
		delete(s.state.Paused, r.Key())
		s.saveStateLocked()
	default:
		return fmt.Errorf("op %q (add or remove)", op)
	}
	if err := s.o.Store.SaveRules(rules, s.preset); err != nil {
		return err
	}
	s.eng.SetRules(rules)
	s.record(ledger.Record{UID: uid, Event: event, Data: map[string]any{"rule": changed.ID, "ref": changed.Ref(),
		"scope": changed.Scope, "sentence": changed.Sentence(), "by": by, "effect": changed.Effect}})
	s.publishLocked(gate.Event{Type: "rules", Note: op + " " + changed.Key()}, nil)
	return nil
}

// tightens reports a change that only narrows what runs without asking:
// adding an ask or refuse rule, removing an allow rule.
func (s *Server) tightens(args map[string]any, uid int) bool {
	op, _ := args["op"].(string)
	scope, _ := args["scope"].(string)
	switch op {
	case "add":
		m, _ := args["rule"].(map[string]any)
		eff, _ := m["effect"].(string)
		return eff == policy.Ask || eff == policy.Refuse
	case "remove":
		id, _ := args["id"].(string)
		r, ok := s.findRule(scope, uid, id)
		return ok && (policy.Allows(r.Effect) || r.Effect == policy.AskRemember)
	}
	return false
}

// rulesApply makes a tightening change at once, from a trusted surface:
// "always ask", "never allow" and revoking an allow rule take effect
// immediately. A system-wide change needs administrator authentication.
func (s *Server) rulesApply(ctx context.Context, c *conn, req gate.Request) gate.Reply {
	if r := s.noAgents(c, "rules.apply"); r != nil {
		return *r
	}
	var args map[string]any
	if err := json.Unmarshal(req.Rule, &args); err != nil {
		return gate.Reply{Error: "rules.apply: rule holds {op, scope, rule | id}"}
	}
	scope, _ := args["scope"].(string)
	if scope == "" {
		scope = policy.ScopeUser
		args["scope"] = scope
	}
	// Tightening is instant from any surface (ADR 0020): a user's own
	// programs (not agents) may tighten that user's own rules too, e.g. the
	// shell removing the rule of a grant the person revoked. Anything else
	// needs a trusted surface.
	if !c.roles.Decider && (scope != policy.ScopeUser || c.p.UID == 0) {
		return gate.Reply{Error: "refused: only a trusted surface changes rules directly; propose gate.rule.change instead"}
	}
	now := s.now()
	if err := s.checkRuleChange(args, c.p.UID, now); err != nil {
		return gate.Reply{Error: err.Error()}
	}
	s.mu.Lock()
	tight := s.tightens(args, c.p.UID)
	s.mu.Unlock()
	if !tight {
		return gate.Reply{Error: "refused: this change loosens what runs without asking; propose gate.rule.change and approve it"}
	}
	action := ""
	switch {
	case scope == policy.ScopeSystem && c.p.UID != 0:
		action = polkit.DecideAdmin
	case c.roles.Polkit:
		action = polkit.Decide
	}
	if action != "" {
		if s.o.Polkit == nil {
			return gate.Reply{Error: "refused: no polkit checker"}
		}
		if err := s.o.Polkit.Check(ctx, c.p.PID, c.p.UID, action); err != nil {
			return gate.Reply{Error: "refused: " + err.Error()}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyChangeLocked(args, c.p.UID, "person:"+surface(c), now); err != nil {
		return gate.Reply{Error: err.Error()}
	}
	return gate.Reply{OK: true}
}

// rulesUnpause lets a rule allow again after its limit tripped (a person
// looked).
func (s *Server) rulesUnpause(ctx context.Context, c *conn, req gate.Request) gate.Reply {
	if r := s.noAgents(c, "rules.unpause"); r != nil {
		return *r
	}
	if !c.roles.Decider {
		return gate.Reply{Error: "refused: only a trusted surface resumes a rule"}
	}
	s.mu.Lock()
	var target *policy.Rule
	for _, r := range s.visibleRules(c.p.UID) {
		if r.Key() == req.ID || (r.ID == req.ID && r.Scope == policy.ScopeUser && r.UID == c.p.UID) {
			rc := r
			target = &rc
		}
	}
	_, paused := s.state.Paused[req.ID]
	if target != nil {
		_, paused = s.state.Paused[target.Key()]
	}
	s.mu.Unlock()
	if target == nil || !paused {
		return gate.Reply{Error: "no paused rule " + req.ID}
	}
	action := ""
	switch {
	case target.Scope == policy.ScopeSystem && c.p.UID != 0:
		action = polkit.DecideAdmin
	case c.roles.Polkit:
		action = polkit.Decide
	}
	if action != "" {
		if s.o.Polkit == nil {
			return gate.Reply{Error: "refused: no polkit checker"}
		}
		if err := s.o.Polkit.Check(ctx, c.p.PID, c.p.UID, action); err != nil {
			return gate.Reply{Error: "refused: " + err.Error()}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.state.Paused, target.Key())
	s.saveStateLocked()
	s.record(ledger.Record{UID: c.p.UID, Event: "gate.rule.change", Data: map[string]any{"rule": target.ID,
		"scope": target.Scope, "sentence": target.Sentence() + " (allows again after its limit)", "by": "person:" + surface(c)}})
	return gate.Reply{OK: true}
}
