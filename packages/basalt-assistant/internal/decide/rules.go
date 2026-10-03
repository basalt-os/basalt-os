package decide

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Rules is the deterministic backend. Each question has a prior over its
// options and a list of rules; a rule that matches the question's features
// multiplies the weight of the options it names. The product is normalized
// into probabilities. The factors are fixed by hand from lab cases (a
// likelihood-ratio style estimate), so the probabilities are calibrated
// only roughly; the shared evaluation suite measures calibration and the
// thresholds are configurable for that reason.
type Rules struct{}

// Name implements Backend.
func (Rules) Name() string { return "rules/v1" }

// rule: when every feature in All is true and none in None is, multiply.
type rule struct {
	Name   string
	All    []string
	None   []string
	Factor map[string]float64
}

type ruleSet struct {
	Prior map[string]float64
	Rules []rule
}

// The rule sets per question. Option names must match the question's.
var ruleSets = map[string]ruleSet{
	// Why did a unit fail?
	"unit.cause": {
		Prior: map[string]float64{"config_error": 1, "selinux_denial": 1, "port_conflict": 1, "dependency_failed": 1,
			"disk_full": 1, "missing_file": 1, "crashed": 1, "unknown": 2},
		Rules: []rule{
			{"config check failed", []string{"config_check_failed"}, nil, map[string]float64{"config_error": 30}},
			{"config error in the journal", []string{"journal_config_error"}, nil, map[string]float64{"config_error": 12}},
			{"the journal names the file and line", []string{"journal_config_location"}, nil, map[string]float64{"config_error": 4}},
			{"config check passed", []string{"config_check_passed"}, nil, map[string]float64{"config_error": 0.1}},
			{"denials for the unit's domain", []string{"avc_for_domain"}, nil, map[string]float64{"selinux_denial": 25}},
			{"permission denied in the journal", []string{"journal_permission_denied"}, nil, map[string]float64{"selinux_denial": 3, "missing_file": 1.5}},
			{"address already in use", []string{"journal_address_in_use"}, nil, map[string]float64{"port_conflict": 30}},
			{"port owned by another process", []string{"port_owner_found"}, nil, map[string]float64{"port_conflict": 5}},
			{"a dependency failed", []string{"dependency_failed"}, nil, map[string]float64{"dependency_failed": 30}},
			{"file system full", []string{"disk_full"}, nil, map[string]float64{"disk_full": 20}},
			{"ENOSPC in the journal", []string{"journal_no_space"}, nil, map[string]float64{"disk_full": 25}},
			{"missing file", []string{"journal_no_such_file"}, nil, map[string]float64{"missing_file": 10}},
			{"killed by a signal or core dump", []string{"signal_or_core"}, nil, map[string]float64{"crashed": 15}},
			{"a path in a permission error has a wrong or generic label", []string{"path_label_problem"}, nil, map[string]float64{"selinux_denial": 15}},
			{"the path is mislabeled (default label would work)", []string{"path_mislabeled"}, nil, map[string]float64{"selinux_denial": 3}},
			{"the denial explains a permission error", []string{"avc_for_domain", "journal_permission_denied"}, nil, map[string]float64{"selinux_denial": 3}},
		},
	},
	// What kind of SELinux denial is this?
	"avc.class": {
		Prior: map[string]float64{"mislabeled": 1, "missing_fcontext": 1, "port": 1, "boolean": 1, "unknown": 2, "suspicious": 1},
		Rules: []rule{
			{"default label differs and is allowed", []string{"default_differs", "default_allowed"}, nil, map[string]float64{"mislabeled": 60}},
			{"default label differs", []string{"default_differs"}, []string{"default_allowed"}, map[string]float64{"mislabeled": 2, "unknown": 1.5}},
			{"a boolean allows it and is off", []string{"boolean_off"}, nil, map[string]float64{"boolean": 50}},
			{"port label for the domain found", []string{"port_case", "port_type_found"}, nil, map[string]float64{"port": 60}},
			{"port already owned by another type", []string{"port_owned_by_other"}, nil, map[string]float64{"port": 0.25, "unknown": 2}},
			{"generic label and a service type exists", []string{"generic_target", "candidate_type"}, nil, map[string]float64{"missing_fcontext": 40}},
			{"service type exists, label not generic", []string{"candidate_type"}, []string{"generic_target"}, map[string]float64{"missing_fcontext": 3, "unknown": 2}},
			{"object not located", nil, []string{"has_path", "port_case"}, map[string]float64{"unknown": 4}},
			{"security-sensitive target", []string{"sensitive_target"}, nil, map[string]float64{"suspicious": 400}},
			{"permissive domain", []string{"permissive"}, nil, map[string]float64{"unknown": 1.2}},
			{"found by a label check (no AVC logged)", []string{"no_avc"}, nil, map[string]float64{"unknown": 1.5}},
		},
	},
	// Severity on a 1 to 5 rubric (1 informational, 5 the system or a
	// service is down or about to be).
	"event.severity": {
		Prior: map[string]float64{"1": 1, "2": 2, "3": 2, "4": 1, "5": 0.5},
		Rules: []rule{
			{"unit failed", []string{"unit_failed"}, nil, map[string]float64{"4": 12, "5": 4, "3": 1.5}},
			{"denial blocked a service", []string{"avc", "enforcing"}, nil, map[string]float64{"3": 4, "4": 3}},
			{"denial only logged (permissive)", []string{"avc"}, []string{"enforcing"}, map[string]float64{"2": 4}},
			{"suspicious denial", []string{"suspicious"}, nil, map[string]float64{"4": 4, "5": 2}},
			{"disk above warning", []string{"disk_warn"}, nil, map[string]float64{"3": 6}},
			{"disk above critical", []string{"disk_crit"}, nil, map[string]float64{"5": 30, "4": 8}},
			{"failed package transaction", []string{"dnf_failed"}, nil, map[string]float64{"4": 12, "3": 2, "5": 2}},
		},
	},
	// Tell the person now (vs. only list it in `basalt pending`)?
	"event.notify": {
		Prior: map[string]float64{"true": 1, "false": 1},
		Rules: []rule{
			{"severity 4 or more", []string{"severity_high"}, nil, map[string]float64{"true": 9}},
			{"severity 2 or less", []string{"severity_low"}, nil, map[string]float64{"false": 6}},
			{"repeat of a known event", []string{"repeat"}, nil, map[string]float64{"false": 4}},
		},
	},
	// What should a failed package transaction lead to?
	"dnf.next": {
		Prior: map[string]float64{"rollback": 1, "investigate": 2},
		Rules: []rule{
			{"pre snapshot without post", []string{"pre_without_post"}, nil, map[string]float64{"rollback": 6}},
			{"scriptlet failed", []string{"scriptlet_failed"}, nil, map[string]float64{"rollback": 2}},
			{"a %post scriptlet failed (installed, not set up)", []string{"post_scriptlet_failed"}, nil, map[string]float64{"rollback": 4}},
			{"only a %pre scriptlet failed (not installed)", []string{"pre_scriptlet_failed"}, []string{"rpmdb_changed"}, map[string]float64{"investigate": 4}},
			{"packages changed before the failure", []string{"rpmdb_changed"}, nil, map[string]float64{"rollback": 2}},
			{"nothing changed", []string{"rpmdb_unchanged"}, nil, map[string]float64{"investigate": 8}},
		},
	},
	// What holds the space on a full file system?
	"disk.cause": {
		Prior: map[string]float64{"snapshots": 1, "journal": 1, "package_cache": 1, "other_data": 2},
		Rules: []rule{
			{"snapshots hold much exclusive space", []string{"snapshots_large"}, nil, map[string]float64{"snapshots": 20}},
			{"journal is large", []string{"journal_large"}, nil, map[string]float64{"journal": 10}},
			{"package cache is large", []string{"cache_large"}, nil, map[string]float64{"package_cache": 10}},
			{"nothing reclaimable found", nil, []string{"snapshots_large", "journal_large", "cache_large"}, map[string]float64{"other_data": 6}},
		},
	},
	// Should this case be handed to a bigger (opt-in) model? Always false
	// until a model backend exists; kept so the routing seam is logged.
	"route.bigger_model": {
		Prior: map[string]float64{"true": 1, "false": 19},
	},
}

