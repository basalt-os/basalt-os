// Package explain turns a finding into plain, friendly English.
//
// A finding is described by Facts: its kind and cause plus every value a
// text may mention (unit names, paths, numbers, snapshot numbers). The
// deterministic templates in this package write the prose from the facts;
// they are what people see by default. An optional model (humanize.go) may
// write the same prose in its own words, but only the prose: commands,
// the apply line, risk and undo are always rendered by the template, and a
// faithfulness check rejects model text that mentions anything the facts do
// not hold.
package explain

import (
	"sort"
	"strconv"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// Facts are the structured facts behind one finding.
type Facts struct {
	Kind    string            `json:"kind"`              // unit, selinux, disk, dnf, snapshot
	Cause   string            `json:"cause"`             // the diagnosed cause or class
	Subject string            `json:"subject,omitempty"` // a unit, an SELinux domain, a mount point
	Values  map[string]string `json:"values,omitempty"`  // named values the text may use
	// Hint: found by the confined view, which cannot check the snapshot
	// the change rests on; root confirms it first.
	Hint bool `json:"hint,omitempty"`
	// Review: below the decision threshold, or no safe automatic change.
	Review bool `json:"review,omitempty"`
	// Incomplete: a probe failed; nothing is proposed.
	Incomplete bool `json:"incomplete,omitempty"`
	// OK: nothing is wrong (a running unit, a disk below its thresholds).
	OK bool `json:"ok,omitempty"`
}

// New returns facts with an empty value map.
func New(kind, cause, subject string) *Facts {
	return &Facts{Kind: kind, Cause: cause, Subject: subject, Values: map[string]string{}}
}

// Set stores a value when it is not empty and returns the facts.
func (f *Facts) Set(key, val string) *Facts {
	if val != "" {
		f.Values[key] = val
	}
	return f
}

// SetInt stores a number when it is positive.
func (f *Facts) SetInt(key string, n int) *Facts {
	if n > 0 {
		f.Values[key] = strconv.Itoa(n)
	}
	return f
}

// V returns a value ("" when absent).
func (f *Facts) V(key string) string {
	if f == nil || f.Values == nil {
		return ""
	}
	return f.Values[key]
}

// Has reports a value.
func (f *Facts) Has(key string) bool { return f.V(key) != "" }

// Keys lists the value names in a stable order.
func (f *Facts) Keys() []string {
	ks := make([]string, 0, len(f.Values))
	for k := range f.Values {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Change is one planned change as the model sees it: the kind and its
// validated parameters, never a command line.
type Change struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params"`
}

// Changes converts actions for a model's input.
func Changes(acts []action.Action) []Change {
	out := make([]Change, 0, len(acts))
	for _, a := range acts {
		out = append(out, Change{Kind: a.Kind, Params: a.Params})
	}
	return out
}
