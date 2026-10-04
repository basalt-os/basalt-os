package cli

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
)

// go test ./internal/cli -update rewrites testdata/golden/*.txt.
var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	fn := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(fn), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fn, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(fn)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update)", name, err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s:\n%s", name, fn, got)
	}
}

func TestGoldenStatus(t *testing.T) {
	os := diag.OSRelease{Name: "Basalt OS", Version: "44 (Server)"}
	fine := diag.Status{OS: os, SELinux: "Enforcing", Disk: diag.FSStat{Total: 32 << 30, Free: 21 << 30}, DiskPct: 34.4,
		Snapshots: 14, LastSnapshot: `31 (2026-10-04 09:12:00, post, "dnf install htop")`, RollbackState: "none", Daemon: "active"}
	var b bytes.Buffer
	writeStatus(&b, fine, 212, nil)
	golden(t, "status-fine", b.String())

	bad := fine
	bad.SELinux, bad.FailedUnits, bad.Denials24h, bad.DiskPct, bad.Pending = "Permissive", []string{"nginx.service"}, 3, 91.2, 2
	bad.Problems = []string{"SELinux is Permissive, not Enforcing, so it is not protecting the system",
		"nginx.service has failed. See why: basalt why nginx.service",
		"SELinux blocked something 3 times in the last 24 hours. See what: basalt fix selinux --since 24h",
		"The root file system is 91 % full. See what uses it: basalt disk",
		"2 proposals are waiting for your decision. See: basalt pending"}
	b.Reset()
	writeStatus(&b, bad, 0, errors.New("record 17: hash mismatch"))
	golden(t, "status-problems", b.String())
}

func TestGoldenDiskAndSnapshots(t *testing.T) {
	rep := &diag.DiskReport{FS: diag.FSStat{Total: 32 << 30, Free: 1 << 30}, UsedPct: 96.9, Journal: 1610 << 20, PkgCache: 920 << 20,
		SnapshotTotal: 3 << 30, Forecast: diag.Forecast{Samples: 12, SpanHours: 6, DaysToFull: 3},
		Snapshots: []diag.SnapSpace{{Snapshot: diag.Snapshot{Number: 12, Date: "2026-09-30 08:00:01", Description: "before upgrade"}, Exclusive: 2900 << 20},
			{Snapshot: diag.Snapshot{Number: 30, Date: "2026-10-04 09:11:58", Description: "dnf install htop"}, Exclusive: 12 << 20}}}
	var b bytes.Buffer
	writeDisk(&b, rep, false)
	golden(t, "disk", b.String())

	snaps := []diag.Snapshot{
		{Number: 1, Type: "single", Date: "2026-10-01 12:00:00", Description: "first root filesystem"},
		{Number: 30, Type: "pre", Date: "2026-10-04 09:11:58", Description: "dnf install htop"},
		{Number: 31, Type: "post", Pre: 30, Date: "2026-10-04 09:12:00", Description: "dnf install htop"},
		{Number: 32, Type: "pre", Date: "2026-10-04 09:40:00", Description: "basalt apply p-1a2b3c", Userdata: map[string]string{"basalt": "apply", "proposal": "p-1a2b3c"}},
		{Number: 41, Type: "pre", Date: "2026-10-04 10:00:00", Description: "dnf install basalt-lab-broken"},
	}
	b.Reset()
	writeSnapshots(&b, snaps, snaps[4:])
	golden(t, "snapshots", b.String())
	b.Reset()
	writeSnapshots(&b, nil, nil)
	golden(t, "snapshots-none", b.String())

	if got := human(24 * time.Hour); got != "24 hours" {
		t.Errorf("human: %q", got)
	}
}
