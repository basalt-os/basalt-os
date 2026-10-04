package vsm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
)

// Bind instantiates a case's action templates: a "$name" parameter takes
// the binding the tools extracted, any other value is used as written.
// A missing binding or an action the validators refuse is an error: the
// case is then shown as a hint, never proposed.
func Bind(templates []knowledge.Template, bindings map[string]string) ([]action.Action, error) {
	out := make([]action.Action, 0, len(templates))
	for _, t := range templates {
		params := map[string]string{}
		for _, key := range sortedKeys(t.Params) {
			val := t.Params[key]
			if s, ok := val.(string); ok && strings.HasPrefix(s, "$") {
				name := s[1:]
				if bindings[name] == "" {
					return nil, fmt.Errorf("no binding for %s", name)
				}
				params[key] = bindings[name]
				continue
			}
			params[key] = scalar(val)
		}
		a := action.Action{Kind: t.Kind, Params: params}
		if err := a.Validate(); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	}
	return fmt.Sprint(v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RootConfirmationMissing is the ADR 0008 rule: from the confined view, a
// file restore or a snapshot rollback stays a hint unless a tool that ran
// with root's view confirmed the snapshot (binding root_confirmed=yes: it
// read the snapshot copy, or diffed the snapshot's package set). It
// returns the reason to hold the actions back, or "".
func RootConfirmationMissing(actions []action.Action, view string, bindings map[string]string) string {
	if view != ViewConfined {
		return ""
	}
	kinds := map[string]bool{}
	for _, a := range actions {
		if action.NeedsRootView(a.Kind) {
			kinds[a.Kind] = true
		}
	}
	if len(kinds) == 0 || bindings["root_confirmed"] == "yes" {
		return ""
	}
	return "confined view: " + strings.Join(sortedKeys(kinds), ", ") +
		" needs the snapshot confirmed by the root CLI (ADR 0008)"
}
