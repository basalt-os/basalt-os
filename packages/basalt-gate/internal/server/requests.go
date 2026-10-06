package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// entry is one request in the queue (pending, or decided and kept for
// claims, waits and history).
type entry struct {
	P          *proposal.Proposal `json:"proposal"`
	Facts      []policy.CallFacts `json:"facts"`
	Status     string             `json:"status"` // gate.Asked, Allowed, Refused, Declined, Expired, Cancelled
	Decision   policy.Decision    `json:"decision"`
	DecidedBy  string             `json:"decided_by,omitempty"`
	DecidedAt  time.Time          `json:"decided_at,omitempty"`
	Owner      string             `json:"owner"`
	Executor   string             `json:"executor"`
	Claimed    bool               `json:"claimed,omitempty"`
	ClaimedBy  string             `json:"claimed_by,omitempty"`
	ClaimedAt  time.Time          `json:"claimed_at,omitempty"`
	ClaimOwner string             `json:"claim_owner,omitempty"`
	Result     string             `json:"result,omitempty"`
	NeedsAdmin bool               `json:"needs_admin,omitempty"`
	done       chan struct{}
}

func (e *entry) final() bool { return e.Status != gate.Asked }

// finish sets a final status and wakes waiters (locked).
func (e *entry) finish(status, by string, now time.Time) {
	e.Status, e.DecidedBy, e.DecidedAt = status, by, now
	select {
	case <-e.done:
	default:
		close(e.done)
	}
}

const (
	maxCalls   = 32
	maxEntries = 2000
)

// buildError is a refusal before any rule: the registry or the format.
type buildError struct{ msg string }

func (b buildError) Error() string { return b.msg }

func refuse(format string, a ...any) error { return buildError{fmt.Sprintf(format, a...)} }

// requester establishes who asks: the kernel's view of the peer, or what
// a trusted relay reports within the kinds it may report.
func (s *Server) requester(c *conn, req gate.Request) (proposal.Requester, error) {
	r := proposal.Requester{Kind: c.roles.Kind, Name: c.roles.Name, Session: c.roles.Session, UID: c.p.UID, PID: c.p.PID,
		Context: c.p.Context, Origin: "request"}
	if r.Kind == proposal.KindAgent && r.Session != "" {
		r.Name = s.agentName(r.Session)
	}
	if ob := req.OnBehalf; ob != nil {
		allowed := false
		for _, k := range c.roles.Relay {
			allowed = allowed || k == ob.Kind
		}
		if !allowed {
			return r, refuse("only a trusted relay may ask on behalf of someone else (%s may not report a %s)", orNone(c.p.Type()), ob.Kind)
		}
		if !proposal.Kinds[ob.Kind] || ob.Kind == proposal.KindSchedule {
			return r, refuse("requester kind %q", ob.Kind)
		}
		r.RelayedBy = c.p.Type()
		r.Kind, r.Name, r.Session = ob.Kind, clip(ob.Name, 80), clip(ob.Session, 40)
		if err := addVia(&r, ob.Via); err != nil {
			return r, err
		}
	}
	// Anyone may add intermediaries: it only narrows what rules match.
	if err := addVia(&r, req.Via); err != nil {
		return r, err
	}
	switch req.Origin {
	case "", "request":
	case "event":
		r.Origin = "event"
	case "schedule":
		return r, refuse("only the gate itself starts scheduled requests")
	default:
		return r, refuse("origin %q (request or event)", req.Origin)
	}
	taint := req.Taint
	if taint == "" {
		taint = "none"
	}
	if !validTaint(taint) {
		return r, refuse("taint %q (none, system, personal, web)", taint)
	}
	// Agents read untrusted content by nature: their requests, and any
	// request an agent is part of, carry at least the configured floor.
	for _, p := range r.Chain() {
		if p.Kind == proposal.KindAgent && proposal.TaintRank(taint) < proposal.TaintRank(s.cfg.AgentTaintFloor) {
			taint = s.cfg.AgentTaintFloor
		}
	}
	r.Taint = taint
	return r, nil
}

