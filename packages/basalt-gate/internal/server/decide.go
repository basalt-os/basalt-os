package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/polkit"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/store"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

type storeUseT = store.Use

// canSee: the requester sees its own requests; a decider sees the
// requests of its own user and the system's (root sees all).
func (s *Server) canSee(c *conn, e *entry) bool {
	if e.Owner == c.owner() || s.isExecutorOf(c, e) {
		return true
	}
	return c.roles.Decider && (c.p.UID == 0 || e.P.Requester.UID == c.p.UID || e.P.Requester.UID == 0)
}

// view renders an entry for a client; the short code only for deciders.
func (s *Server) view(c *conn, e *entry) gate.View {
	p := e.P
	v := gate.View{ID: p.ID, Group: p.Group, Ref: p.Ref, Decision: e.Status, Class: p.Class, ClassName: proposal.ClassNames[p.Class],
		Actions: p.Actions(), Who: p.Requester.Who(), UID: p.Requester.UID, Taint: p.Requester.Taint,
		Origin: p.Requester.Origin, Leaves: p.Leaves, Created: p.Created.Format(time.RFC3339),
		Expires: p.Expires.Format(time.RFC3339), Deferrable: p.Deferrable, By: e.DecidedBy, Reason: e.Decision.Reason,
		NeedsAdmin: e.NeedsAdmin, Digest: p.Digest, Executor: e.Executor, Claimed: e.Claimed, Result: e.Result,
		Requester: gate.Party{Kind: p.Requester.Kind, Name: p.Requester.Name, Session: p.Requester.Session}}
	if e.Status == gate.Asked {
		v.By = e.Decision.By
	}
	for _, x := range p.Requester.Via {
		v.Via = append(v.Via, gate.Party{Kind: x.Kind, Name: x.Name, Session: x.Session})
	}
	if p.Reversible != nil {
		v.Reversible = p.Reversible.How
	}
	v.Preview = gate.Preview{TitleKey: p.Preview.TitleKey, TitleArgs: p.Preview.TitleArgs, Diff: p.Preview.Diff,
		Commands: p.Preview.Commands}
	for _, l := range p.Preview.Lines {
		v.Preview.Lines = append(v.Preview.Lines, gate.Line{Key: l.Key, Args: l.Args})
	}
	v.Resources = []gate.Resource{}
	for _, r := range p.Resources {
		v.Resources = append(v.Resources, gate.Resource{Kind: r.Kind, Value: r.Value, Beneath: r.Beneath, Bytes: r.Bytes})
	}
	for _, call := range p.Calls {
		v.Calls = append(v.Calls, gate.Call{Action: call.Action, Args: call.Args})
	}
	if e.Status == gate.Asked && e.Decision.Effect == policy.AskRemember {
		v.Remember = e.Decision.Remember
	}
	if c.roles.Decider {
		v.Code = p.Code
	}
	return v
}

func (s *Server) status(c *conn, id string) gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil || !s.canSee(c, e) {
		return gate.Reply{Error: "no such request"}
	}
	v := s.view(c, e)
	return gate.Reply{OK: true, ID: id, Decision: e.Status, Class: e.P.Class, Reason: e.Decision.Reason, By: v.By, Request: &v}
}

