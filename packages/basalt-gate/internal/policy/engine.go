package policy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
)

// Outcomes of a decision.
const (
	Allowed = "allowed"
	Asked   = "asked"
	Refused = "refused"
)

// Decision is the engine's answer for one proposal.
type Decision struct {
	Outcome string `json:"outcome"` // allowed, asked, refused
	Effect  string `json:"effect"`
	// By names who decided: rule:<id>@<hash>, hard-limit:<tier>:<name>,
	// stop, default (no rule matched), registry.
	By       string   `json:"by"`
	Rules    []string `json:"rules,omitempty"` // every allow rule used, when several calls
	Reason   string   `json:"reason"`          // plain English
	Remember string   `json:"remember,omitempty"`
	Notify   string   `json:"notify,omitempty"`
	// Limit is set when a rule's limit turned an allow into ask.
	Limit *LimitTrip `json:"limit,omitempty"`
	// RuleKey of the rule that decided (for remember and limits).
	RuleKey string `json:"rule_key,omitempty"`
}

// LimitTrip says which limit of which rule tripped.
type LimitTrip struct {
	Rule  string `json:"rule"`
	Key   string `json:"key"`
	Limit string `json:"limit"`
}

// Input is one proposal with the facts of each call.
type Input struct {
	P     *proposal.Proposal
	Calls []CallFacts
	Env   Env
	// Stopped: the emergency stop is on (every allow becomes ask).
	Stopped bool
}

// Counter remembers what rules allowed, for limits.
type Counter interface {
	// Count returns how many requests the rule allowed since t, all
	// requesters or one (requester "" for all).
	Count(ruleKey, requester string, since time.Time) int
	// Paused reports a rule paused by its circuit breaker.
	Paused(ruleKey string) bool
}

// Engine decides with a rule set.
type Engine struct {
	Reg    *registry.Registry
	Limits *HardLimits
	Home   registry.HomeFunc

	mu    sync.RWMutex
	rules []Rule
}

// NewEngine returns an engine over a registry and hard limits.
func NewEngine(reg *registry.Registry, h *HardLimits, home registry.HomeFunc) *Engine {
	return &Engine{Reg: reg, Limits: h, Home: home}
}

// SetRules replaces the rule set (system rules and every user's).
func (e *Engine) SetRules(rs []Rule) {
	e.mu.Lock()
	e.rules = append([]Rule(nil), rs...)
	e.mu.Unlock()
}

// Rules returns a copy of the rule set.
func (e *Engine) Rules() []Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]Rule(nil), e.rules...)
}

type callResult struct {
	effect, by, reason, notify, remember, ruleKey string
	rules                                         []string
	allowRule                                     *Rule
}

// Decide applies hard limits, then the rules, to every call; the most
// restrictive call decides the proposal. counter may be nil (no limits
// state: limits that count are not checked, as in a dry run).
func (e *Engine) Decide(in Input, counter Counter) Decision {
	return e.decide(in, counter, e.Rules())
}

// DecideWith is Decide with another rule set (dry runs of drafts).
func (e *Engine) DecideWith(in Input, counter Counter, rules []Rule) Decision {
	return e.decide(in, counter, rules)
}

func (e *Engine) decide(in Input, counter Counter, rules []Rule) Decision {
	if len(in.Calls) == 0 {
		return Decision{Outcome: Refused, Effect: Refuse, By: "registry", Reason: "a request has at least one call"}
	}
	var worst *callResult
	var used []string
	for _, c := range in.Calls {
		r := e.decideCall(in, c, counter, rules)
		if Allows(r.effect) {
			used = append(used, r.rules...)
		}
		if worst == nil || EffectRank(r.effect) > EffectRank(worst.effect) {
			cp := r
			worst = &cp
		}
	}
	d := Decision{Effect: worst.effect, By: worst.by, Reason: worst.reason, Notify: worst.notify, Remember: worst.remember,
		RuleKey: worst.ruleKey}
	if Allows(d.Effect) {
		sort.Strings(used)
		d.Rules = uniq(used)
		// Limits of every rule that allows part of the proposal.
		if counter != nil {
			if trip := e.checkLimits(in, counter, rules, d.Rules); trip != nil {
				d.Effect, d.By, d.Limit = Ask, "limit:"+trip.Rule, trip
				d.Reason = fmt.Sprintf("the rule %s reached its limit (%s); a person looks first", trip.Rule, trip.Limit)
			}
		}
	}
	switch {
	case d.Effect == Refuse:
		d.Outcome = Refused
	case Allows(d.Effect):
		d.Outcome = Allowed
	default:
		d.Outcome = Asked
	}
	return d
}

