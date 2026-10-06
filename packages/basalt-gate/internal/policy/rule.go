// Package policy is the gate's deterministic decision engine: rules as
// data, hard limits checked before any rule, conditions, limits with a
// circuit breaker, and "the most restrictive matching effect wins". No
// model is involved anywhere here; a model may draft a rule, a person
// confirms it.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/canon"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
)

// Effects, from the least to the most restrictive.
const (
	AllowQuiet  = "allow-quiet"
	AllowTell   = "allow-tell"
	AskRemember = "ask-remember"
	Ask         = "ask"
	Refuse      = "refuse"
)

var effectRank = map[string]int{AllowQuiet: 0, AllowTell: 1, AskRemember: 2, Ask: 3, Refuse: 4}

// EffectRank returns how restrictive an effect is (-1: unknown).
func EffectRank(e string) int {
	if r, ok := effectRank[e]; ok {
		return r
	}
	return -1
}

// Allows reports an effect that approves without asking.
func Allows(e string) bool { return e == AllowQuiet || e == AllowTell }

// Scopes of a rule.
const (
	ScopeSystem = "system" // written with administrator authentication
	ScopeUser   = "user"   // one user's own C0 to C3 actions
)

// Resources limits where a rule applies.
type Resources struct {
	PathBeneath []string `json:"path_beneath,omitempty"`
	Exclude     []string `json:"exclude,omitempty"`
	Units       []string `json:"units,omitempty"`
	Packages    []string `json:"packages,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
	Recipients  []string `json:"recipients,omitempty"`
	Booleans    []string `json:"booleans,omitempty"`
	Mailboxes   []string `json:"mailboxes,omitempty"`
}

// Trigger lets the gate start the job itself (origin = schedule).
type Trigger struct {
	Schedule string `json:"schedule"`
}

// When are the conditions of a rule, in local time.
type When struct {
	Days     string `json:"days,omitempty"`     // mon-sun, mon-fri, sat,sun
	Hours    string `json:"hours,omitempty"`    // 00:00-24:00, 22:00-06:00
	Power    string `json:"power,omitempty"`    // any, ac, battery
	Network  string `json:"network,omitempty"`  // any, metered, home
	Presence string `json:"presence,omitempty"` // any, present, away
}

// Limits cap what a rule allows; reaching one turns the decision into
// "ask" and pauses the rule until a person looks.
type Limits struct {
	PerRunItems      int    `json:"per_run_items,omitempty"`
	PerRunBytes      string `json:"per_run_bytes,omitempty"`
	PerDay           int    `json:"per_day,omitempty"`
	RequesterPerHour int    `json:"requester_per_hour,omitempty"`
}

// Created says who confirmed a rule, when and where.
type Created struct {
	By  string `json:"by,omitempty"` // person, preset, remember
	UID int    `json:"uid,omitempty"`
	At  string `json:"at,omitempty"`
	Via string `json:"via,omitempty"`
}

// Rule is one policy rule.
type Rule struct {
	ID          string     `json:"id"`
	SentenceKey string     `json:"sentence_key,omitempty"`
	Effect      string     `json:"effect"`
	Actions     []string   `json:"actions"`
	Requesters  []string   `json:"requesters"`
	Resources   *Resources `json:"resources,omitempty"`
	Trigger     *Trigger   `json:"trigger,omitempty"`
	When        *When      `json:"when,omitempty"`
	Limits      *Limits    `json:"limits,omitempty"`
	TaintMax    string     `json:"taint_max,omitempty"`
	Expires     string     `json:"expires,omitempty"`
	Remember    string     `json:"remember,omitempty"`
	Notify      string     `json:"notify,omitempty"`
	Created     *Created   `json:"created,omitempty"`
	// Set by the store, never by the rule's author.
	Scope     string `json:"scope,omitempty"`
	UID       int    `json:"uid,omitempty"`
	Temporary bool   `json:"temporary,omitempty"`
	Preset    string `json:"preset,omitempty"`
	// From is the key of the ask-remember rule a temporary rule answers:
	// while it lasts, that rule does not ask again for what it covers.
	From string `json:"from,omitempty"`
}

// Hash is the first 8 hex digits of the SHA-256 of the rule's canonical
// form: "rule:<id>@<hash>" names exactly the version that decided.
func (r Rule) Hash() string {
	b, err := canon.Marshal(r)
	if err != nil {
		return "00000000"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:8]
}

// Ref is "rule:<id>@<hash>".
func (r Rule) Ref() string { return "rule:" + r.ID + "@" + r.Hash() }

// Key identifies a rule in its scope.
func (r Rule) Key() string {
	if r.Scope == ScopeUser {
		return fmt.Sprintf("user/%d/%s", r.UID, r.ID)
	}
	return "system/" + r.ID
}

var (
	ruleIDRe   = regexp.MustCompile(`^r-[a-z0-9][a-z0-9-]{0,40}$`)
	selectorRe = regexp.MustCompile(`^(any|person|assistant|system-assistant|agent|app|tool|schedule|phone)(:[A-Za-z0-9._@+-]{1,80})?$`)
	notifies   = map[string]bool{"": true, "none": true, "desktop": true, "phone-if-away": true, "phone": true}
)

// RuleSet is what a rule file holds: [[rule]] tables.
type RuleSet struct {
	Rules []Rule `json:"rule"`
}

// ParseRules reads rules from TOML (the subset of ParseTOML) or, when the
// text starts with "{", JSON of the same shape ({"rule": [...]}).
func ParseRules(src string) ([]Rule, error) {
	var raw []byte
	if strings.HasPrefix(strings.TrimSpace(src), "{") {
		raw = []byte(src)
	} else {
		m, err := ParseTOML(src)
		if err != nil {
			return nil, err
		}
		for k := range m {
			if k != "rule" {
				return nil, fmt.Errorf("unexpected top-level key %q (rule files hold [[rule]] tables)", k)
			}
		}
		if raw, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var rs RuleSet
	if err := dec.Decode(&rs); err != nil {
		return nil, err
	}
	for _, r := range rs.Rules {
		if r.Scope != "" || r.UID != 0 || r.Temporary || r.Preset != "" || r.From != "" {
			return nil, fmt.Errorf("rule %s: scope, uid, temporary, preset and from are set by the gate, not by a rule file", r.ID)
		}
	}
	return rs.Rules, nil
}

// LoadPresets reads every preset file (*.toml) of dir, by name.
func LoadPresets(dir string) (map[string][]Rule, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	out := map[string][]Rule{}
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return nil, err
		}
		rules, err := ParseRules(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		name := strings.TrimSuffix(filepath.Base(n), ".toml")
		for i := range rules {
			rules[i].Preset = name
		}
		out[name] = rules
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Validation

// Validate checks a rule for a scope against the registry and fills
// defaults (taint_max system; an allow rule above C1 without an expiry
// expires after 90 days). now is the confirmation time.
func Validate(r *Rule, reg *registry.Registry, scope string, now time.Time) error {
	if !ruleIDRe.MatchString(r.ID) {
		return fmt.Errorf("rule id %q (r- followed by lower-case letters, digits and dashes)", r.ID)
	}
	if EffectRank(r.Effect) < 0 {
		return fmt.Errorf("effect %q (ask, ask-remember, allow-tell, allow-quiet, refuse)", r.Effect)
	}
	if len(r.Actions) == 0 {
		return errors.New("a rule names at least one action")
	}
	if len(r.Requesters) == 0 {
		return errors.New("a rule names at least one requester (or any)")
	}
	for _, s := range r.Requesters {
		if !selectorRe.MatchString(s) {
			return fmt.Errorf("requester %q", s)
		}
	}
	if scope != ScopeSystem && scope != ScopeUser {
		return fmt.Errorf("scope %q", scope)
	}
	maxClass, anySystem, err := reach(r.Actions, reg)
	if err != nil {
		return err
	}
	loosens := Allows(r.Effect) || r.Effect == AskRemember
	if r.Effect == AllowQuiet && proposal.ClassRank(maxClass) > 1 {
		return fmt.Errorf("allow-quiet covers only Look and Undoable actions (C0, C1); this rule reaches %s", maxClass)
	}
	if scope == ScopeUser && loosens {
		if anySystem {
			return errors.New("a user rule cannot cover a system action")
		}
		if proposal.ClassRank(maxClass) > 3 {
			return fmt.Errorf("a user rule covers C0 to C3 actions; this rule reaches %s", maxClass)
		}
	}
	if r.TaintMax == "" {
		r.TaintMax = "system"
	}
	if proposal.TaintRank(r.TaintMax) < 0 || !validTaint(r.TaintMax) {
		return fmt.Errorf("taint_max %q (none, system, personal, web)", r.TaintMax)
	}
	if !notifies[r.Notify] {
		return fmt.Errorf("notify %q", r.Notify)
	}
	if r.Effect == AskRemember {
		if r.Remember == "" {
			r.Remember = "8h"
		}
		d, err := ParseDuration(r.Remember)
		if err != nil || d <= 0 || d > 7*24*time.Hour {
			return fmt.Errorf("remember %q (up to 7d)", r.Remember)
		}
	} else if r.Remember != "" {
		return errors.New("remember is only for ask-remember rules")
	}
	if r.Trigger != nil {
		if _, err := ParseSchedule(r.Trigger.Schedule); err != nil {
			return err
		}
	}
	if r.When != nil {
		if err := r.When.check(); err != nil {
			return err
		}
	}
	if r.Limits != nil {
		l := r.Limits
		if l.PerRunItems < 0 || l.PerDay < 0 || l.RequesterPerHour < 0 {
			return errors.New("limits cannot be negative")
		}
		if l.PerRunBytes != "" {
			if _, err := ParseBytes(l.PerRunBytes); err != nil {
				return err
			}
		}
	}
	if r.Resources != nil {
		for _, p := range append(append([]string{}, r.Resources.PathBeneath...), r.Resources.Exclude...) {
			if !strings.HasPrefix(p, "/") && p != "~" && !strings.HasPrefix(p, "~/") {
				return fmt.Errorf("path %q must be absolute or start with ~/", p)
			}
			if strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
				return fmt.Errorf("path %q goes up with ..", p)
			}
		}
	}
	created := now
	if r.Created != nil && r.Created.At != "" {
		if t, err := time.Parse(time.RFC3339, r.Created.At); err == nil {
			created = t
		}
	}
	if r.Expires == "" && loosens && (proposal.ClassRank(maxClass) > 1) && !r.Temporary {
		r.Expires = "90d"
	}
	if r.Expires != "" {
		exp, err := ExpiryOf(r.Expires, created)
		if err != nil {
			return err
		}
		if loosens && proposal.ClassRank(maxClass) > 1 && exp.Sub(created) > 366*24*time.Hour {
			return errors.New("rules above Undoable (C1) expire within a year")
		}
		// A duration becomes the time it ends, so the stored rule says
		// exactly when it stops.
		if _, derr := ParseDuration(r.Expires); derr == nil {
			r.Expires = exp.UTC().Format(time.RFC3339)
		}
	} else if r.Expires == "" && loosens && proposal.ClassRank(maxClass) > 1 {
		return errors.New("rules above Undoable (C1) must expire")
	}
	r.Scope = scope
	return nil
}

func validTaint(t string) bool {
	for _, v := range proposal.Taints {
		if v == t {
			return true
		}
	}
	return false
}

// reach returns the highest class a rule's action selectors can cover and
// whether they include a system action.
func reach(sels []string, reg *registry.Registry) (string, bool, error) {
	maxClass, system := proposal.C0, false
	for _, s := range sels {
		switch {
		case s == "*":
			return proposal.C5, true, nil
		case strings.HasPrefix(s, "class:"):
			c := strings.TrimPrefix(s, "class:")
			if proposal.ClassRank(c) < 0 {
				return "", false, fmt.Errorf("action selector %q", s)
			}
			maxClass = proposal.MaxClass(maxClass, c)
			for _, id := range reg.Expand(s) {
				system = system || reg.Actions[id].System
			}
		case strings.HasPrefix(s, "group:"):
			ids := reg.Expand(s)
			if len(ids) == 0 {
				return "", false, fmt.Errorf("unknown action group %q", s)
			}
			for _, id := range ids {
				a := reg.Actions[id]
				maxClass = proposal.MaxClass(maxClass, a.Class)
				system = system || a.System
			}
		default:
			if !proposal.ActionRe.MatchString(s) {
				return "", false, fmt.Errorf("action selector %q", s)
			}
			a := reg.Actions[s]
			if a == nil {
				// Unknown today: assume the worst for what it may reach.
				maxClass, system = proposal.C5, true
				continue
			}
			c := a.Class
			if a.ClassFromHint {
				c = proposal.C5 // the tool decides its hint; assume the worst
			}
			maxClass = proposal.MaxClass(maxClass, c)
			system = system || a.System
		}
	}
	return maxClass, system, nil
}

// ParseDuration reads 30m, 8h, 7d, 90d (and Go durations).
func ParseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		d, err := strconv.Atoi(n)
		if err != nil || d < 0 || d > 3660 {
			return 0, fmt.Errorf("duration %q", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("duration %q", s)
	}
	return d, nil
}

// ExpiryOf reads an expiry: a date (2027-01-06, local midnight), an
// RFC 3339 time, or a duration after created.
func ExpiryOf(s string, created time.Time) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := ParseDuration(s); err == nil {
		return created.Add(d), nil
	}
	return time.Time{}, fmt.Errorf("expires %q (a date, a time or a duration such as 90d; never forever)", s)
}

// ExpiresAt returns when a rule ends (zero: never).
func (r Rule) ExpiresAt() time.Time {
	if r.Expires == "" {
		return time.Time{}
	}
	created := time.Time{}
	if r.Created != nil && r.Created.At != "" {
		created, _ = time.Parse(time.RFC3339, r.Created.At)
	}
	t, err := ExpiryOf(r.Expires, created)
	if err != nil {
		// An unreadable expiry has already ended: fail closed.
		return time.Unix(1, 0)
	}
	return t
}

// ParseBytes reads 500, 20K, 20M, 20G, 1T.
func ParseBytes(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "T"):
		mult, s = 1<<40, strings.TrimSuffix(s, "T")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("size %q (e.g. 20G)", s)
	}
	return n * mult, nil
}

// ---------------------------------------------------------------------------
// Sentences (English; the catalogs of the shell and the Approvals app
// render the same fields in other languages)

// Sentence renders a rule from its structured fields (never from text a
// model wrote).
func (r Rule) Sentence() string {
	who := requestersText(r.Requesters)
	what := strings.Join(r.Actions, ", ")
	var s string
	switch r.Effect {
	case Ask:
		s = fmt.Sprintf("Always ask before %s does %s", who, what)
	case AskRemember:
		s = fmt.Sprintf("Ask once before %s does %s, then remember the answer for %s", who, what, r.Remember)
	case AllowTell:
		s = fmt.Sprintf("Let %s do %s without asking, and tell you", who, what)
	case AllowQuiet:
		s = fmt.Sprintf("Let %s do %s without asking", who, what)
	case Refuse:
		s = fmt.Sprintf("Never let %s do %s", who, what)
	default:
		s = r.Effect + " " + what
	}
	if rs := r.Resources; rs != nil {
		var parts []string
		if len(rs.PathBeneath) > 0 {
			parts = append(parts, "in "+strings.Join(rs.PathBeneath, ", "))
		}
		if len(rs.Exclude) > 0 {
			parts = append(parts, "except "+strings.Join(rs.Exclude, ", "))
		}
		for _, l := range []struct {
			n string
			v []string
		}{{"units", rs.Units}, {"packages", rs.Packages}, {"hosts", rs.Hosts}, {"recipients", rs.Recipients}, {"booleans", rs.Booleans}, {"mailboxes", rs.Mailboxes}} {
			if len(l.v) > 0 {
				parts = append(parts, "for the "+l.n+" "+strings.Join(l.v, ", "))
			}
		}
		if len(parts) > 0 {
			s += " " + strings.Join(parts, " ")
		}
	}
	if r.Trigger != nil {
		s += ", on schedule " + r.Trigger.Schedule
	}
	if w := r.When; w != nil {
		var c []string
		if w.Days != "" && w.Days != "mon-sun" {
			c = append(c, "on "+w.Days)
		}
		if w.Hours != "" && w.Hours != "00:00-24:00" {
			c = append(c, "between "+strings.Replace(w.Hours, "-", " and ", 1))
		}
		if w.Power != "" && w.Power != "any" {
			c = append(c, "on "+map[string]string{"ac": "AC power", "battery": "battery"}[w.Power])
		}
		if w.Presence == "away" {
			c = append(c, "only while you are away")
		} else if w.Presence == "present" {
			c = append(c, "only while you are here")
		}
		if len(c) > 0 {
			s += ", " + strings.Join(c, ", ")
		}
	}
	if r.Expires != "" {
		s += ", until " + r.ExpiresAt().Local().Format("2006-01-02 15:04")
	}
	return s
}

func requestersText(sels []string) string {
	out := make([]string, 0, len(sels))
	for _, s := range sels {
		k, n, _ := strings.Cut(s, ":")
		switch k {
		case "any":
			out = append(out, "anyone")
		case "person":
			out = append(out, "you")
		case "assistant":
			out = append(out, "the assistant")
		case "system-assistant":
			out = append(out, "the system assistant")
		case "agent":
			if n != "" {
				out = append(out, "the agent "+n)
			} else {
				out = append(out, "any agent")
			}
		case "app":
			out = append(out, "the app "+n)
		case "tool":
			out = append(out, "the tool "+n)
		default:
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " or ")
}
