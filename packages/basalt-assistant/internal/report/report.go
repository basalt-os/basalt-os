// Package report turns diagnoses into proposals and renders them as plain,
// friendly English from templates (render.go): what is wrong, why, what
// applying will do, the risk, how to undo it and the next step, with the
// exact commands in their own clearly marked block. Each proposal also
// carries the structured facts its text is written from (package
// explain), which the optional humanize layer gives to a model. No
// language model is needed.
package report

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

func base(source, kind, subject, key string) *proposal.Proposal {
	now := time.Now().UTC()
	return &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1,
		Source: source, Kind: kind, Subject: subject, Key: key, Status: proposal.Pending}
}

// needsReview: below threshold, or nothing to apply.
func finish(p *proposal.Proposal, ds ...decide.Decision) *proposal.Proposal {
	p.Decisions = append(p.Decisions, ds...)
	for _, d := range ds {
		if !d.Confident {
			p.NeedsReview = true
		}
	}
	if len(p.Actions) == 0 {
		p.NeedsReview = true
	}
	return p
}

// Short names of causes, for titles and listings.
var unitCauses = map[string]string{
	"config_error": "configuration error", "selinux_denial": "blocked by SELinux", "port_conflict": "port already in use",
	"dependency_failed": "a unit it needs failed", "disk_full": "disk full", "missing_file": "missing file",
	"crashed": "crashed", "crash_loop": "keeps crashing", "oom": "out of memory", "permission": "file permissions", "not_found": "no such unit",
	"unknown": "cause unknown",
}

var avcClasses = map[string]string{
	selinux.ClassMislabeled: "wrong file label", selinux.ClassMissingFcontext: "no label rule for this place",
	selinux.ClassPort: "port not labeled for it", selinux.ClassBoolean: "a boolean is off",
	selinux.ClassUnknown: "no known fix", selinux.ClassSuspicious: "looks deliberate",
}

var diskCauses = map[string]string{"snapshots": "snapshots hold the space", "journal": "large journal",
	"package_cache": "large package cache", "other_data": "data"}

func phrase(m map[string]string, k string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return strings.ReplaceAll(k, "_", " ")
}