func (s *Server) wait(c *conn, req gate.Request) gate.Reply {
	s.mu.Lock()
	e := s.entries[req.ID]
	if e == nil || !s.canSee(c, e) {
		s.mu.Unlock()
		return gate.Reply{Error: "no such request"}
	}
	done := e.done
	s.mu.Unlock()
	to := req.Timeout
	if to <= 0 {
		to = 180
	}
	if to > 600 {
		to = 600
	}
	timedOut := false
	select {
	case <-done:
	case <-time.After(time.Duration(to) * time.Second):
		timedOut = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Status == gate.Allowed && s.requesterRuns(e) && !e.Claimed && e.Owner == c.owner() {
		s.claimLocked(e, "requester", c.owner(), s.now())
		s.saveQueueLocked()
	}
	reason := e.Decision.Reason
	switch e.Status {
	case gate.Declined:
		reason = "a person declined it"
	case gate.Expired:
		reason = "nobody decided in time"
	}
	return gate.Reply{OK: true, ID: e.P.ID, Decision: e.Status, Class: e.P.Class, Reason: reason, By: e.DecidedBy, TimedOut: timedOut}
}

func (s *Server) cancel(c *conn, id string) gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil || e.Owner != c.owner() {
		return gate.Reply{Error: "no such request of yours"}
	}
	if e.final() {
		return gate.Reply{Error: "the request is already " + e.Status}
	}
	e.finish(gate.Cancelled, "requester", s.now())
	s.recordDecision(e, "cancelled", "requester", "", "withdrawn")
	s.publishLocked(gate.Event{Type: "decision", ID: id, Note: gate.Cancelled}, e)
	s.saveQueueLocked()
	return gate.Reply{OK: true, ID: id, Decision: gate.Cancelled}
}

func (s *Server) list(c *conn, pending bool, limit int) gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []gate.View
	for _, id := range s.order {
		e := s.entries[id]
		if e == nil || !s.canSee(c, e) || (pending != (e.Status == gate.Asked)) {
			continue
		}
		out = append(out, s.view(c, e))
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	if out == nil {
		out = []gate.View{}
	}
	return gate.Reply{OK: true, Requests: out}
}

// surface names a decider for records ("person:<surface>").
func surface(c *conn) string {
	switch c.p.Type() {
	case "basalt_shell_ui_t":
		return "shell"
	case "basalt_app_approvals_t":
		return "approvals-app"
	case "basalt_gate_tty_t":
		if c.p.UID == 0 {
			return "tty-root"
		}
		return "tty"
	}
	return c.p.Type()
}

// isUserRuleChange: a person changing their own user rules.
func isUserRuleChange(e *entry, uid int) bool {
	if len(e.P.Calls) != 1 || e.P.Calls[0].Action != "gate.rule.change" {
		return false
	}
	sc, _ := e.P.Calls[0].Args["scope"].(string)
	return sc == policy.ScopeUser && e.P.Requester.UID == uid
}

// polkitFor returns the polkit action a decider's approval needs ("" for
// none): administrator authentication for system and critical changes
// and for another user's requests; a terminal decider authenticates every
// time.
func polkitFor(c *conn, es []*entry) string {
	admin := false
	for _, e := range es {
		if e.NeedsAdmin && !isUserRuleChange(e, c.p.UID) {
			admin = true
		}
		if e.P.Requester.UID != c.p.UID && c.p.UID != 0 {
			admin = true
		}
	}
	switch {
	case admin:
		return polkit.DecideAdmin
	case c.roles.Polkit:
		return polkit.Decide
	}
	return ""
}