func validTaint(t string) bool {
	for _, v := range proposal.Taints {
		if v == t {
			return true
		}
	}
	return false
}

func addVia(r *proposal.Requester, via []gate.Party) error {
	for _, v := range via {
		if !proposal.Kinds[v.Kind] {
			return refuse("intermediary kind %q", v.Kind)
		}
		if len(r.Via) >= 4 {
			return refuse("at most 4 intermediaries")
		}
		r.Via = append(r.Via, proposal.Party{Kind: v.Kind, Name: clip(v.Name, 80), Session: clip(v.Session, 40)})
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Server) agentName(session string) string {
	s.mu.Lock()
	n, ok := s.names[session]
	s.mu.Unlock()
	if ok {
		return n
	}
	if s.o.AgentName != nil {
		n = s.o.AgentName(session)
	}
	if n != "" {
		s.mu.Lock()
		if len(s.names) > 1024 {
			s.names = map[string]string{}
		}
		s.names[session] = n
		s.mu.Unlock()
	}
	return n
}

// build validates and classifies a request into a sealed proposal.
func (s *Server) build(c *conn, req gate.Request) (*proposal.Proposal, []policy.CallFacts, string, error) {
	rq, err := s.requester(c, req)
	if err != nil {
		return nil, nil, "", err
	}
	if len(req.Calls) == 0 || len(req.Calls) > maxCalls {
		return nil, nil, "", refuse("a request has 1 to %d calls", maxCalls)
	}
	if req.Group != "" && !proposal.ValidGroup(req.Group) {
		return nil, nil, "", refuse("group %q (lower-case letters, digits and dashes)", req.Group)
	}
	if req.Ref != "" && (!proposal.ValidRef(req.Ref) || rq.Kind != proposal.KindSystemAssistant) {
		return nil, nil, "", refuse("a reference is for the system assistant's proposals")
	}
	if req.ClassHint != "" && proposal.ClassRank(req.ClassHint) < 0 {
		return nil, nil, "", refuse("class_hint %q (C0 to C5)", req.ClassHint)
	}
	p := &proposal.Proposal{V: proposal.Version, Group: req.Group, Ref: req.Ref, Requester: rq, Deferrable: req.Deferrable}
	var facts []policy.CallFacts
	executor := ""
	maxClass, hasHigh, hasOther := proposal.C0, false, false
	for _, call := range req.Calls {
		a, err := s.o.Reg.Lookup(call.Action)
		if err != nil {
			return nil, nil, "", refuse("%v: not in the action registry", err)
		}
		if err := s.allowedRequester(a, rq); err != nil {
			return nil, nil, "", err
		}
		args, err := a.Validate(call.Args)
		if err != nil {
			return nil, nil, "", refuse("%s: %v", a.ID, err)
		}
		if a.ID == RuleChange {
			if err := s.checkRuleChange(args, rq.UID, s.now()); err != nil {
				return nil, nil, "", err
			}
		}
		cls := s.classOf(a, args, req.ClassHint)
		res, err := a.Extract(args, rq.UID, s.o.Home)
		if err != nil {
			return nil, nil, "", refuse("%s: %v", a.ID, err)
		}
		if executor != "" && executor != a.Executor {
			return nil, nil, "", refuse("one request goes to one executor; split %s from the other calls", a.ID)
		}
		executor = a.Executor
		if proposal.ClassRank(cls) >= 4 {
			hasHigh = true
		} else {
			hasOther = true
		}
		maxClass = proposal.MaxClass(maxClass, cls)
		p.Leaves = p.Leaves || a.Leaves
		if a.Snapshot {
			p.Snapshot = "pre"
		}
		if a.Reversible != "" && len(req.Calls) == 1 {
			p.Reversible = &proposal.Reversible{How: a.Reversible}
		}
		pc := proposal.Call{Action: a.ID, Args: args}
		p.Calls = append(p.Calls, pc)
		p.Resources = append(p.Resources, res...)
		facts = append(facts, policy.CallFacts{Call: pc, Class: cls, Resources: res, System: a.System})
	}
	if hasHigh && (hasOther || len(req.Calls) > 1) {
		return nil, nil, "", refuse("Critical and Cannot be undone calls are asked one by one; send them as separate requests")
	}
	p.Class = maxClass
	pv, err := s.preview(c, rq, req, p)
	if err != nil {
		return nil, nil, "", err
	}
	p.Preview = pv
	if err := p.Seal(); err != nil {
		return nil, nil, "", refuse("the request cannot be put in canonical form: %v", err)
	}
	return p, facts, executor, nil
}

// allowedRequester applies the registry's who-may-ask rules before any
// policy: person-only actions refuse agents (and anything relayed through
// one), agent-only actions refuse everyone else.
func (s *Server) allowedRequester(a *registry.Action, rq proposal.Requester) error {
	chain := rq.Chain()
	if a.PersonOnly {
		if rq.Kind != proposal.KindPerson {
			return refuse("%s is asked for only by the person, in their own words", a.ID)
		}
		for _, p := range chain {
			if p.Kind == proposal.KindAgent {
				return refuse("%s is asked for only by the person; an agent is part of this request", a.ID)
			}
		}
	}
	if a.AgentOnly && rq.Kind != proposal.KindAgent {
		return refuse("%s is asked for only by an agent", a.ID)
	}
	if len(a.Requesters) > 0 {
		ok := false
		for _, k := range a.Requesters {
			ok = ok || k == rq.Kind
		}
		if !ok {
			return refuse("%s cannot be asked for by %s", a.ID, withArticle(rq.Kind))
		}
	}
	return nil
}

// classOf is the registry's class, raised (never lowered) by a hint, by
// the tool's own registry file, and to C5 for destructive tool calls.
func (s *Server) classOf(a *registry.Action, args map[string]any, hint string) string {
	cls := a.Class
	if a.ClassFromHint {
		h, _ := args["class_hint"].(string)
		if proposal.ClassRank(h) < 0 {
			h = a.HintDefault
		}
		if h != "" {
			cls = proposal.MaxClass(cls, h)
		}
		tool, _ := args["tool"].(string)
		op, _ := args["operation"].(string)
		if d, ok := s.o.Reg.Tools[tool][op]; ok {
			cls = proposal.MaxClass(cls, d.Class)
		}
	}
	if a.DestructiveArg != "" {
		if b, _ := args[a.DestructiveArg].(bool); b {
			cls = proposal.C5
		}
	}
	if hint != "" {
		cls = proposal.MaxClass(cls, hint)
	}
	return cls
}

// preview: a trusted planner (a relay, the system assistant) sends the
// preview its planner built; for everyone else the gate builds it from
// the registry, so an agent cannot describe its request in its own words.
func (s *Server) preview(c *conn, rq proposal.Requester, req gate.Request, p *proposal.Proposal) (proposal.Preview, error) {
	trusted := len(c.roles.Relay) > 0 || c.roles.Kind == proposal.KindSystemAssistant
	if trusted && req.Preview != nil {
		return toPreview(req.Preview)
	}
	pv := proposal.Preview{}
	for i, call := range p.Calls {
		a := s.o.Reg.Actions[call.Action]
		show := map[string]any{}
		for _, k := range a.Preview.Show {
			if v, ok := call.Args[k]; ok {
				show[k] = v
			}
		}
		if a.Preview.Commands != "" {
			if argv, ok := call.Args[a.Preview.Commands].([]any); ok {
				pv.Commands = append(pv.Commands, shellQuote(argv))
			}
		}
		key := a.Preview.TitleKey
		if key == "" {
			key = a.ID + ".title"
		}
		if i == 0 && len(p.Calls) == 1 {
			pv.TitleKey, pv.TitleArgs = key, show
			continue
		}
		if i == 0 {
			pv.TitleKey, pv.TitleArgs = "gate.group.title", map[string]any{"count": int64(len(p.Calls))}
		}
		pv.Lines = append(pv.Lines, proposal.Line{Key: key, Args: show})
	}
	return pv, nil
}

// toPreview checks and converts a planner's preview.
func toPreview(in *gate.Preview) (proposal.Preview, error) {
	b, err := json.Marshal(in)
	if err != nil || len(b) > 32<<10 {
		return proposal.Preview{}, refuse("preview unencodable or larger than 32 KiB")
	}
	var pv proposal.Preview
	if err := json.Unmarshal(b, &pv); err != nil {
		return pv, refuse("preview: %v", err)
	}
	if pv.TitleKey == "" || !proposal.ActionRe.MatchString(pv.TitleKey) {
		return pv, refuse("preview title_key %q", pv.TitleKey)
	}
	return pv, nil
}

// policyEnv is the machine's conditions at decision time.
type policyEnv = policy.Env

func policyInput(p *proposal.Proposal, facts []policy.CallFacts, env policy.Env, stopped bool) policy.Input {
	return policy.Input{P: p, Calls: facts, Env: env, Stopped: stopped}
}

// shellQuote renders argv as one line a person can read and paste.
func shellQuote(argv []any) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		s, _ := a.(string)
		if s != "" && strings.IndexFunc(s, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@,+%", r))
		}) < 0 {
			out = append(out, s)
			continue
		}
		out = append(out, "'"+strings.ReplaceAll(s, "'", `'\''`)+"'")
	}
	return strings.Join(out, " ")
}

