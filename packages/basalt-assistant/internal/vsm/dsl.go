// Package vsm is the assistant's VSM backend (ADR 0004): goals become
// diagnoses and typed actions through a loop of deterministic tools, a
// knowledge index lookup and a small learned planner.
//
//   - The knowledge index ranks candidate cases for the evidence
//     (internal/knowledge).
//   - The planner (internal/vsm/planner) judges: it calls tools, looks the
//     evidence up, then accepts a candidate, picks another or abstains.
//   - A deterministic guard outside the model refuses a pick the evidence
//     does not fully support (a required code missing or a contradiction):
//     the pick becomes "unknown, here is the evidence".
//   - The binder instantiates the picked case's typed actions from the
//     bindings the tools extracted and checks them with the action
//     validators; from the confined view a file restore or a snapshot
//     rollback stays a hint until the root command line has confirmed the
//     snapshot (ADR 0008).
//
// The planner never sees names: a goal is a verb, the evidence two-letter
// feature codes, the candidates match statistics. Units, paths, ports,
// types and snapshot numbers are bindings, so an unseen service cannot
// change what it does.
package vsm

import (
	"sort"
	"strings"
)

// DSLVersion this package speaks: candidates shown in rank order (2) and
// the code dm for a file-permission (DAC) finding, om tied to oom_killed
// (3). A planner and a knowledge index must speak the same version.
const DSLVersion = "basalt-os-dsl/3"

// Planner goals and the decision-layer question each one answers.
const (
	GoalDiagnose = "diagnose"           // basalt why UNIT -> unit.cause
	GoalDenial   = "explain_denial"     // basalt fix selinux -> avc.class
	GoalDisk     = "check_disk"         // basalt disk -> disk.cause
	GoalRollback = "rollback_candidate" // failed package transaction -> dnf.next
	maxActions   = 16
	pickChars    = "012"
	lookupChar   = 'K'
	abstainChar  = '?'
	endChar      = ';'
	toolLetters  = "UJALCPDT"
)

// QuestionOfGoal maps a goal to its decision-layer question.
var QuestionOfGoal = map[string]string{
	GoalDiagnose: "unit.cause", GoalDenial: "avc.class", GoalDisk: "disk.cause", GoalRollback: "dnf.next",
}

// GoalOfQuestion is the inverse.
var GoalOfQuestion = map[string]string{
	"unit.cause": GoalDiagnose, "avc.class": GoalDenial, "disk.cause": GoalDisk, "dnf.next": GoalRollback,
}

// Options of each question, exactly as the decision layer builds them.
var Options = map[string][]string{
	"unit.cause": {"config_error", "selinux_denial", "port_conflict", "dependency_failed", "disk_full", "missing_file", "crashed", "unknown"},
	"avc.class":  {"mislabeled", "missing_fcontext", "port", "boolean", "unknown", "suspicious"},
	"dnf.next":   {"rollback", "investigate"},
	"disk.cause": {"snapshots", "journal", "package_cache", "other_data"},
}

// Cautious is what an abstention means for each question.
var Cautious = map[string]string{
	"unit.cause": "unknown", "avc.class": "unknown", "dnf.next": "investigate", "disk.cause": "other_data",
}

// Features maps the diagnosers' feature names to the planner's codes (the
// table of VSM's DSL). Codes the DSL introduced without an assistant
// feature (to, om, sc, xs, xj, xk) come from the journal reader and the
// tools.
var Features = map[string]string{
	"unit_failed": "uf", "signal_or_core": "sg",
	"journal_config_error": "ce", "journal_config_location": "cl", "journal_permission_denied": "pd",
	"journal_address_in_use": "au", "journal_no_space": "ns", "journal_no_such_file": "nf", "dependency_failed": "dp",
	"config_check_failed": "cf", "config_check_passed": "cp", "config_check_runtime": "cr",
	"avc_for_domain": "av", "port_case": "pc", "port_type_found": "pt", "port_owned_by_other": "pb",
	"boolean_off": "bo", "sensitive_target": "st", "permissive": "pv", "unsupported_class": "uc",
	"path_label_problem": "lp", "path_mislabeled": "lm", "dac_denied": "dm",
	"default_differs": "dd", "default_allowed": "da",
	"generic_target": "gt", "candidate_type": "ct", "has_path": "hp", "no_avc": "nv",
	"port_owner_found": "po",
	"oom_killed":       "om",
	"disk_full":        "fs", "disk_warn": "dw", "disk_crit": "dc", "snapshots_large": "sl", "journal_large": "jl",
	"cache_large":      "kl",
	"pre_without_post": "pp", "scriptlet_failed": "sf", "post_scriptlet_failed": "ps", "pre_scriptlet_failed": "pr",
	"rpmdb_changed": "rc", "rpmdb_unchanged": "ru",
}

// Extensions are assistant features newer than the planner's DSL table,
// mapped onto existing codes (part of the adapter, not of the reference:
// the parity tests turn them off). Empty in DSL 3: oom_killed, the
// extension of DSL 2, is in the table.
var Extensions = map[string]string{}

// NotCodes are the diagnosers' features deliberately left out of the DSL
// (audit of DSL 3, the same table as VSM's dsl.NOT_CODES), with the reason.
var NotCodes = map[string]string{
	"policy_query_failed": "not evidence about the fault: a deterministic guard answers the cautious option",
	"resolved":            "internal to the SELinux analysis; resolved findings are dropped before the question",
	"avc":                 "event.severity input (rules only)",
	"enforcing":           "event.severity input (rules only)",
	"suspicious":          "event.severity input (rules only)",
	"dnf_failed":          "event.severity input (rules only)",
	"severity_high":       "event.notify input (rules only)",
	"severity_low":        "event.notify input (rules only)",
	"repeat":              "event.notify input (rules only)",
}

// Code groups by the tool that produces them.
var (
	unitCodes    = set("uf", "sg")
	journalCodes = set("ce", "cl", "pd", "au", "ns", "nf", "dp", "to", "om")
	configCodes  = set("cf", "cp", "cr")
	labelCodes   = set("lp", "lm", "dm")
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// CodesOf turns diagnoser features into codes (with the extensions when
// ext is set).
func CodesOf(features map[string]bool, ext bool) map[string]bool {
	out := map[string]bool{}
	for name, on := range features {
		if !on {
			continue
		}
		if c, ok := Features[name]; ok {
			out[c] = true
		} else if c, ok := Extensions[name]; ok && ext {
			out[c] = true
		}
	}
	return out
}

func sortedCodes(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for c, on := range m {
		if on {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func fmtCodes(m map[string]bool) string {
	s := sortedCodes(m)
	if len(s) == 0 {
		return "-"
	}
	return strings.Join(s, " ")
}

func prompt(goal string) string { return "os " + goal + " |" }
