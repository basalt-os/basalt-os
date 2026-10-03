package diag

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/journal"
)

// DnfReport diagnoses a failed package transaction.
type DnfReport struct {
	Pre         *Snapshot       `json:"pre,omitempty"` // pre snapshot without a post
	Command     string          `json:"command"`
	Failed      []string        `json:"failed_packages,omitempty"`
	LogLines    []string        `json:"log_lines,omitempty"`
	Packages    PackageDiff     `json:"packages"`
	Features    map[string]bool `json:"features"`
	Decision    decide.Decision `json:"decision"`
	Explanation string          `json:"explanation"`
	Evidence    []string        `json:"evidence"`
	Actions     []action.Action `json:"actions,omitempty"`
}

// DnfLog is dnf5's log file.
var DnfLog = "/var/log/dnf5.log"

var reDnfErr = regexp.MustCompile(`(?i)\b(ERROR|CRITICAL)\b|scriptlet failed|transaction failed|error: `)

// DnfLogFailure reports whether a dnf5.log line records a failure worth
// a diagnosis (an rpm scriptlet failure is only a WARNING for %post), and
// its time.
func DnfLogFailure(line string) (time.Time, bool) {
	if len(line) < 25 {
		return time.Time{}, false
	}
	if !strings.Contains(line, "scriptlet failed") && !strings.Contains(line, "Transaction failed") &&
		!(strings.Contains(line, " ERROR [rpm]") && strings.Contains(line, "failed")) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02T15:04:05-0700", line[:24])
	return t, err == nil
}

