package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
)

// Hard limits are checked before any rule, in two tiers:
//
//   - locked: no rule can allow them, the request is refused; an
//     administrator unlocks one occurrence (one digest, 10 minutes) at the
//     local console with a second factor;
//   - always ask: no rule can make them automatic; a normal confirmation
//     is enough.
//
// They are code plus data: the built-in list below always applies, and
// /usr/share/basalt/gate/hardlimits.json adds to it (never removes).

// Tiers.
const (
	TierLocked    = "locked"
	TierAlwaysAsk = "always-ask"
)

// ResMatch matches a call's resources of one kind.
type ResMatch struct {
	Kind     string   `json:"kind"`
	Values   []string `json:"values,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
	// NotListedIn: the resource is not among the destinations the deciding
	// rule lists (used for "new destinations always ask").
	Any bool `json:"any,omitempty"`
}

// Matcher is one hard limit.
type Matcher struct {
	Name         string            `json:"name"`
	Why          string            `json:"why"`
	Action       string            `json:"action,omitempty"`
	ActionPrefix string            `json:"action_prefix,omitempty"`
	Class        string            `json:"class,omitempty"`
	ArgEquals    map[string]string `json:"arg_equals,omitempty"`
	Resource     *ResMatch         `json:"resource,omitempty"`
	RequesterNot string            `json:"requester_not,omitempty"` // matches unless the requester is this kind
	// Argv matches tool.exec calls: each pattern is a command name
	// (basename) and leading arguments; "*" matches any one argument and
	// a trailing "prefix*" matches an argument by prefix.
	Argv [][]string `json:"argv,omitempty"`
}

// HardLimits is the loaded set.
type HardLimits struct {
	Locked    []Matcher `json:"locked"`
	AlwaysAsk []Matcher `json:"always_ask"`
}

// builtin limits hold even without the data file.
var builtin = HardLimits{
	Locked: []Matcher{
		{Name: "gate.disable", Why: "the approval gate itself cannot be turned off by a request", Action: "gate.disable"},
		{Name: "gate.limits", Why: "hard limits cannot be loosened", Action: "gate.limits.change"},
		{Name: "selinux.permissive", Why: "SELinux stays enforcing", Action: "selinux.mode"},
		{Name: "ledger.disable", Why: "the audit ledger cannot be turned off or rewritten", ActionPrefix: "ledger."},
	},
	AlwaysAsk: []Matcher{
		{Name: "cannot-be-undone", Why: "it cannot be undone", Class: proposal.C5},
		{Name: "gate.rules", Why: "a change to the rules widens or narrows what runs without asking", Action: "gate.rule.change"},
	},
}

// LoadHardLimits reads the data file and adds the built-in limits. A
// missing file is fine (the built-ins apply); an unreadable one is an
// error, so the gate does not start with less than it should.
func LoadHardLimits(file string) (*HardLimits, error) {
	h := &HardLimits{Locked: append([]Matcher{}, builtin.Locked...), AlwaysAsk: append([]Matcher{}, builtin.AlwaysAsk...)}
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	var f HardLimits
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for _, m := range append(append([]Matcher{}, f.Locked...), f.AlwaysAsk...) {
		if m.Name == "" || m.Why == "" {
			return nil, fmt.Errorf("%s: every hard limit has a name and a why", file)
		}
		if m.Action == "" && m.ActionPrefix == "" && m.Class == "" && len(m.Argv) == 0 {
			return nil, fmt.Errorf("%s: hard limit %s matches nothing", file, m.Name)
		}
	}
	h.Locked = append(h.Locked, f.Locked...)
	h.AlwaysAsk = append(h.AlwaysAsk, f.AlwaysAsk...)
	return h, nil
}

// CallFacts is what hard limits and rules look at for one call.
type CallFacts struct {
	Call      proposal.Call
	Class     string
	Resources []proposal.Resource
	System    bool
}

// Check returns the tier and the matching limit for a call (tier "" when
// none applies). Locked wins over always ask.
func (h *HardLimits) Check(c CallFacts, req proposal.Requester) (string, *Matcher) {
	for i := range h.Locked {
		if h.Locked[i].match(c, req) {
			return TierLocked, &h.Locked[i]
		}
	}
	for i := range h.AlwaysAsk {
		if h.AlwaysAsk[i].match(c, req) {
			return TierAlwaysAsk, &h.AlwaysAsk[i]
		}
	}
	return "", nil
}

func (m *Matcher) match(c CallFacts, req proposal.Requester) bool {
	if m.Action != "" && c.Call.Action != m.Action {
		return false
	}
	if m.ActionPrefix != "" && !strings.HasPrefix(c.Call.Action, m.ActionPrefix) {
		return false
	}
	if m.Class != "" && c.Class != m.Class {
		return false
	}
	for k, want := range m.ArgEquals {
		if fmt.Sprint(c.Call.Args[k]) != want {
			return false
		}
	}
	if m.RequesterNot != "" {
		// The person themselves, with no one in between, is exempt.
		if req.Kind == m.RequesterNot && len(req.Via) == 0 {
			return false
		}
	}
	if m.Resource != nil && !m.Resource.match(c.Resources) {
		return false
	}
	if len(m.Argv) > 0 {
		argv := argvOf(c.Call)
		hit := false
		for _, pat := range m.Argv {
			hit = hit || argvMatch(pat, argv)
		}
		if !hit {
			return false
		}
	}
	return true
}

func (r *ResMatch) match(res []proposal.Resource) bool {
	for _, x := range res {
		if x.Kind != r.Kind {
			continue
		}
		if r.Any {
			return true
		}
		for _, v := range r.Values {
			if x.Value == v {
				return true
			}
		}
		for _, p := range r.Prefixes {
			if strings.HasPrefix(x.Value, p) {
				return true
			}
		}
	}
	return false
}

func argvOf(c proposal.Call) []string {
	l, _ := c.Args["argv"].([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// argvMatch matches a command line against a pattern. sudo, pkexec, env
// and doas in front of the command are looked through, so wrapping a
// locked command does not hide it.
func argvMatch(pat, argv []string) bool {
	for len(argv) > 0 {
		switch path.Base(argv[0]) {
		case "sudo", "pkexec", "doas", "env", "nice", "ionice", "nohup", "command", "exec":
			argv = argv[1:]
			for len(argv) > 0 && (strings.HasPrefix(argv[0], "-") || strings.Contains(argv[0], "=")) {
				argv = argv[1:]
			}
			continue
		}
		break
	}
	if len(pat) == 0 || len(argv) == 0 {
		return false
	}
	for i, p := range pat {
		if p == "**" {
			// Every remaining pattern element appears somewhere after.
			for _, q := range pat[i+1:] {
				found := false
				for _, a := range argv[i:] {
					found = found || argMatch(q, a)
				}
				if !found {
					return false
				}
			}
			return true
		}
		if i >= len(argv) {
			return false
		}
		a := argv[i]
		if i == 0 {
			a = path.Base(a)
		}
		if !argMatch(p, a) {
			return false
		}
	}
	return true
}

func argMatch(p, a string) bool {
	switch {
	case p == "*":
		return true
	case strings.HasSuffix(p, "*"):
		return strings.HasPrefix(a, strings.TrimSuffix(p, "*"))
	}
	return a == p
}
