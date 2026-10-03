package diag

import (
	"context"
	"fmt"
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
	SELinux       string   `json:"selinux"`
	FailedUnits   []string `json:"failed_units"`
	Denials24h    int      `json:"denials_24h"`
	Disk          FSStat   `json:"disk"`
	DiskPct       float64  `json:"disk_pct"`
	Snapshots     int      `json:"snapshots"`
	LastSnapshot  string   `json:"last_snapshot"`
	OrphanPre     []int    `json:"unfinished_transactions,omitempty"`
	RollbackState string   `json:"rollback_state"`
	Daemon        string   `json:"daemon"`
	Pending       int      `json:"pending"`
	Problems      []string `json:"problems"`
}

// GetStatus collects the summary (read-only, no root needed for most).
func (e *Env) GetStatus(ctx context.Context, pending int) Status {
	s := Status{Pending: pending}
	s.SELinux = strings.TrimSpace(e.R.Read(ctx, "getenforce").Out)
	if s.SELinux != "Enforcing" {
		s.Problems = append(s.Problems, "SELinux is "+orDash(s.SELinux)+", not Enforcing")
	}
	res := e.R.Read(ctx, "systemctl", "list-units", "--failed", "--plain", "--no-legend", "--no-pager")
	for _, l := range strings.Split(res.Out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			s.FailedUnits = append(s.FailedUnits, f[0])
		}
	}
	if len(s.FailedUnits) > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("%d failed unit(s): %s (basalt why <unit>)", len(s.FailedUnits), strings.Join(s.FailedUnits, " ")))
	}
	s.Denials24h = len(e.CollectAVCs(ctx, e.now().Add(-24*time.Hour)))
	if s.Denials24h > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("%d SELinux denial(s) in 24 h (basalt fix selinux)", s.Denials24h))
	}
	if st, err := e.Statfs("/"); err == nil {
		s.Disk, s.DiskPct = st, st.UsedPct()
		if s.DiskPct >= DefaultDiskThresholds.WarnPct {
			s.Problems = append(s.Problems, fmt.Sprintf("root file system %.0f %% full (basalt disk)", s.DiskPct))
		}
	}
	snaps := e.Snapshots(ctx)
	s.Snapshots = len(snaps)
	if len(snaps) > 0 {
		last := snaps[len(snaps)-1]
		s.LastSnapshot = fmt.Sprintf("%d %s %s %q", last.Number, last.Type, last.Date, last.Description)
	}
	for _, o := range OrphanPre(snaps, e.now(), 10*time.Minute) {
		s.OrphanPre = append(s.OrphanPre, o.Number)
	}
	if len(s.OrphanPre) > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("unfinished package transaction(s) after pre snapshot %v", s.OrphanPre))
	}
	s.RollbackState = "none"
	if !e.Confined {
		def := e.R.Read(ctx, "btrfs", "subvolume", "get-default", "/").Out
		root := strings.TrimSpace(e.R.Read(ctx, "btrfs", "inspect-internal", "rootid", "/").Out)
		if f := strings.Fields(def); len(f) >= 2 && root != "" && f[1] != root {
			s.RollbackState = "pending: reboot to use the rolled-back root"
			s.Problems = append(s.Problems, "a rollback is pending until the next boot")
		}
	}
	s.Daemon = strings.TrimSpace(e.R.Read(ctx, "systemctl", "is-active", "basalt-assistantd.service").Out)
	if pending > 0 {
		s.Problems = append(s.Problems, fmt.Sprintf("%d pending proposal(s) (basalt pending)", pending))
	}
	return s
}