// allowRate is a token bucket per requester.
func (s *Server) allowRate(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rate := float64(s.cfg.RequestsPerMinute) / 60
	burst := float64(s.cfg.RequestsPerMinute) * 1.5
	b := s.buckets[key]
	if b == nil {
		if len(s.buckets) > 4096 {
			s.buckets = map[string]*bucket{}
		}
		b = &bucket{tokens: burst, last: now}
		s.buckets[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// propose submits (or, for check, only evaluates) a request.
func (s *Server) propose(c *conn, req gate.Request, checkOnly bool) gate.Reply {
	if !s.allowRate(c.owner()) {
		s.refuseOnce(c.owner()+"|rate", ledger.Record{UID: c.p.UID, Event: "gate.request", Outcome: "denied",
			Data: map[string]any{"who": s.peerWho(c), "actions": actionIDs(req.Calls), "reason": "too many requests"}})
		return gate.Reply{OK: true, Decision: gate.Refused, Reason: "too many requests; try again in a minute"}
	}
	p, facts, executor, err := s.build(c, req)
	if err != nil {
		var be buildError
		if !errors.As(err, &be) {
			return gate.Reply{Error: err.Error()}
		}
		if !checkOnly {
			s.refuseOnce(c.owner()+"|"+err.Error(), ledger.Record{UID: c.p.UID, Session: c.roles.Session,
				Event: "gate.request", Outcome: "denied", Data: map[string]any{"who": s.peerWho(c),
					"actions": actionIDs(req.Calls), "reason": err.Error(), "context": c.p.Context}})
		}
		return gate.Reply{OK: true, Decision: gate.Refused, Reason: err.Error(), By: "registry"}
	}
	now := s.now()
	env := s.o.Env(now)
	s.mu.Lock()
	in := policy.Input{P: p, Calls: facts, Env: env, Stopped: s.state.Stop.On}
	d := s.eng.Decide(in, lockedCounter{s})
	if checkOnly {
		s.mu.Unlock()
		return gate.Reply{OK: true, Decision: d.Outcome, Class: p.Class, Reason: d.Reason, By: d.By}
	}
	// Identical pending requests from the same requester are one item.
	owner := c.owner()
	for _, id := range s.order {
		e := s.entries[id]
		if e != nil && e.Status == gate.Asked && e.Owner == owner && e.P.Digest == p.Digest {
			s.mu.Unlock()
			return gate.Reply{OK: true, ID: e.P.ID, Decision: gate.Asked, Class: e.P.Class, Reason: e.Decision.Reason, By: e.Decision.By}
		}
	}
	p.ID = proposal.NewID()
	for s.entries[p.ID] != nil {
		p.ID = proposal.NewID()
	}
	p.Created = now.UTC()
	exp := s.cfg.InteractiveExpiry
	if p.Deferrable {
		exp = s.cfg.DeferrableExpiry
	}
	p.Expires = now.Add(exp).UTC()
	e := &entry{P: p, Facts: facts, Status: gate.Asked, Decision: d, Owner: owner, Executor: executor,
		NeedsAdmin: s.needsAdmin(facts), done: make(chan struct{})}
	s.addLocked(e)
	s.recordRequest(e, env)
	if d.Limit != nil {
		s.pauseLocked(d.Limit, e)
	}
	switch d.Outcome {
	case policy.Allowed:
		s.useLocked(d.Rules, p, now)
		e.finish(gate.Allowed, d.By, now)
	case policy.Refused:
		e.finish(gate.Refused, d.By, now)
	}
	s.recordDecision(e, decisionOutcome(e), d.By, d.Effect, d.Reason)
	if e.Status == gate.Allowed && s.requesterRuns(e) {
		s.claimLocked(e, "requester", owner, now)
	}
	s.publishLocked(gate.Event{Type: "request", ID: p.ID, Note: e.Status}, e)
	s.saveQueueLocked()
	start := s.unitFor(e)
	s.mu.Unlock()
	s.startUnits(start)
	return gate.Reply{OK: true, ID: p.ID, Decision: e.Status, Class: p.Class, Reason: d.Reason, By: d.By}
}

func actionIDs(calls []gate.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, clip(c.Action, 64))
	}
	return out
}

func (s *Server) needsAdmin(facts []policy.CallFacts) bool {
	for _, f := range facts {
		if proposal.ClassRank(f.Class) >= 4 {
			return true
		}
		if f.System {
			// A system action whose executor applies the administrator's own
			// policy (decide_auth = session) is the person's to approve.
			if a := s.o.Reg.Actions[f.Call.Action]; a == nil || a.DecideAuth != "session" {
				return true
			}
		}
	}
	return false
}

// decisionOutcome is the word recorded for an entry's state.
func decisionOutcome(e *entry) string {
	switch e.Status {
	case gate.Allowed:
		if strings.HasPrefix(e.DecidedBy, "person:") {
			return "approved"
		}
		return "allowed"
	case gate.Asked:
		return "asked"
	}
	return e.Status
}

func (s *Server) requesterRuns(e *entry) bool {
	ex, ok := s.o.Reg.Executors[e.Executor]
	return ok && ex.Requester
}

func (s *Server) addLocked(e *entry) {
	s.entries[e.P.ID] = e
	s.order = append(s.order, e.P.ID)
	// Keep every pending request; drop the oldest decided ones.
	for len(s.order) > maxEntries {
		dropped := false
		for i, id := range s.order {
			if x := s.entries[id]; x == nil || x.final() {
				delete(s.entries, id)
				s.order = append(s.order[:i], s.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			break
		}
	}
}

// lockedCounter is the engine's view of limits state (s.mu held).
type lockedCounter struct{ s *Server }

func (l lockedCounter) Count(rule, chain string, since time.Time) int {
	n := 0
	for _, u := range l.s.state.Uses {
		if u.Rule == rule && (chain == "" || u.Chain == chain) && !u.Time.Before(since) {
			n++
		}
	}
	return n
}

func (l lockedCounter) Paused(rule string) bool {
	_, ok := l.s.state.Paused[rule]
	return ok
}

// useLocked counts an allowed request against the rules that allowed it.
func (s *Server) useLocked(refs []string, p *proposal.Proposal, now time.Time) {
	byRef := map[string]policy.Rule{}
	for _, r := range s.eng.Rules() {
		byRef[r.Ref()] = r
	}
	chain := policy.ChainKey(p.Requester)
	for _, ref := range refs {
		if r, ok := byRef[ref]; ok && r.Limits != nil {
			s.state.Uses = append(s.state.Uses, storeUse(r.Key(), chain, now))
		}
	}
	// Keep two days of uses.
	cut := now.Add(-48 * time.Hour)
	i := 0
	for i < len(s.state.Uses) && s.state.Uses[i].Time.Before(cut) {
		i++
	}
	s.state.Uses = s.state.Uses[i:]
	s.saveStateLocked()
}

// pauseLocked trips a rule's circuit breaker.
func (s *Server) pauseLocked(t *policy.LimitTrip, e *entry) {
	if _, already := s.state.Paused[t.Key]; already {
		return
	}
	s.state.Paused[t.Key] = t.Limit
	s.saveStateLocked()
	s.record(ledger.Record{UID: e.P.Requester.UID, Event: "gate.limit", Data: map[string]any{"rule": t.Rule, "key": t.Key,
		"limit": t.Limit, "id": e.P.ID}})
	s.publishLocked(gate.Event{Type: "rules", Note: "paused " + t.Key}, nil)
}

func (s *Server) saveStateLocked() {
	if err := s.o.Store.SaveState(s.state); err != nil {
		s.o.Logf("saving state: %v", err)
	}
}

// queueFile is what queue.json holds.
type queueFile struct {
	V       int      `json:"v"`
	Entries []*entry `json:"entries"`
}

func (s *Server) saveQueueLocked() {
	q := queueFile{V: 1}
	for _, id := range s.order {
		if e := s.entries[id]; e != nil {
			q.Entries = append(q.Entries, e)
		}
	}
	if err := s.o.Store.SaveQueue(q); err != nil {
		s.o.Logf("saving the queue: %v", err)
	}
}

func (s *Server) loadQueue() {
	var q queueFile
	if err := s.o.Store.LoadQueue(&q); err != nil {
		s.o.Logf("queue: %v (starting with an empty queue)", err)
		return
	}
	for _, e := range q.Entries {
		if e == nil || e.P == nil || !proposal.ValidID(e.P.ID) {
			continue
		}
		e.done = make(chan struct{})
		if e.final() {
			close(e.done)
		}
		s.entries[e.P.ID] = e
		s.order = append(s.order, e.P.ID)
	}
}

// expireLoop ends requests nobody decided in time.
func (s *Server) expireLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		s.ExpireNow()
	}
}

// ExpireNow expires overdue pending requests.
func (s *Server) ExpireNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	changed := false
	for _, id := range s.order {
		e := s.entries[id]
		if e != nil && e.Status == gate.Asked && !now.Before(e.P.Expires) {
			e.finish(gate.Expired, "expiry", now)
			s.recordDecision(e, "expired", "expiry", "", "nobody decided in time")
			s.publishLocked(gate.Event{Type: "decision", ID: id, Note: gate.Expired}, e)
			changed = true
		}
	}
	if changed {
		s.saveQueueLocked()
	}
}