// DiagnoseDnf explains a failed package transaction. pre selects the pre
// snapshot taken before it (0: an unfinished transaction, a pre snapshot
// without its post, else the newest dnf pre snapshot taken before at).
// rpm-plugin-audit's SOFTWARE_UPDATE records with res=failed and dnf5's
// log give the failed packages and messages.
func (e *Env) DiagnoseDnf(ctx context.Context, pre int, at time.Time) *DnfReport {
	r := &DnfReport{Features: map[string]bool{}}
	snaps := e.Snapshots(ctx)
	hasPost := map[int]bool{}
	for _, s := range snaps {
		if s.Type == "post" {
			hasPost[s.Pre] = true
		}
	}
	if at.IsZero() {
		at = e.now()
	}
	for i := range snaps {
		s := snaps[i]
		if s.Type != "pre" || s.Userdata["basalt"] != "dnf" {
			continue
		}
		switch {
		case pre != 0 && s.Number == pre:
			r.Pre = &s
		case pre == 0 && !s.Time.After(at.Add(2*time.Second)):
			r.Pre = &s // newest one before the failure wins (list is oldest first)
		}
	}
	since := at.Add(-1 * time.Hour)
	until := at.Add(30 * time.Second)
	if r.Pre != nil {
		r.Command = r.Pre.Description
		since = r.Pre.Time.Add(-1 * time.Second)
		for _, s := range snaps {
			if s.Type == "post" && s.Pre == r.Pre.Number && !s.Time.IsZero() {
				// Only this transaction: up to its post snapshot.
				until = s.Time.Add(2 * time.Second)
			}
		}
		if !hasPost[r.Pre.Number] {
			r.Features["pre_without_post"] = true
			r.Evidence = append(r.Evidence, fmt.Sprintf("snapshot %d (pre, %s, %q) has no post snapshot: the transaction did not finish",
				r.Pre.Number, r.Pre.Date, r.Pre.Description))
		} else {
			r.Evidence = append(r.Evidence, fmt.Sprintf("snapshot %d (pre, %s) was taken just before %q", r.Pre.Number, r.Pre.Date, r.Pre.Description))
		}
	}

	// rpm-plugin-audit writes one SOFTWARE_UPDATE record per package: a
	// successful one means the package database changed in this
	// transaction, a failed one names the package that failed.
	res := e.R.Read(ctx, "journalctl", "--no-pager", "-o", "json", "_TRANSPORT=audit", "--since", "@"+strconv.FormatInt(since.Unix(), 10))
	ok := 0
	for _, en := range journal.ParseAll(res.Out) {
		if en.AuditType != journal.AuditSoftwareUpdate || (!en.Time.IsZero() && en.Time.After(until)) {
			continue
		}
		if sw, failed := en.SoftwareUpdateFailed(); failed {
			r.Failed = append(r.Failed, sw)
		} else if strings.Contains(en.Message, "res=success") {
			ok++
		}
	}
	if len(r.Failed) > 0 {
		r.Features["scriptlet_failed"] = true
		r.Evidence = append(r.Evidence, "rpm reported failures for: "+strings.Join(r.Failed, ", "))
	}
	if ok > 0 {
		r.Features["rpmdb_changed"] = true
		r.Evidence = append(r.Evidence, fmt.Sprintf("rpm recorded %d successful package operation(s) in this transaction", ok))
	}

	if b, err := e.ReadFile(DnfLog); err == nil {
		lines := strings.Split(string(b), "\n")
		start := max(len(lines)-4000, 0)
		for _, l := range lines[start:] {
			if len(l) < 25 || !reDnfErr.MatchString(l) {
				continue
			}
			if t, err := time.Parse("2006-01-02T15:04:05-0700", l[:24]); err == nil && (t.Before(since) || t.After(until)) {
				continue
			}
			r.LogLines = append(r.LogLines, strings.TrimSpace(l))
		}
		if len(r.LogLines) > 15 {
			r.LogLines = r.LogLines[len(r.LogLines)-15:]
		}
		for _, l := range r.LogLines {
			switch {
			case strings.Contains(l, "%prein(") || strings.Contains(l, "%pretrans("):
				// The package was not installed: nothing changed for it.
				r.Features["pre_scriptlet_failed"] = true
			case strings.Contains(l, "%post(") || strings.Contains(l, "%posttrans(") || strings.Contains(l, "%triggerin("):
				// The files are in place but setup did not finish.
				r.Features["post_scriptlet_failed"] = true
				r.Features["rpmdb_changed"] = true
			}
		}
	}

	if r.Pre != nil && !e.Confined {
		r.Packages = e.DiffPackages(ctx, r.Pre.Number, 0)
		if r.Packages.Error == "" {
			n := len(r.Packages.Added) + len(r.Packages.Removed) + len(r.Packages.Changed)
			if n > 0 {
				r.Features["rpmdb_changed"] = true
				r.Evidence = append(r.Evidence, fmt.Sprintf("since snapshot %d: %d packages added, %d removed, %d changed",
					r.Pre.Number, len(r.Packages.Added), len(r.Packages.Removed), len(r.Packages.Changed)))
			} else if !r.Features["rpmdb_changed"] {
				r.Features["rpmdb_unchanged"] = true
				r.Evidence = append(r.Evidence, fmt.Sprintf("the package set is the same as in snapshot %d", r.Pre.Number))
			}
		}
	}

	r.Decision = e.ask(ctx, decide.DnfNext(r.Command, r.Features))
	switch {
	case r.Pre == nil && len(r.Failed) == 0:
		r.Explanation = "No failed package transaction found."
	case r.Decision.Answer.Top == "rollback" && r.Pre != nil:
		r.Explanation = fmt.Sprintf("The transaction %q failed after changing packages; rolling the root back to snapshot %d (taken just before it) is proposed.",
			r.Command, r.Pre.Number)
		r.Actions = []action.Action{{Kind: action.SnapshotRollback, Params: map[string]string{"snapshot": strconv.Itoa(r.Pre.Number)}}}
	default:
		r.Explanation = fmt.Sprintf("The transaction %q failed; the package set did not change, so nothing needs rolling back. Read the log lines, fix the cause and retry.", r.Command)
	}
	return r
}
