package server

// ADR 0020 phase 2: what the approval paths that move to the gate need
// beyond phase 1.
//
//   - observe: shadow mode. A component that keeps its own confirmation
//     for now tells the gate what it decided; the gate records the request
//     and what it would have decided (the data dry runs need), and queues
//     nothing.
//   - confirm: root at a terminal confirms a system assistant proposal
//     (`basalt apply ID`, typed yes, or `--yes --confirm CODE` with the
//     short code), recorded as the person's decision (person:tty-root).
//   - executor units: once a request whose executor names a unit is
//     approved, the gate starts that unit (basalt-gate-exec@ID.service),
//     which claims the decision and runs it.
//   - claims with calls: the executor sends what it is about to run and
//     the gate computes the digest the same way it did for the request.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// observeOutcomes are the outcomes a component may report.
var observeOutcomes = map[string]bool{"approved": true, "declined": true, "expired": true, "refused": true,
	"failed": true, "cancelled": true}

// observe records a decision an approval path made by itself (shadow
// mode) with what the gate would have decided. Agents may not report
// observations: they never decide anything.
func (s *Server) observe(c *conn, req gate.Request) gate.Reply {
	if c.roles.Agent {
		s.refuseOnce(c.owner()+"|observe", ledger.Record{UID: c.p.UID, Session: c.roles.Session, Event: "gate.request",
			Outcome: "denied", Data: map[string]any{"who": s.peerWho(c), "actions": actionIDs(req.Calls),
				"reason": "an agent cannot report decisions", "observed": true}})
		return gate.Reply{Error: "refused: an agent cannot report decisions"}
	}
	if !observeOutcomes[req.Outcome] {
		return gate.Reply{Error: "observe needs outcome: approved, declined, expired, refused, failed or cancelled"}
	}
	if !s.allowRate(c.owner()) {
		return gate.Reply{OK: true, Decision: gate.Refused, Reason: "too many requests; try again in a minute"}
	}
	p, facts, executor, err := s.build(c, req)
	if err != nil {
		var be buildError
		if !errors.As(err, &be) {
			return gate.Reply{Error: err.Error()}
		}
		s.refuseOnce(c.owner()+"|observe|"+err.Error(), ledger.Record{UID: c.p.UID, Event: "gate.request", Outcome: "denied",
			Data: map[string]any{"who": s.peerWho(c), "actions": actionIDs(req.Calls), "reason": err.Error(), "observed": true,
				"was": req.Outcome}})
		return gate.Reply{OK: true, Decision: gate.Refused, Reason: err.Error(), By: "registry"}
	}
	now := s.now()
	env := s.o.Env(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.eng.Decide(policyInput(p, facts, env, s.state.Stop.On), lockedCounter{s})
	p.ID = proposal.NewID()
	p.Created = now.UTC()
	p.Expires = p.Created
	e := &entry{P: p, Facts: facts, Status: req.Outcome, Decision: d, Executor: executor, Owner: c.owner()}
	s.recordRequestObserved(e, env)
	by := "observed:" + clip(observeBy(req.DecidedBy), 60)
	data := map[string]any{"id": p.ID, "actions": p.Actions(), "outcome": req.Outcome, "by": by, "class": p.Class,
		"digest": p.Digest, "observed": true, "would": d.Outcome, "would_by": d.By,
		"verdict": fmt.Sprintf("%s by %s outside the gate (shadow mode); the gate would have %s it (%s)",
			req.Outcome, observeBy(req.DecidedBy), wouldWord(d.Outcome), d.Reason)}
	lo := "ok"
	switch req.Outcome {
	case "approved":
		lo = "allowed"
	case "declined", "expired", "refused":
		lo = "denied"
	}
	s.record(ledger.Record{UID: p.Requester.UID, Session: p.Requester.Session, Event: "gate.decision", Outcome: lo,
		Data: data, Subject: ledger.Subject{Profile: agentProfile(p.Requester)}})
	return gate.Reply{OK: true, ID: p.ID, Decision: d.Outcome, Class: p.Class, Reason: d.Reason, By: d.By}
}

func observeBy(by string) string {
	if by == "" {
		return "its own confirmation"
	}
	return by
}

func wouldWord(outcome string) string {
	switch outcome {
	case gate.Allowed:
		return "allowed"
	case gate.Refused:
		return "refused"
	}
	return "asked a person about"
}

// recordRequestObserved is recordRequest for an observation.
func (s *Server) recordRequestObserved(e *entry, env policyEnv) {
	s.recordRequestData(e, env, map[string]any{"observed": true, "was": e.Status})
}

// confirm: root at a terminal confirms one of the system assistant's
// proposals with its short code (compatibility with `basalt apply ID
// --yes --confirm CODE`, and the typed yes of an interactive apply).
func (s *Server) confirm(c *conn, req gate.Request) gate.Reply {
	deny := func(reason string) gate.Reply {
		actions := []string{}
		s.mu.Lock()
		if e := s.entries[req.ID]; e != nil {
			actions = e.P.Actions()
		}
		s.mu.Unlock()
		s.refuseOnce(c.owner()+"|confirm|"+req.ID+"|"+reason, ledger.Record{UID: c.p.UID, Session: c.roles.Session,
			Event: "gate.decision", Outcome: "denied", Data: map[string]any{"id": clip(req.ID, 20), "actions": actions,
				"verdict": "a confirmation by " + s.peerWho(c) + " was refused: " + reason, "context": c.p.Context}})
		return gate.Reply{Error: "refused: " + reason}
	}
	if c.p.UID != 0 || c.roles.Agent {
		return deny("only root at a terminal confirms with a code")
	}
	mode := req.Mode
	if mode == "" {
		mode = "code"
	}
	if mode != "code" && mode != "terminal" {
		return gate.Reply{Error: "confirm mode: code or terminal"}
	}
	if mode == "code" && !s.cfg.AllowCodeConfirm {
		return deny("confirmation codes are turned off (allow_code_confirm = no); approve it in the queue")
	}
	s.mu.Lock()
	e := s.entries[req.ID]
	if e == nil {
		s.mu.Unlock()
		return deny("no such request")
	}
	if e.P.Ref == "" {
		s.mu.Unlock()
		return deny("a code confirms only the system assistant's proposals")
	}
	switch e.Status {
	case gate.Allowed:
		s.mu.Unlock()
		return gate.Reply{OK: true, ID: e.P.ID, Decision: gate.Allowed, By: e.DecidedBy}
	case gate.Asked:
	default:
		st := e.Status
		s.mu.Unlock()
		return deny("the request is " + st)
	}
	if req.Code != e.P.Code {
		s.mu.Unlock()
		return deny("the code does not match the commands of this request")
	}
	now := s.now()
	by := "person:tty-root"
	e.finish(gate.Allowed, by, now)
	s.recordDecision(e, "approved", by, "", "")
	s.publishLocked(gate.Event{Type: "decision", ID: e.P.ID, Note: gate.Allowed}, e)
	s.saveQueueLocked()
	start := s.unitFor(e)
	s.mu.Unlock()
	s.startUnits(start)
	return gate.Reply{OK: true, ID: e.P.ID, Decision: gate.Allowed, By: by}
}

// unitFor returns the executor unit to start for an approved entry
// (locked), or "" when its executor has none.
func (s *Server) unitFor(e *entry) []unitStart {
	if !s.cfg.ExecUnits || e.Status != gate.Allowed || e.Claimed {
		return nil
	}
	ex, ok := s.o.Reg.Executors[e.Executor]
	if !ok || ex.Unit == "" {
		return nil
	}
	return []unitStart{{id: e.P.ID, uid: e.P.Requester.UID, unit: strings.Replace(ex.Unit, "@.", "@"+e.P.ID+".", 1)}}
}

type unitStart struct {
	id, unit string
	uid      int
}

// startUnits starts executor units outside the lock; a failure is
// recorded as the request's result.
func (s *Server) startUnits(us []unitStart) {
	if len(us) == 0 {
		return
	}
	start := s.o.StartUnit
	if start == nil {
		start = s.systemctlStart
	}
	go func() {
		for _, u := range us {
			if err := start(u.unit); err != nil {
				s.o.Logf("starting %s: %v", u.unit, err)
				s.mu.Lock()
				if e := s.entries[u.id]; e != nil && e.Result == "" {
					e.Result = "failed: the executor could not be started: " + clip(err.Error(), 300)
					s.saveQueueLocked()
				}
				s.mu.Unlock()
				s.record(ledger.Record{UID: u.uid, Event: "gate.result", Outcome: "error", Data: map[string]any{"id": u.id,
					"executor": u.unit, "result": "the executor could not be started: " + clip(err.Error(), 300), "ok": false}})
			}
		}
	}()
}

// execUnitRe is the only kind of unit the gate starts.
var execUnitRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,60}@g-[0-9a-f]{12}\.service$`)

// systemctlArgv is the command that starts an executor unit: never
// interactive. basalt-gated runs as root without capabilities, so when
// systemctl cannot use systemd's private socket and goes over D-Bus,
// systemd asks polkit; --no-ask-password keeps that from ever reaching a
// person (the person already decided in the gate), and the polkit rule
// 49-basalt-gate-exec.rules lets root start exactly these units.
func systemctlArgv(systemctl, unit string) ([]string, error) {
	if !execUnitRe.MatchString(unit) {
		return nil, fmt.Errorf("refusing to start %q: not an executor unit of a request", unit)
	}
	return []string{systemctl, "--no-ask-password", "--no-block", "start", "--", unit}, nil
}

// systemctlStart starts a unit without waiting for it.
func (s *Server) systemctlStart(unit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv, err := systemctlArgv(s.cfg.Systemctl, unit)
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isExecutorOf: the peer is the registered executor of the entry's
// action (it may see the request before claiming it).
func (s *Server) isExecutorOf(c *conn, e *entry) bool {
	if c.roles.Agent {
		return false
	}
	ex, ok := s.o.Reg.Executors[e.Executor]
	if !ok || ex.Internal || ex.Requester {
		return false
	}
	if ex.UID != nil && *ex.UID != c.p.UID {
		return false
	}
	for _, t := range ex.Types {
		if t != "" && t == c.p.Type() {
			return true
		}
	}
	return false
}

// digestFromCalls computes the digest of what an executor says it is
// about to run: its calls validated by the registry (the same
// normalization as the request), the resources extracted for the
// requester, the executor's preview (or the request's when it sends
// none) and the request's reference.
func (s *Server) digestFromCalls(e *entry, calls []gate.Call, pv *gate.Preview) (string, error) {
	var pcs []proposal.Call
	var res []proposal.Resource
	for _, call := range calls {
		a, err := s.o.Reg.Lookup(call.Action)
		if err != nil {
			return "", err
		}
		args, err := a.Validate(call.Args)
		if err != nil {
			return "", fmt.Errorf("%s: %v", a.ID, err)
		}
		r, err := a.Extract(args, e.P.Requester.UID, s.o.Home)
		if err != nil {
			return "", fmt.Errorf("%s: %v", a.ID, err)
		}
		pcs = append(pcs, proposal.Call{Action: a.ID, Args: args})
		res = append(res, r...)
	}
	preview := e.P.Preview
	if pv != nil {
		got, err := toPreview(pv)
		if err != nil {
			return "", err
		}
		preview = got
	}
	return proposal.DigestOf(pcs, preview, res, e.P.Ref)
}

// GrantRuleID is the id of the rule a person's approval of a grant
// becomes: "r-grant-" and the request id's digits (the requester can name
// it to remove it).
func GrantRuleID(requestID string) string { return "r-grant-" + strings.TrimPrefix(requestID, "g-") }

// grantRuleLocked keeps a person's approval of a grant (an action with
// grant_arg) as a narrow user rule (allow-quiet, or allow-tell above
// Undoable), with the
// grant's expiry: the same action, requester and resources. A skill grant
// is then a rule like any other: listed with the person's rules, removed
// at once from a trusted surface (rules.apply remove), and asking again
// for the same scope before it ends is allowed without a new question.
func (s *Server) grantRuleLocked(e *entry, c *conn, now time.Time) error {
	var dur time.Duration
	has := false
	for _, call := range e.P.Calls {
		a := s.o.Reg.Actions[call.Action]
		if a == nil || a.GrantArg == "" {
			return nil
		}
		has = true
		d := time.Hour
		if v, _ := call.Args[a.GrantArg].(string); v != "" {
			p, err := policy.ParseDuration(v)
			if err != nil || p <= 0 {
				return fmt.Errorf("grant duration %q", v)
			}
			d = p
		}
		if d > 7*24*time.Hour {
			d = 7 * 24 * time.Hour
		}
		if d > dur {
			dur = d
		}
	}
	if !has || e.P.Requester.Kind != proposal.KindPerson || e.P.Requester.UID == 0 {
		return nil
	}
	src := policy.Rule{ID: "r-grant", Scope: policy.ScopeUser, UID: e.P.Requester.UID, Remember: dur.String()}
	r, err := policy.NarrowRule(e.P, e.Facts, src, GrantRuleID(e.P.ID), now)
	if err != nil {
		return err
	}
	r.Effect = policy.AllowQuiet
	if proposal.ClassRank(e.P.Class) > 1 {
		r.Effect = policy.AllowTell
	}
	r.From = ""
	r.Created = &policy.Created{By: "person", UID: e.P.Requester.UID, At: now.UTC().Format(time.RFC3339), Via: surface(c) + " (grant)"}
	if err := policy.Validate(&r, s.o.Reg, policy.ScopeUser, now); err != nil {
		return err
	}
	rules := s.eng.Rules()
	kept := rules[:0:0]
	for _, x := range rules {
		if x.Key() != r.Key() {
			kept = append(kept, x)
		}
	}
	kept = append(kept, r)
	if err := s.o.Store.SaveRules(kept, s.preset); err != nil {
		return err
	}
	s.eng.SetRules(kept)
	s.record(ledger.Record{UID: e.P.Requester.UID, Event: "gate.rule.add", Data: map[string]any{"rule": r.ID, "ref": r.Ref(),
		"scope": r.Scope, "sentence": r.Sentence(), "by": "person:" + surface(c), "temporary": true, "grant": e.P.ID,
		"expires": r.Expires}})
	s.publishLocked(gate.Event{Type: "rules", Note: "added " + r.Key()}, nil)
	return nil
}