// ---------------------------------------------------------------------------
// Records

func (s *Server) recordRequest(e *entry, env policy.Env) { s.recordRequestData(e, env, nil) }

func (s *Server) recordRequestData(e *entry, env policy.Env, extra map[string]any) {
	p := e.P
	calls := make([]map[string]any, 0, len(e.Facts))
	for _, f := range e.Facts {
		res := make([]string, 0, len(f.Resources))
		for i, r := range f.Resources {
			if i == 20 {
				break
			}
			res = append(res, clip(r.String(), 256))
		}
		calls = append(calls, map[string]any{"action": f.Call.Action, "class": f.Class, "system": f.System, "resources": res})
	}
	chain := make([]string, 0, len(p.Requester.Via)+1)
	for _, party := range p.Requester.Chain() {
		chain = append(chain, party.String())
	}
	data := map[string]any{"id": p.ID, "group": p.Group, "who": p.Requester.Who(), "actions": p.Actions(),
		"class": p.Class, "class_name": proposal.ClassNames[p.Class], "chain": chain,
		"requester": proposal.Party{Kind: p.Requester.Kind, Name: p.Requester.Name, Session: p.Requester.Session},
		"via":       p.Requester.Via, "taint": p.Requester.Taint, "origin": p.Requester.Origin, "digest": p.Digest,
		"leaves": p.Leaves, "calls": calls, "executor": e.Executor, "relayed_by": p.Requester.RelayedBy,
		"context": p.Requester.Context, "deferrable": p.Deferrable,
		"env": map[string]any{"power": env.Power, "presence": env.Presence, "network": env.Network,
			"local_time": env.Now.Local().Format("Mon 15:04")}}
	if p.Ref != "" {
		data["ref"] = p.Ref
	}
	for k, v := range extra {
		data[k] = v
	}
	s.record(ledger.Record{UID: p.Requester.UID, Session: p.Requester.Session, Event: "gate.request", Data: data,
		Subject: ledger.Subject{Profile: agentProfile(p.Requester)}})
}

