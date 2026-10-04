package diag

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// SELinuxItem is one denial group with its fix and decision.
type SELinuxItem struct {
	Fix      selinux.Fix     `json:"fix"`
	Decision decide.Decision `json:"decision"`
}

// FixSELinux analyzes the denials since a time.
func (e *Env) FixSELinux(ctx context.Context, since time.Time) []SELinuxItem {
	var out []SELinuxItem
	for _, g := range selinux.GroupAVCs(e.CollectAVCs(ctx, since)) {
		f := e.AnalyzeAVC(ctx, g)
		d := e.ask(ctx, decide.AVCClass(g.AVC.Key(), f.Features))
		out = append(out, SELinuxItem{Fix: f, Decision: d})
	}
	return out
}

// Status is the health summary.
type Status struct {
	OS            OSRelease `json:"os"`
	SELinux       string    `json:"selinux"`
	FailedUnits   []string  `json:"failed_units"`
	Denials24h    int       `json:"denials_24h"`
	Disk          FSStat    `json:"disk"`
	DiskPct       float64   `json:"disk_pct"`
	Snapshots     int       `json:"snapshots"`
	LastSnapshot  string    `json:"last_snapshot"`
	OrphanPre     []int     `json:"unfinished_transactions,omitempty"`
	RollbackState string    `json:"rollback_state"`
	Daemon        string    `json:"daemon"`
	Pending       int       `json:"pending"`
	Problems      []string  `json:"problems"`
}

// GetStatus collects the summary (read-only, no root needed for most).
func (e *Env) GetStatus(ctx context.Context, pending int) Status {
	s := Status{Pending: pending, OS: e.osRelease()}
	s.SELinux = strings.TrimSpace(e.R.Read(ctx, "getenforce").Out)
	if s.SELinux != "Enforcing" {
		s.Problems = append(s.Problems, "SELinux is "+orDash(s.SELinux)+", not Enforcing, so it is not protecting the system")
	}
	res := e.R.Read(ctx, "systemctl", "list-units", "--failed", "--plain", "--no-legend", "--no-pager")
	for _, l := range strings.Split(res.Out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			s.FailedUnits = append(s.FailedUnits, f[0])
		}
	}
	if len(s.FailedUnits) > 0 {
		for _, u := range s.FailedUnits {
			s.Problems = append(s.Problems, u+" has failed. See why: basalt why "+u)
		}
	}
	s.Denials24h = len(e.CollectAVCs(ctx, e.now().Add(-24*time.Hour)))
	if s.Denials24h > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("SELinux blocked something %s in the last 24 hours. See what: basalt fix selinux --since 24h", times(s.Denials24h)))
	}
	if st, err := e.Statfs("/"); err == nil {
		s.Disk, s.DiskPct = st, st.UsedPct()
		if s.DiskPct >= DefaultDiskThresholds.WarnPct {
			s.Problems = append(s.Problems, fmt.Sprintf("The root file system is %.0f %% full. See what uses it: basalt disk", s.DiskPct))
		}
	}
	snaps := e.Snapshots(ctx)
	s.Snapshots = len(snaps)
	if len(snaps) > 0 {
		last := snaps[len(snaps)-1]
		s.LastSnapshot = fmt.Sprintf("%d (%s, %s, %q)", last.Number, last.Date, last.Type, last.Description)
	}
	for _, o := range OrphanPre(snaps, e.now(), 10*time.Minute) {
		s.OrphanPre = append(s.OrphanPre, o.Number)
	}
	if len(s.OrphanPre) > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("A package transaction did not finish (snapshot %s has no matching post snapshot). See: basalt snapshots", joinInts(s.OrphanPre)))
	}
	s.RollbackState = "none"
	if !e.Confined {
		def := e.R.Read(ctx, "btrfs", "subvolume", "get-default", "/").Out
		root := strings.TrimSpace(e.R.Read(ctx, "btrfs", "inspect-internal", "rootid", "/").Out)
		if f := strings.Fields(def); len(f) >= 2 && root != "" && f[1] != root {
			s.RollbackState = "pending: reboot to use the rolled-back root"
			s.Problems = append(s.Problems, "A rollback is waiting: it takes effect at the next boot")
		}
	}
	s.Daemon = strings.TrimSpace(e.R.Read(ctx, "systemctl", "is-active", "basalt-assistantd.service").Out)
	if pending > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("%s waiting for your decision. See: basalt pending", plural(pending, "proposal is", "proposals are")))
	}
	return s
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func joinInts(ns []int) string {
	var ss []string
	for _, n := range ns {
		ss = append(ss, strconv.Itoa(n))
	}
	return strings.Join(ss, ", ")
}
