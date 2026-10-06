package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
)

// parseRuleText reads a rule file (the TOML subset of the gate, or JSON
// {"rule": [...]}) into one object per rule.
func parseRuleText(text string) ([]map[string]any, error) {
	var m map[string]any
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			return nil, err
		}
	} else {
		var err error
		if m, err = policy.ParseTOML(text); err != nil {
			return nil, err
		}
	}
	for k := range m {
		if k != "rule" {
			return nil, fmt.Errorf("unexpected top-level key %q (rule files hold [[rule]] tables)", k)
		}
	}
	l, ok := m["rule"].([]any)
	if !ok {
		return nil, errors.New("no [[rule]] tables")
	}
	out := make([]map[string]any, 0, len(l))
	for _, e := range l {
		r, ok := e.(map[string]any)
		if !ok {
			return nil, errors.New("a rule is a table")
		}
		out = append(out, r)
	}
	return out, nil
}