func (s *Server) decide(ctx context.Context, c *conn, req gate.Request) gate.Reply {
	if !c.roles.Decider {
		ids := strings.TrimSpace(strings.Join(append(append([]string{}, req.IDs...), req.ID), " "))
		actions := []string{}
		s.mu.Lock()
		if e := s.entries[req.ID]; e != nil {
			actions = e.P.Actions()
		}
		s.mu.Unlock()
		if ids == "" {
			ids = "(none)"
		}
		s.refuseOnce(c.owner()+"|decide", ledger.Record{UID: c.p.UID, Session: c.roles.Session, Event: "gate.decision",
			Outcome: "denied", Data: map[string]any{"id": ids, "actions": actions,
				"verdict": "a decision from " + s.peerWho(c) + " was refused: only a trusted surface decides",
				"context": c.p.Context}})
		return gate.Reply{Error: "refused: only the shell, the Approvals app or the terminal approval tool decide"}
	}
	if req.Approve == nil {
		return gate.Reply{Error: "decide needs approve: true or false"}
	}
	approve := *req.Approve
	ids := append([]string{}, req.IDs...)
	if req.ID != "" {
		ids = append(ids, req.ID)
	}
	s.mu.Lock()
	if req.Group != "" {
		for _, id := range s.order {
			e := s.entries[id]
			if e != nil && e.Status == gate.Asked && e.P.Group == req.Group && s.canSee(c, e) {
				// A group decision never covers Critical or Cannot be undone.
				if approve && proposal.ClassRank(e.P.Class) >= 4 {
					continue
				}
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		s.mu.Unlock()
		return gate.Reply{Error: "nothing to decide"}
	}
	var es []*entry
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		e := s.entries[id]
		if e == nil || !s.canSee(c, e) {
			s.mu.Unlock()
			return gate.Reply{Error: "no such request: " + id}
		}
		if e.Status != gate.Asked {
			s.mu.Unlock()
			return gate.Reply{Error: fmt.Sprintf("request %s is already %s", id, e.Status)}
		}
		es = append(es, e)
	}
	if approve && len(es) > 1 {
		tainted, chains := false, map[string]bool{}
		for _, e := range es {
			if proposal.ClassRank(e.P.Class) >= 4 {
				s.mu.Unlock()
				return gate.Reply{Error: "Critical and Cannot be undone requests are approved one by one"}
			}
			tainted = tainted || e.P.Requester.Taint != "none"
			chains[policy.ChainKey(e.P.Requester)] = true
		}
		if tainted && len(chains) > 1 {
			s.mu.Unlock()
			return gate.Reply{Error: "requests from different requesters, one of them after untrusted content, are approved one by one"}
		}
	}
	var src *policy.Rule
	if approve && req.Remember {
		if len(es) != 1 || es[0].Decision.Effect != policy.AskRemember {
			s.mu.Unlock()
			return gate.Reply{Error: "remember is offered only where a rule asks once and then remembers"}
		}
		for _, r := range s.eng.Rules() {
			if r.Key() == es[0].Decision.RuleKey {
				rc := r
				src = &rc
			}
		}
		if src == nil {
			s.mu.Unlock()
			return gate.Reply{Error: "the rule that offered remember is gone"}
		}
	}
	action := ""
	if approve {
		action = polkitFor(c, es)
	}
	s.mu.Unlock()

	// Authentication happens outside the lock (the person types a password).
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
	now := s.now()
	by := "person:" + surface(c)
	out := map[string]string{}
	for _, e := range es {
		if e.Status != gate.Asked {
			out[e.P.ID] = e.Status
			continue
		}
		if !approve {
			e.finish(gate.Declined, by, now)
			s.recordDecision(e, "declined", by, "", "")
		} else {
			e.finish(gate.Allowed, by, now)
			s.recordDecision(e, "approved", by, "", "")
			if s.o.Reg.Executors[e.Executor].Internal {
				s.runInternalLocked(e, by, now)
			}
		}
		out[e.P.ID] = e.Status
		s.publishLocked(gate.Event{Type: "decision", ID: e.P.ID, Note: e.Status}, e)
	}
	if src != nil && es[0].Status == gate.Allowed {
		if err := s.rememberLocked(es[0], *src, c, now); err != nil {
			s.o.Logf("remember: %v", err)
		}
	}
	s.saveQueueLocked()
	var starts []unitStart
	for _, e := range es {
		starts = append(starts, s.unitFor(e)...)
	}
	defer s.startUnits(starts)
	rep := gate.Reply{OK: true, Decided: out}
	if len(es) == 1 {
		rep.ID, rep.Decision = es[0].P.ID, es[0].Status
	}
	return rep
}

// rememberLocked adds the narrow temporary rule of "approve and remember".
func (s *Server) rememberLocked(e *entry, src policy.Rule, c *conn, now time.Time) error {
	r, err := policy.NarrowRule(e.P, e.Facts, src, "r-tmp-"+strings.TrimPrefix(e.P.ID, "g-"), now)
	if err != nil {
		return err
	}
	r.Created.Via = surface(c)
	if err := policy.Validate(&r, s.o.Reg, src.Scope, now); err != nil {
		return err
	}
	rules := append(s.eng.Rules(), r)
	if err := s.o.Store.SaveRules(rules, s.preset); err != nil {
		return err
	}
	s.eng.SetRules(rules)
	s.record(ledger.Record{UID: e.P.Requester.UID, Event: "gate.rule.add", Data: map[string]any{"rule": r.ID,
		"ref": r.Ref(), "scope": r.Scope, "sentence": r.Sentence(), "by": "person:" + surface(c), "temporary": true}})
	s.publishLocked(gate.Event{Type: "rules", Note: "added " + r.Key()}, nil)
	return nil
}

func (s *Server) claimLocked(e *entry, executor, owner string, now time.Time) {
	e.Claimed, e.ClaimedBy, e.ClaimedAt, e.ClaimOwner = true, executor, now, owner
	s.record(ledger.Record{UID: e.P.Requester.UID, Event: "gate.claim", Data: map[string]any{"id": e.P.ID,
		"executor": executor, "digest_match": true, "digest": e.P.Digest}})
}

// claim hands an allowed request to its executor, once, if what the
// executor is about to run has the digest that was approved.
func (s *Server) claim(c *conn, req gate.Request) gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.entries[req.ID]
	deny := func(name, reason string) gate.Reply {
		data := map[string]any{"id": req.ID, "executor": name, "reason": reason, "context": c.p.Context, "peer_uid": c.p.UID}
		uid := c.p.UID
		if e != nil {
			uid = e.P.Requester.UID
		}
		s.refuseOnceLocked(c.owner()+"|claim|"+req.ID+"|"+reason, ledger.Record{UID: uid, Event: "gate.claim", Outcome: "denied", Data: data})
		return gate.Reply{Error: "refused: " + reason}
	}
	if e == nil {
		return deny(orNone(c.p.Type()), "no such request")
	}
	ex := s.o.Reg.Executors[e.Executor]
	name := e.Executor
	switch {
	case ex.Internal:
		return deny(orNone(c.p.Type()), "the gate itself carries out this request")
	case ex.Requester:
		if e.Owner != c.owner() {
			return deny(orNone(c.p.Type()), "only the requester runs this request")
		}
	default:
		if c.roles.Agent {
			return deny(orNone(c.p.Type()), "an agent cannot claim a request for an executor")
		}
		ok := false
		for _, t := range ex.Types {
			ok = ok || (t != "" && t == c.p.Type())
		}
		if !ok {
			return deny(orNone(c.p.Type()), "this program is not the executor of "+strings.Join(e.P.Actions(), ", "))
		}
		if ex.UID != nil && *ex.UID != c.p.UID {
			return deny(name, fmt.Sprintf("the executor runs as uid %d", *ex.UID))
		}
	}
	if req.Executor != "" && req.Executor != e.Executor {
		return deny(name, "the request is for the executor "+e.Executor)
	}
	switch {
	case e.Status != gate.Allowed:
		return deny(name, "the request is "+e.Status+", not approved")
	case e.Claimed:
		return deny(name, "already claimed (a decision is used once)")
	case now.Sub(e.DecidedAt) > s.cfg.ClaimWindow:
		return deny(name, "the decision is too old; ask again")
	}
	digest := req.Digest
	if digest == "" && len(req.Calls) > 0 {
		d, err := s.digestFromCalls(e, req.Calls, req.Preview)
		if err != nil {
			d = "invalid: " + err.Error()
		}
		digest = d
	}
	if digest != e.P.Digest {
		// What would run is not what was approved: the request is void and
		// must be planned and decided again.
		e.Status, e.DecidedBy = gate.Refused, "stale"
		e.Decision.Reason = "what the executor would run differs from what was approved"
		s.recordDecision(e, "refused", "stale", "", e.Decision.Reason)
		s.saveQueueLocked()
		return deny(name, "digest mismatch: what would run differs from what was approved")
	}
	s.claimLocked(e, name, c.owner(), now)
	s.saveQueueLocked()
	v := s.view(c, e)
	return gate.Reply{OK: true, ID: e.P.ID, Decision: e.Status, Digest: e.P.Digest, Request: &v}
}