func agentProfile(r proposal.Requester) string {
	if r.Kind == proposal.KindAgent {
		return r.Name
	}
	return ""
}

// verdicts in plain English for gate.decision records.
func verdict(outcome, by, reason string) string {
	switch outcome {
	case "allowed":
		return "allowed: " + reason
	case "approved":
		return "approved by " + byText(by)
	case "asked":
		return "waits for a person (" + reason + ")"
	case "refused":
		return "refused: " + reason
	case "declined":
		return "declined by " + byText(by)
	case "expired":
		return "nobody decided in time; it expired"
	case "cancelled":
		return "withdrawn by the requester"
	}
	return outcome + " (" + reason + ")"
}

func byText(by string) string {
	switch {
	case strings.HasPrefix(by, "person:"):
		return "a person (" + strings.TrimPrefix(by, "person:") + ")"
	case strings.HasPrefix(by, "rule:"):
		return "the rule " + strings.TrimPrefix(by, "rule:")
	}
	return by
}

func (s *Server) recordDecision(e *entry, outcome, by, effect, reason string) {
	lo := "ok"
	switch outcome {
	case "allowed", "approved":
		lo = "allowed"
	case "refused", "declined", "expired":
		lo = "denied"
	}
	data := map[string]any{"id": e.P.ID, "actions": e.P.Actions(), "outcome": outcome, "by": by, "effect": effect,
		"verdict": verdict(outcome, by, reason), "class": e.P.Class, "digest": e.P.Digest}
	if len(e.Decision.Rules) > 0 && outcome == "allowed" {
		data["rules"] = e.Decision.Rules
	}
	if e.Decision.Notify != "" && outcome == "allowed" {
		data["notify"] = e.Decision.Notify
	}
	s.record(ledger.Record{UID: e.P.Requester.UID, Session: e.P.Requester.Session, Event: "gate.decision", Outcome: lo,
		Data: data, Subject: ledger.Subject{Profile: agentProfile(e.P.Requester)}})
}

func storeUse(rule, chain string, t time.Time) storeUseT {
	return storeUseT{Rule: rule, Chain: chain, Time: t}
}

func withArticle(kind string) string {
	if kind != "" && strings.ContainsRune("aeio", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
}