var (
	reTimePrefix = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}(?:\.\d+)?\s+`)
	reLogPrefix  = regexp.MustCompile(`^[A-Za-z0-9_.@-]+(?:\[\d+\])?:\s*\[\w+\]\s*`)
	reSystemdMsg = regexp.MustCompile(`Failed with result|Failed to start|Main process exited|Scheduled restart job|Start request repeated|Stopped |Starting |Deactivated successfully|Consumed .* CPU time`)
)

// clean strips the time and the program's level prefix from a log line
// and shortens it.
func clean(l string) string {
	l = strings.TrimSpace(reTimePrefix.ReplaceAllString(strings.TrimSpace(l), ""))
	l = reLogPrefix.ReplaceAllString(l, "")
	l = strings.TrimRight(l, ". ")
	if len(l) > 160 {
		// Cut at a word, without an ellipsis.
		l = l[:160]
		if i := strings.LastIndexByte(l, ' '); i > 80 {
			l = l[:i]
		}
	}
	return strings.TrimRight(l, ",;: ")
}

// relevantLine is the first journal line that is not systemd's own
// bookkeeping.
func relevantLine(lines []string) string {
	for _, l := range lines {
		if !reSystemdMsg.MatchString(l) && strings.TrimSpace(l) != "" {
			return clean(l)
		}
	}
	return ""
}

func checkerLine(out, file string) string {
	var first string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		low := strings.ToLower(l)
		if (file != "" && strings.Contains(l, file)) || strings.Contains(low, "error") || strings.Contains(low, "emerg") ||
			strings.Contains(low, "unknown") || strings.Contains(low, "invalid") {
			return clean(l)
		}
		if first == "" {
			first = clean(l)
		}
	}
	return first
}

func pct2(f float64) string { return fmt.Sprintf("%.2f", f) }

// UnitFacts are the facts of a unit diagnosis.
func UnitFacts(r *diag.UnitReport) *explain.Facts {
	cause := r.Cause
	switch {
	case r.State["LoadState"] == "not-found":
		cause = "not_found"
	case cause == "crashed" && r.Features["oom_killed"]:
		cause = "oom"
	case cause == "crashed" && r.Crash != nil && r.Crash.Repeating:
		cause = "crash_loop"
	case cause == "unknown" && r.Features["dac_denied"] && len(r.DAC) > 0 && !r.Healthy:
		cause = "permission"
	}
	f := explain.New("unit", cause, r.Unit)
	f.OK = r.Healthy && cause != "not_found"
	switch cause {
	case "config_error":
		f.Set("file", r.ConfigFile).SetInt("line", r.ConfigLine)
		if c := r.ConfigCheck; c != nil && !c.OK {
			f.Set("checker", c.Command).Set("checker_msg", checkerLine(c.Output, r.ConfigFile))
		}
		if !f.Has("checker_msg") {
			f.Set("journal", relevantLine(r.Journal))
		}
		if rc := r.Restore; rc != nil {
			f.SetInt("snapshot", rc.Snapshot).Set("snapshot_date", rc.Date).Set("restore_path", rc.Path)
		}
		acts := append([]action.Action{}, r.Actions...)
		for _, h := range r.Hints {
			acts = append(acts, h.Actions...)
		}
		for _, a := range acts {
			if a.Kind == action.FileRestore && !f.Has("snapshot") {
				f.Set("snapshot", a.Params["snapshot"]).Set("restore_path", a.Params["path"])
			}
		}
	case "selinux_denial":
		f.Set("domain", r.Domain)
		if len(r.AVCs) > 0 {
			avcFacts(f, r.AVCs[0])
			if len(r.AVCs) > 1 {
				f.SetInt("denials", len(r.AVCs))
			}
		}
	case "port_conflict":
		for _, p := range r.Ports {
			f.SetInt("port", p.Port).Set("port_owner", p.Owner)
			if p.Owner != "" {
				break
			}
		}
	case "dependency_failed":
		for _, d := range r.Deps {
			f.Set("dep", d.Unit).Set("dep_state", d.Active)
			break
		}
	case "disk_full":
		for _, d := range r.Disks {
			f.Set("fs_path", d.Path).Set("fs_pct", fmt.Sprintf("%.0f", d.UsedPct()))
			break
		}
	case "missing_file", "unknown":
		f.Set("journal", relevantLine(r.Journal))
	case "oom":
		if o := r.OOM; o != nil {
			if m, ok := o.Limits["MemoryMax"]; ok && m != "infinity" && m != "" {
				f.Set("memory_max", diag.MemValue(m))
			}
			switch o.Constraint {
			case "CONSTRAINT_MEMCG":
				f.Set("oom_scope", "cgroup")
			case "CONSTRAINT_NONE":
				f.Set("oom_scope", "system")
			}
		}
	case "crashed":
		f.Set("result", r.State["Result"])
		if c := r.Crash; c != nil {
			f.Set("ran_for", c.RanFor)
		}
	case "crash_loop":
		f.Set("result", r.State["Result"])
		if c := r.Crash; c != nil {
			f.Set("exit", strings.TrimSpace(c.Code+" "+c.Status)).Set("ran_for", c.RanFor).Set("window", c.Window).
				SetInt("restarts", c.Restarts)
			if c.Count >= 2 {
				f.SetInt("crashes", c.Count)
			}
			if c.StartLimit {
				f.Set("start_limit", "yes")
			}
			if c.RanSeconds >= 0 && c.RanSeconds < int64(diag.QuickCrash.Seconds()) {
				f.Set("at_start", "yes")
			}
			if c.Code == "dumped" {
				f.Set("core", "yes")
			}
		}
	case "permission":
		d := r.DAC[0]
		f.Set("dac_path", d.Path).Set("dac_mode", d.Mode).Set("dac_owner", d.Owner).Set("dac_user", d.User).Set("dac_need", d.Need)
	}
	f.Hint = len(r.Hints) > 0
	if len(r.Errors) > 0 {
		f.Incomplete = true
		f.Set("error", r.Errors[0])
	}
	if !r.Decision.Confident && !r.Healthy {
		f.Review = true
		f.Set("confidence", pct2(r.Decision.Answer.Confidence)).Set("threshold", pct2(r.Decision.Threshold))
	}
	for _, d := range r.AVCDecision {
		if !d.Confident {
			f.Review = true
		}
	}
	return f
}

func avcFacts(f *explain.Facts, fx selinux.Fix) {
	a := fx.Group.AVC
	if !f.Has("domain") {
		f.Set("domain", a.SType())
	}
	f.Set("perm", strings.Join(a.Perms, " ")).Set("object", objectOf(fx)).Set("target_type", a.TType())
	if fx.Group.Count > 1 {
		f.SetInt("count", fx.Group.Count)
	}
	f.Set("path", fx.Path).Set("default_type", fx.DefaultType)
	if a.IsPort() {
		f.SetInt("port", a.Port).Set("proto", a.Proto()).Set("port_type_now", fx.Facts["port_type_now"])
	}
	for _, act := range fx.Actions {
		switch act.Kind {
		case action.SELinuxBoolean:
			f.Set("boolean", act.Params["name"])
		case action.SELinuxFcontext:
			f.Set("want_type", act.Params["type"])
		}
	}
}

// FromUnit builds a proposal from a unit diagnosis.
func FromUnit(source string, r *diag.UnitReport) *proposal.Proposal {
	p := base(source, "unit", r.Unit, "unit:"+r.Unit+":"+r.Cause)
	p.Facts = UnitFacts(r)
	p.Title = r.Unit + " stopped: " + phrase(unitCauses, p.Facts.Cause)
	if r.Healthy {
		p.Title = r.Unit + " is running"
	}
	p.Report = r.Explanation
	p.Evidence = append(p.Evidence, r.Evidence...)
	for _, l := range r.Journal {
		p.Evidence = append(p.Evidence, "journal: "+l)
	}
	if r.ConfigCheck != nil {
		p.Evidence = append(p.Evidence, fmt.Sprintf("config check `%s`: %s", r.ConfigCheck.Command, okFail(r.ConfigCheck.OK)))
		for _, l := range strings.Split(r.ConfigCheck.Output, "\n") {
			if strings.TrimSpace(l) != "" {
				p.Evidence = append(p.Evidence, "  "+l)
			}
		}
	}
	for _, f := range r.AVCs {
		p.Evidence = append(p.Evidence, f.Evidence...)
	}
	for _, d := range r.Deps {
		p.Evidence = append(p.Evidence, fmt.Sprintf("dependency %s: %s (%s, load %s)", d.Unit, d.Active, d.Result, d.Load))
	}
	for _, pi := range r.Ports {
		p.Evidence = append(p.Evidence, fmt.Sprintf("port %d: %s", pi.Port, orDash(pi.Owner)))
	}
	for _, d := range r.Disks {
		p.Evidence = append(p.Evidence, fmt.Sprintf("%s is %.0f %% full", d.Path, d.UsedPct()))
	}
	if r.Restore != nil && r.Restore.Diff != "" {
		p.Evidence = append(p.Evidence, "difference from snapshot "+strconv.Itoa(r.Restore.Snapshot)+":")
		for _, l := range strings.Split(r.Restore.Diff, "\n") {
			p.Evidence = append(p.Evidence, "  "+l)
		}
	}
	for _, s := range r.Skipped {
		p.Evidence = append(p.Evidence, "not checked here: "+s)
	}
	for _, e := range r.Errors {
		p.Evidence = append(p.Evidence, "error: "+e)
	}
	p.Actions = r.Actions
	p.Hints = r.Hints
	p.Severity = 4
	if r.Healthy {
		p.Severity = 1
	}
	p = finish(p, append([]decide.Decision{r.Decision}, r.AVCDecision...)...)
	if len(r.Errors) > 0 {
		p.NeedsReview = true
	}
	return p
}

// SELinuxFacts are the facts of one denial group.
func SELinuxFacts(it diag.SELinuxItem) *explain.Facts {
	a := it.Fix.Group.AVC
	f := explain.New("selinux", it.Decision.Answer.Top, a.SType())
	avcFacts(f, it.Fix)
	f.Review = !it.Decision.Confident || f.Cause == selinux.ClassUnknown || f.Cause == selinux.ClassSuspicious
	if !it.Decision.Confident {
		f.Set("confidence", pct2(it.Decision.Answer.Confidence)).Set("threshold", pct2(it.Decision.Threshold))
	}
	if len(it.Fix.Errors) > 0 {
		f.Incomplete = true
		f.Set("error", it.Fix.Errors[0])
	}
	return f
}

// FromSELinux builds a proposal from one denial group.
func FromSELinux(source string, it diag.SELinuxItem) *proposal.Proposal {
	a := it.Fix.Group.AVC
	p := base(source, "selinux", a.SType()+" -> "+a.TType(), "avc:"+a.Key())
	p.Facts = SELinuxFacts(it)
	p.Title = fmt.Sprintf("SELinux blocked %s (%s) on %s: %s", a.SType(), strings.Join(a.Perms, " "),
		objectOf(it.Fix), phrase(avcClasses, it.Decision.Answer.Top))
	p.Report = it.Fix.Explanation + "."
	switch it.Decision.Answer.Top {
	case selinux.ClassSuspicious:
		p.Report += " The target is security sensitive: this looks like the policy doing its job. No change is proposed; find out why the program tried this."
	case selinux.ClassUnknown:
		p.Report += " No known fix; a local policy module is not generated automatically."
	}
	p.Evidence = append(p.Evidence, it.Fix.Evidence...)
	p.Evidence = append(p.Evidence, "raw: "+a.Raw)
	if it.Decision.Confident && it.Decision.Answer.Top != selinux.ClassUnknown && it.Decision.Answer.Top != selinux.ClassSuspicious &&
		len(it.Fix.Errors) == 0 {
		p.Actions = it.Fix.Actions
	}
	p.Severity = 3
	return finish(p, it.Decision)
}

func objectOf(f selinux.Fix) string {
	a := f.Group.AVC
	if f.Path != "" {
		return f.Path
	}
	if a.IsPort() {
		return a.Proto() + " port " + strconv.Itoa(a.Port)
	}
	if a.Name != "" {
		return a.Name
	}
	return a.Class
}

// DiskFacts are the facts of a disk report.
func DiskFacts(r *diag.DiskReport) *explain.Facts {
	f := explain.New("disk", r.Decision.Answer.Top, "")
	f.Set("used_pct", fmt.Sprintf("%.0f", r.UsedPct))
	f.OK = !r.Features["disk_warn"] && !r.Features["disk_crit"]
	if r.Features["disk_crit"] {
		f.Set("level", "critical")
	}
	for _, act := range r.Actions {
		if act.Kind != action.SnapshotDelete {
			continue
		}
		for _, s := range r.Snapshots {
			if strconv.Itoa(s.Snapshot.Number) == act.Params["snapshot"] {
				f.SetInt("snapshot", s.Snapshot.Number).Set("snapshot_date", s.Snapshot.Date).
					Set("snapshot_desc", s.Snapshot.Description).Set("snapshot_size", diag.HumanBytes(s.Exclusive))
			}
		}
	}
	if r.SnapshotTotal > 0 {
		f.Set("snapshots_size", diag.HumanBytes(r.SnapshotTotal))
	}
	switch f.Cause {
	case "journal":
		f.Set("journal_size", diag.HumanBytes(r.Journal))
	case "package_cache":
		f.Set("cache_size", diag.HumanBytes(r.PkgCache))
	}
	if r.Forecast.DaysToFull > 0 {
		f.Set("days_to_full", fmt.Sprintf("%.0f", math.Max(1, math.Round(r.Forecast.DaysToFull))))
	}
	if len(r.Skipped) > 0 {
		f.Set("confined", "yes")
	}
	if !f.OK && len(r.Actions) == 0 && f.Cause != "other_data" {
		f.Set("nothing_large", "yes")
	}
	f.Review = !r.Decision.Confident && !f.OK
	return f
}

// FromDisk builds a proposal from a disk report.
func FromDisk(source string, r *diag.DiskReport) *proposal.Proposal {
	p := base(source, "disk", "/", "disk:/")
	p.Facts = DiskFacts(r)
	p.Title = fmt.Sprintf("root file system %.0f %% full: %s", r.UsedPct, phrase(diskCauses, r.Decision.Answer.Top))
	if p.Facts.OK {
		p.Title = fmt.Sprintf("root file system %.0f %% full", r.UsedPct)
	}
	p.Report = r.Explanation
	p.Evidence = append(p.Evidence, r.Evidence...)
	for i, s := range r.Snapshots {
		if i >= 5 {
			break
		}
		p.Evidence = append(p.Evidence, fmt.Sprintf("snapshot %d (%s %q) holds %s exclusively", s.Snapshot.Number, s.Snapshot.Date, s.Snapshot.Description, diag.HumanBytes(s.Exclusive)))
	}
	for _, s := range r.Skipped {
		p.Evidence = append(p.Evidence, "not checked here: "+s)
	}
	p.Actions = r.Actions
	p.Severity = 3
	if r.Features["disk_crit"] {
		p.Severity = 5
	}
	if p.Facts.OK {
		p.Severity = 1
	}
	return finish(p, r.Decision)
}

// DnfFacts are the facts of a failed package transaction.
func DnfFacts(r *diag.DnfReport) *explain.Facts {
	cause := "investigate"
	if len(r.Actions) > 0 || len(r.Hints) > 0 {
		cause = "rollback"
	}
	f := explain.New("dnf", cause, "")
	f.OK = r.Pre == nil && len(r.Failed) == 0
	f.Set("command", r.Command).Set("failed", strings.Join(r.Failed, ", "))
	if r.Pre != nil {
		f.SetInt("snapshot", r.Pre.Number)
	}
	if pd := r.Packages; pd.Error == "" && (len(pd.Added)+len(pd.Removed)+len(pd.Changed) > 0) {
		f.Set("added", strconv.Itoa(len(pd.Added))).Set("removed", strconv.Itoa(len(pd.Removed))).Set("changed", strconv.Itoa(len(pd.Changed)))
	}
	f.Hint = len(r.Hints) > 0
	f.Review = !r.Decision.Confident && !f.OK
	return f
}

// FromDnf builds a proposal from a failed transaction.
func FromDnf(source string, r *diag.DnfReport) *proposal.Proposal {
	key := "dnf:"
	if r.Pre != nil {
		key += strconv.Itoa(r.Pre.Number)
	}
	p := base(source, "dnf", r.Command, key)
	p.Facts = DnfFacts(r)
	p.Title = fmt.Sprintf("package transaction failed: %s", orDash(r.Command))
	p.Report = r.Explanation
	p.Evidence = append(p.Evidence, r.Evidence...)
	for _, l := range r.LogLines {
		p.Evidence = append(p.Evidence, "dnf5.log: "+l)
	}
	for i, x := range r.Packages.Changed {
		if i >= 10 {
			p.Evidence = append(p.Evidence, fmt.Sprintf("(%d more changed)", len(r.Packages.Changed)-10))
			break
		}
		p.Evidence = append(p.Evidence, "changed: "+x)
	}
	for i, x := range r.Packages.Added {
		if i >= 10 {
			break
		}
		p.Evidence = append(p.Evidence, "added: "+x)
	}
	p.Actions = r.Actions
	p.Hints = r.Hints
	p.Severity = 4
	if p.Facts.OK {
		p.Severity = 1
	}
	return finish(p, r.Decision)
}

// RollbackFacts are the facts of a rollback plan; before is the proposal
// it undoes, if any.
func RollbackFacts(r *diag.RollbackPlan, before string) *explain.Facts {
	f := explain.New("snapshot", "rollback", "")
	f.SetInt("snapshot", r.Target.Number).Set("snapshot_date", r.Target.Date).Set("snapshot_desc", r.Target.Description).Set("before", before)
	if pd := r.Packages; pd.Error == "" {
		f.Set("added", strconv.Itoa(len(pd.Added))).Set("removed", strconv.Itoa(len(pd.Removed))).Set("changed", strconv.Itoa(len(pd.Changed)))
	}
	if r.Files.Skipped == "" {
		f.Set("files", strconv.Itoa(r.Files.Total))
	}
	return f
}

// FromRollback builds a proposal to roll back to a snapshot; before is the
// proposal it undoes ("" when requested directly).
func FromRollback(source string, r *diag.RollbackPlan, why, before string) *proposal.Proposal {
	p := base(source, "snapshot", strconv.Itoa(r.Target.Number), "rollback:"+strconv.Itoa(r.Target.Number))
	p.Facts = RollbackFacts(r, before)
	p.Title = fmt.Sprintf("roll back to snapshot %d (%s, %q)", r.Target.Number, r.Target.Date, r.Target.Description)
	p.Report = why
	pd := r.Packages
	if pd.Error != "" {
		p.Evidence = append(p.Evidence, "packages: "+pd.Error)
	} else {
		p.Evidence = append(p.Evidence, fmt.Sprintf("packages after rollback: %d added, %d removed, %d changed (relative to now)", len(pd.Added), len(pd.Removed), len(pd.Changed)))
		for _, l := range limit(pd.Changed, 15) {
			p.Evidence = append(p.Evidence, "  "+l)
		}
		for _, l := range limit(pd.Removed, 10) {
			p.Evidence = append(p.Evidence, "  removed: "+l)
		}
		for _, l := range limit(pd.Added, 10) {
			p.Evidence = append(p.Evidence, "  back: "+l)
		}
	}
	if r.Files.Skipped != "" {
		p.Evidence = append(p.Evidence, "files: "+r.Files.Skipped)
	} else {
		p.Evidence = append(p.Evidence, fmt.Sprintf("files that differ: %d", r.Files.Total))
		for _, l := range r.Files.Etc {
			p.Evidence = append(p.Evidence, "  "+l)
		}
	}
	p.Evidence = append(p.Evidence, r.Notes...)
	p.Actions = r.Actions
	p.Severity = 3
	return p
}

func limit(xs []string, n int) []string {
	if len(xs) > n {
		return append(xs[:n:n], fmt.Sprintf("(%d more)", len(xs)-n))
	}
	return xs
}

func okFail(ok bool) string {
	if ok {
		return "passed"
	}
	return "FAILED"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