func (s *Server) result(c *conn, req gate.Request) gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[req.ID]
	if e == nil || !e.Claimed || e.ClaimOwner != c.owner() {
		return gate.Reply{Error: "no claimed request of yours with this id"}
	}
	if e.Result != "" {
		return gate.Reply{Error: "the result is already recorded"}
	}
	ok := req.OK == nil || *req.OK
	text := "done"
	if req.Exit != nil {
		text = fmt.Sprintf("exit code %d", *req.Exit)
		ok = ok && *req.Exit == 0
	}
	if req.Detail != "" {
		text += ": " + clip(req.Detail, 500)
	}
	if !ok && req.Exit == nil {
		text = "failed: " + clip(req.Detail, 500)
	}
	e.Result = text
	out := "ok"
	if !ok {
		out = "error"
	}
	data := map[string]any{"id": e.P.ID, "executor": e.ClaimedBy, "result": text, "ok": ok}
	if len(req.Snapshots) > 0 {
		data["snapshots"] = req.Snapshots
	}
	s.record(ledger.Record{UID: e.P.Requester.UID, Event: "gate.result", Outcome: out, Data: data})
	s.saveQueueLocked()
	return gate.Reply{OK: true, ID: e.P.ID}
}

// stop is the emergency stop: anyone may pull it, without authentication.
func (s *Server) stop(c *conn, req gate.Request) gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	who := s.peerWho(c)
	again := s.state.Stop.On
	if !again {
		s.state.Stop = store.Stop{On: true, By: who, From: orNone(c.p.Type()), At: now.UTC().Format(time.RFC3339)}
		s.saveStateLocked()
	}
	s.record(ledger.Record{UID: c.p.UID, Session: c.roles.Session, Event: "gate.stop", Data: map[string]any{"who": who,
		"from": c.p.Context, "reason": clip(req.Reason, 200), "already_stopped": again}})
	s.publishLocked(gate.Event{Type: "stop", Note: who}, nil)
	return gate.Reply{OK: true}
}