func uniq(l []string) []string {
	var out []string
	for i, s := range l {
		if i == 0 || s != l[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func (e *Engine) decideCall(in Input, c CallFacts, counter Counter, rules []Rule) callResult {
	req := in.P.Requester
	// 1. Hard limits.
	tier, m := e.Limits.Check(c, req)
	if tier == TierLocked {
		return callResult{effect: Refuse, by: "hard-limit:locked:" + m.Name,
			reason: fmt.Sprintf("always protected (%s): %s", m.Name, m.Why)}
	}
	// 2. Rules: the most restrictive matching effect wins; no match asks.
	best := callResult{effect: Ask, by: "default", reason: "no rule covers it, so a person decides"}
	matched := false
	var allowRefs []string
	var hits []*Rule
	answered := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		if !e.matches(r, in, c, counter) {
			continue
		}
		hits = append(hits, r)
		if r.Temporary && r.From != "" {
			answered[r.From] = true
		}
	}
	for _, r := range hits {
		// An ask-remember rule a person already answered for this request
		// (a temporary rule from "approve and remember") does not ask again.
		if r.Effect == AskRemember && answered[r.Key()] {
			continue
		}
		eff := r.Effect
		// allow-quiet never covers more than Undoable (C1).
		if eff == AllowQuiet && proposal.ClassRank(c.Class) > 1 {
			eff = AllowTell
		}
		if Allows(eff) {
			allowRefs = append(allowRefs, r.Ref())
		}
		if !matched || EffectRank(eff) > EffectRank(best.effect) {
			matched = true
			best = callResult{effect: eff, by: r.Ref(), notify: r.Notify, remember: r.Remember, ruleKey: r.Key(),
				reason: reasonFor(eff, r)}
			if Allows(eff) {
				best.allowRule = r
			}
		}
	}
	if Allows(best.effect) {
		best.rules = allowRefs
		if best.notify == "" {
			best.notify = "desktop"
			if best.effect == AllowQuiet {
				best.notify = "none"
			}
		}
	}
	// 3. Always ask: no rule makes these automatic (ask-remember too).
	if tier == TierAlwaysAsk && EffectRank(best.effect) < EffectRank(Ask) {
		return callResult{effect: Ask, by: "hard-limit:always-ask:" + m.Name,
			reason: fmt.Sprintf("always asks (%s): %s", m.Name, m.Why)}
	}
	// 4. The emergency stop suspends every allow rule.
	if in.Stopped && EffectRank(best.effect) < EffectRank(Ask) {
		return callResult{effect: Ask, by: "stop", reason: "automation is stopped; a person decides every request"}
	}
	return best
}

func reasonFor(eff string, r *Rule) string {
	switch eff {
	case Refuse:
		return "never allowed by the rule " + r.ID
	case Ask:
		return "the rule " + r.ID + " always asks"
	case AskRemember:
		return "the rule " + r.ID + " asks once, then remembers for " + r.Remember
	case AllowTell:
		return "allowed by the rule " + r.ID + ", and you are told"
	case AllowQuiet:
		return "allowed by the rule " + r.ID
	}
	return r.ID
}

// matches checks every part of a rule against one call.
func (e *Engine) matches(r *Rule, in Input, c CallFacts, counter Counter) bool {
	req := in.P.Requester
	now := in.Env.Now
	allowish := Allows(r.Effect) || r.Effect == AskRemember
	// Expiry.
	if exp := r.ExpiresAt(); !exp.IsZero() && !now.Before(exp) {
		return false
	}
	// A paused rule (circuit breaker) allows nothing until a person looks.
	if allowish && counter != nil && counter.Paused(r.Key()) {
		return false
	}
	// Scope: a user rule covers only that user's own C0 to C3 actions.
	if r.Scope == ScopeUser {
		if req.UID != r.UID {
			return false
		}
		if allowish && (c.System || proposal.ClassRank(c.Class) > 3) {
			return false
		}
	}
	// Scheduled requests match only the rule that started them, and a
	// rule with a trigger matches nothing else (content cannot trigger it).
	if req.Origin == "schedule" || r.Trigger != nil {
		if req.Origin != "schedule" || r.Trigger == nil || req.Kind != proposal.KindSchedule || req.Name != r.ID {
			return false
		}
	}
	// Actions.
	if !e.actionMatches(r.Actions, c) {
		return false
	}
	// Requesters: every party of the chain (the least trusted decides).
	if req.Origin != "schedule" {
		for _, p := range req.Chain() {
			if !partyMatches(r.Requesters, p) {
				return false
			}
		}
	}
	// Taint: requests that follow content beyond taint_max do not match
	// allow rules (an ask or refuse rule still applies).
	if allowish {
		max := r.TaintMax
		if max == "" {
			max = "system"
		}
		if proposal.TaintRank(req.Taint) > proposal.TaintRank(max) {
			return false
		}
	}
	// Resources.
	if !e.resourcesMatch(r, req, c, allowish) {
		return false
	}
	// Conditions: unknown values never let an allow rule match.
	switch r.When.eval(in.Env) {
	case condNo:
		return false
	case condUnknown:
		if allowish {
			return false
		}
	}
	return true
}

func (e *Engine) actionMatches(sels []string, c CallFacts) bool {
	for _, s := range sels {
		switch {
		case s == "*":
			return true
		case strings.HasPrefix(s, "class:"):
			if c.Class == strings.TrimPrefix(s, "class:") {
				return true
			}
		case strings.HasPrefix(s, "group:"):
			if e.Reg.InGroup(strings.TrimPrefix(s, "group:"), c.Call.Action) {
				return true
			}
		default:
			if s == c.Call.Action {
				return true
			}
		}
	}
	return false
}

func partyMatches(sels []string, p proposal.Party) bool {
	for _, s := range sels {
		k, n, hasName := strings.Cut(s, ":")
		if k == "any" {
			return true
		}
		if k != p.Kind {
			continue
		}
		if !hasName || n == p.Name {
			return true
		}
	}
	return false
}

// resourcesMatch: an allow rule must cover every resource of the call; an
// ask or refuse rule applies when any resource is in its scope. Hosts and
// recipients (destinations) are never covered by an allow rule that does
// not list them: a new destination always asks.
func (e *Engine) resourcesMatch(r *Rule, req proposal.Requester, c CallFacts, allowish bool) bool {
	rs := r.Resources
	if rs == nil {
		if allowish {
			for _, x := range c.Resources {
				if x.Kind == "host" || x.Kind == "recipient" {
					return false
				}
			}
		}
		return true
	}
	if len(c.Resources) == 0 {
		// A rule scoped to resources says nothing about calls that touch
		// none; only a restrictive rule applies to them.
		return !allowish
	}
	uid := req.UID
	if r.Scope == ScopeUser {
		uid = r.UID
	}
	hit := false
	for _, x := range c.Resources {
		ok := e.resourceIn(rs, x, uid)
		if allowish && !ok {
			return false
		}
		hit = hit || ok
	}
	return allowish || hit
}

func (e *Engine) resourceIn(rs *Resources, x proposal.Resource, uid int) bool {
	list := func(l []string, v string) bool {
		for _, s := range l {
			if strings.EqualFold(strings.TrimSuffix(s, "."), v) {
				return true
			}
		}
		return false
	}
	switch x.Kind {
	case "path":
		if len(rs.PathBeneath) == 0 {
			return false
		}
		in := false
		for _, b := range rs.PathBeneath {
			if p, err := registry.ExpandHome(b, uid, e.Home); err == nil && beneath(x.Value, p) {
				in = true
			}
		}
		for _, b := range rs.Exclude {
			p, err := registry.ExpandHome(b, uid, e.Home)
			if err != nil || beneath(x.Value, p) || (x.Beneath && beneath(p, x.Value)) {
				return false
			}
		}
		return in
	case "unit":
		return list(rs.Units, x.Value)
	case "package":
		return list(rs.Packages, x.Value)
	case "host":
		return list(rs.Hosts, x.Value)
	case "recipient":
		return list(rs.Recipients, x.Value)
	case "boolean":
		return list(rs.Booleans, x.Value)
	}
	return false
}

func beneath(p, base string) bool {
	if base == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

// checkLimits checks the limits of the allow rules a decision used.
func (e *Engine) checkLimits(in Input, counter Counter, rules []Rule, refs []string) *LimitTrip {
	byRef := map[string]*Rule{}
	for i := range rules {
		byRef[rules[i].Ref()] = &rules[i]
	}
	items, bytes := 0, int64(0)
	for _, c := range in.Calls {
		items += max(1, len(c.Resources))
		for _, r := range c.Resources {
			bytes += r.Bytes
		}
	}
	now := in.Env.Now
	who := ChainKey(in.P.Requester)
	for _, ref := range refs {
		r := byRef[ref]
		if r == nil || r.Limits == nil {
			continue
		}
		l := r.Limits
		trip := func(s string) *LimitTrip { return &LimitTrip{Rule: r.ID, Key: r.Key(), Limit: s} }
		if l.PerRunItems > 0 && items > l.PerRunItems {
			return trip(fmt.Sprintf("per_run_items %d, this request has %d", l.PerRunItems, items))
		}
		if l.PerRunBytes != "" {
			if n, err := ParseBytes(l.PerRunBytes); err == nil && bytes > n {
				return trip(fmt.Sprintf("per_run_bytes %s", l.PerRunBytes))
			}
		}
		if l.PerDay > 0 {
			y, mo, d := now.Local().Date()
			start := time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
			if counter.Count(r.Key(), "", start) >= l.PerDay {
				return trip(fmt.Sprintf("per_day %d", l.PerDay))
			}
		}
		if l.RequesterPerHour > 0 && counter.Count(r.Key(), who, now.Add(-time.Hour)) >= l.RequesterPerHour {
			return trip(fmt.Sprintf("requester_per_hour %d", l.RequesterPerHour))
		}
	}
	return nil
}

// ChainKey identifies a requester chain for per-requester limits.
func ChainKey(r proposal.Requester) string {
	parts := make([]string, 0, len(r.Via)+1)
	for _, p := range r.Chain() {
		parts = append(parts, p.String())
	}
	return fmt.Sprintf("%d|%s", r.UID, strings.Join(parts, ">"))
}

// NarrowRule is the temporary rule an "approve and remember" creates from
// an ask-remember decision: the same actions, the same requester chain
// and exactly the same resources, allowed and told, for the remember
// duration, in the scope of the rule that asked.
func NarrowRule(p *proposal.Proposal, calls []CallFacts, src Rule, id string, now time.Time) (Rule, error) {
	d, err := ParseDuration(src.Remember)
	if err != nil || d <= 0 {
		return Rule{}, fmt.Errorf("remember %q", src.Remember)
	}
	r := Rule{ID: id, Effect: AllowTell, Scope: src.Scope, UID: src.UID, Temporary: true, From: src.Key(),
		TaintMax: "system", Expires: now.Add(d).UTC().Format(time.RFC3339),
		Created: &Created{By: "remember", UID: p.Requester.UID, At: now.UTC().Format(time.RFC3339), Via: src.ID}}
	seen := map[string]bool{}
	for _, c := range calls {
		if !seen[c.Call.Action] {
			r.Actions = append(r.Actions, c.Call.Action)
			seen[c.Call.Action] = true
		}
	}
	for _, party := range p.Requester.Chain() {
		r.Requesters = append(r.Requesters, party.String())
	}
	res := &Resources{}
	has := false
	for _, c := range calls {
		for _, x := range c.Resources {
			has = true
			switch x.Kind {
			case "path":
				res.PathBeneath = append(res.PathBeneath, x.Value)
			case "unit":
				res.Units = append(res.Units, x.Value)
			case "package":
				res.Packages = append(res.Packages, x.Value)
			case "host":
				res.Hosts = append(res.Hosts, x.Value)
			case "recipient":
				res.Recipients = append(res.Recipients, x.Value)
			case "boolean":
				res.Booleans = append(res.Booleans, x.Value)
			default:
				return Rule{}, fmt.Errorf("a %s resource cannot be remembered", x.Kind)
			}
		}
	}
	if has {
		r.Resources = res
	}
	return r, nil
}
