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
	groups := selinux.GroupAVCs(e.CollectAVCs(ctx, since))
	e.PrefetchAVCs(ctx, groups)
	for _, g := range groups {
		f := e.AnalyzeAVC(ctx, g)
		q := decide.AVCClass(g.AVC.Key(), f.Features)
		e.MarkView(&q)
		d := e.ask(ctx, q)
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
	SnapshotsOff  string    `json:"snapshots_off,omitempty"`
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
	if off := e.SnapshotsOff(ctx); off != "" {
		s.SnapshotsOff = off
		s.Problems = append(s.Problems, off)
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

// SnapshotsOff says why package transactions take no snapshots of the
// root, or "" when they do (or on a live system, which has nothing to
// keep). A Fedora install that got the Basalt packages afterwards has no
// snapper configuration: basalt-snapshots-auto.service sets it up at the
// next boot when the root is a btrfs subvolume booted by GRUB.
func (e *Env) SnapshotsOff(ctx context.Context) string {
	if e.SnapperConfig == "" || e.Inode == nil {
		return ""
	}
	if _, ok := e.Inode(e.SnapperConfig); ok {
		return ""
	}
	fs := strings.TrimSpace(e.R.Read(ctx, "findmnt", "-no", "FSTYPE", "/").Out)
	switch fs {
	case "overlay", "squashfs", "tmpfs", "iso9660":
		return "" // a live system
	case "btrfs":
		return "Snapshots are off: the root file system has no snapper configuration yet, so updates cannot be undone. " +
			"They are set up at the next start (basalt-snapshots-auto.service), or now with: sudo basalt-snapshots-setup. " +
			"If they still are not, see why: journalctl -u basalt-snapshots-auto"
	case "":
		return "Snapshots are off: the root file system has no snapper configuration, so updates cannot be undone. " +
			"See why: journalctl -u basalt-snapshots-auto"
	}
	return fmt.Sprintf("Snapshots are off: the root file system is %s, not btrfs, so updates cannot be undone from a snapshot. "+
		"Basalt OS snapshots need btrfs (a new install of Basalt OS sets it up)", fs)
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