// resume is a loosening: deciders only, with polkit from a terminal or
// when the stop asked for it.
func (s *Server) resume(ctx context.Context, c *conn) gate.Reply {
	who := s.peerWho(c)
	if !c.roles.Decider {
		s.refuseOnce(c.owner()+"|resume", ledger.Record{UID: c.p.UID, Session: c.roles.Session, Event: "gate.resume",
			Outcome: "denied", Data: map[string]any{"who": who, "reason": "only a trusted surface resumes automation"}})
		return gate.Reply{Error: "refused: only the shell, the Approvals app or the terminal approval tool resume automation"}
	}
	s.mu.Lock()
	st := s.state.Stop
	s.mu.Unlock()
	if !st.On {
		return gate.Reply{OK: true, Reason: "automation was not stopped"}
	}
	action := ""
	switch {
	case st.NeedsAuth:
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
	s.state.Stop = store.Stop{}
	s.saveStateLocked()
	s.record(ledger.Record{UID: c.p.UID, Event: "gate.resume", Data: map[string]any{"who": who, "stopped_by": st.By,
		"stopped_at": st.At}})
	s.publishLocked(gate.Event{Type: "resume", Note: who}, nil)
	return gate.Reply{OK: true}
}

// unlock: a locked action can be unlocked once by an administrator at the
// local console with a second factor (a paired phone or a security key).
// Neither exists yet, so locked actions stay locked.
func (s *Server) unlock(c *conn, req gate.Request) gate.Reply {
	reason := "unlocking needs a second factor (a paired phone or a security key), which this version does not have; locked actions stay locked"
	s.refuseOnce(c.owner()+"|unlock", ledger.Record{UID: c.p.UID, Event: "gate.unlock", Outcome: "denied",
		Data: map[string]any{"id": clip(req.ID, 20), "who": s.peerWho(c), "reason": reason}})
	return gate.Reply{Error: "refused: " + reason}
}