// Questions known to the rules backend.
func KnownQuestions() []string {
	var ids []string
	for id := range ruleSets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Answer implements Backend.
func (Rules) Answer(_ context.Context, q Question) (Answer, error) {
	rs, ok := ruleSets[q.ID]
	if !ok {
		return Answer{}, fmt.Errorf("rules backend: unknown question %q", q.ID)
	}
	p := map[string]float64{}
	for _, o := range q.Options {
		w, ok := rs.Prior[o]
		if !ok {
			return Answer{}, fmt.Errorf("rules backend: option %q not in %s", o, q.ID)
		}
		p[o] = w
	}
	var fired []string
	for _, r := range rs.Rules {
		if !matches(r, q.Features) {
			continue
		}
		fired = append(fired, r.Name)
		for o, f := range r.Factor {
			if _, ok := p[o]; ok {
				p[o] *= f
			}
		}
	}
	a := finish(p, Rules{}.Name())
	a.Rules = fired
	return a, nil
}

func matches(r rule, ft map[string]bool) bool {
	for _, f := range r.All {
		if !ft[f] {
			return false
		}
	}
	for _, f := range r.None {
		if ft[f] {
			return false
		}
	}
	return true
}

// Standard question builders, so options stay consistent across callers.

// UnitCause asks why a unit failed.
func UnitCause(unit string, ft map[string]bool) Question {
	return Question{ID: "unit.cause", Kind: Choice, Subject: unit, Features: ft,
		Prompt:  "Which cause best explains the failure of " + unit + "?",
		Options: []string{"config_error", "selinux_denial", "port_conflict", "dependency_failed", "disk_full", "missing_file", "crashed", "unknown"}}
}

// AVCClass asks what kind of denial this is.
func AVCClass(subject string, ft map[string]bool) Question {
	return Question{ID: "avc.class", Kind: Choice, Subject: subject, Features: ft,
		Prompt:  "Which known class does this SELinux denial belong to?",
		Options: []string{"mislabeled", "missing_fcontext", "port", "boolean", "unknown", "suspicious"}}
}

// Severity rates an event 1 to 5.
func Severity(subject string, ft map[string]bool) Question {
	return Question{ID: "event.severity", Kind: Score, Subject: subject, Features: ft,
		Prompt:  "How severe is this event (1 informational to 5 outage)?",
		Options: []string{"1", "2", "3", "4", "5"}}
}

// Notify asks whether to tell the person now.
func Notify(subject string, ft map[string]bool) Question {
	return Question{ID: "event.notify", Kind: Boolean, Subject: subject, Features: ft,
		Prompt: "Should the administrator be notified now?", Options: []string{"true", "false"}}
}

// DnfNext asks what a failed transaction should lead to.
func DnfNext(subject string, ft map[string]bool) Question {
	return Question{ID: "dnf.next", Kind: Choice, Subject: subject, Features: ft,
		Prompt: "Should the failed transaction be rolled back to its pre snapshot or investigated first?", Options: []string{"rollback", "investigate"}}
}

// DiskCause asks what holds the space.
func DiskCause(subject string, ft map[string]bool) Question {
	return Question{ID: "disk.cause", Kind: Choice, Subject: subject, Features: ft,
		Prompt: "What holds most of the reclaimable space?", Options: []string{"snapshots", "journal", "package_cache", "other_data"}}
}

// RouteBigger asks whether a bigger model should take over.
func RouteBigger(subject string, ft map[string]bool) Question {
	return Question{ID: "route.bigger_model", Kind: Boolean, Subject: subject, Features: ft,
		Prompt: "Does this case need the bigger, opt-in model?", Options: []string{"true", "false"}}
}

// SeverityFeatures turns a severity answer into notify features.
func SeverityFeatures(a Answer) map[string]bool {
	e := a.Expected()
	return map[string]bool{"severity_high": e >= 3.5, "severity_low": e < 2.5}
}

// FormatProb renders a probability.
func FormatProb(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// JournalFacts is the evidence a model backend sees with a question: the
// last journal lines of the case, each cut to 300 bytes (core dumps carry
// long stack traces). The rules backend ignores facts.
func JournalFacts(lines []string) map[string]any {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if i := strings.IndexByte(l, '\n'); i >= 0 {
			l = l[:i]
		}
		if len(l) > 300 {
			l = l[:300]
		}
		out = append(out, l)
	}
	return map[string]any{"journal": out}
}
