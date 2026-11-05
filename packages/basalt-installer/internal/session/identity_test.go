package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

// The confirmation names the disk by its serial number; the kernel name
// only for a disk without one.
func TestConfirmWord(t *testing.T) {
	if w := ConfirmWord(probe.Disk{Path: "/dev/nvme0n1", Serial: " S4EWNX0R123456 "}); w != "S4EWNX0R123456" {
		t.Error(w)
	}
	if w := ConfirmWord(probe.Disk{Path: "/dev/vda"}); w != "vda" {
		t.Error(w)
	}
	if w := ConfirmWord(probe.Disk{Path: "/dev/sda", Serial: "has spaces in it"}); w != "sda" {
		t.Error(w)
	}
}

// Install refuses when the disk under the reviewed kernel name is not the
// reviewed disk any more, or the adopted /home changed.
func TestSameMachineState(t *testing.T) {
	d := probe.Disk{Name: "nvme0n1", Path: "/dev/nvme0n1", Serial: "S1", SizeBytes: 500 << 30, Table: "gpt", PTUUID: "abc",
		Partitions: []probe.Partition{{Path: "/dev/nvme0n1p1", Number: 1, PartUUID: "p1", Start: 2048, Sectors: 1000}}}
	pv := Preview{Resolved: steps.Resolved{Disk: d}}
	f := probe.Facts{Disks: []probe.Disk{d}}
	if why := sameMachineState(pv, f); why != "" {
		t.Fatal(why)
	}
	swapped := d
	swapped.Serial = "S2"
	if why := sameMachineState(pv, probe.Facts{Disks: []probe.Disk{swapped}}); !strings.Contains(why, "serial") {
		t.Errorf("swapped disk: %q", why)
	}
	if why := sameMachineState(pv, probe.Facts{}); !strings.Contains(why, "gone") {
		t.Errorf("missing disk: %q", why)
	}
	pv.Resolved.Existing = &steps.ExistingHome{Device: "/dev/nvme0n1p1", UUID: "home-uuid"}
	if why := sameMachineState(pv, f); !strings.Contains(why, "/home") {
		t.Errorf("changed /home: %q", why)
	}
}

// Sharing an ESP that already holds EFI/fedora is refused at the preview.
func TestSharedESPWithFedoraRefused(t *testing.T) {
	ss := New(Options{ESPHasFedora: func(_ context.Context, dev string) (bool, error) { return dev == "/dev/sda1", nil }})
	p := plan.Plan{Target: plan.Target{Disk: "/dev/sda", ESP: "/dev/sda1"}}
	if err := ss.checkSharedESP(context.Background(), p); err == nil || !strings.Contains(err.Error(), "EFI/fedora") {
		t.Fatalf("%v", err)
	}
	p.Target.ESP = "/dev/sda2"
	if err := ss.checkSharedESP(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ss = New(Options{ESPHasFedora: func(context.Context, string) (bool, error) { return false, errors.New("not FAT") }})
	if err := ss.checkSharedESP(context.Background(), p); err == nil {
		t.Fatal("an ESP that cannot be read is not shared")
	}
}

// An unattended installation on a disk with a serial number must be
// confirmed with that serial, not the kernel name.
func TestUnattendedNeedsTheSerial(t *testing.T) {
	old := lsblkFixture
	t.Cleanup(func() { lsblkFixture = old })
	lsblkFixture = strings.Replace(old, `"serial":null,"tran":null,"fstype":null,"mountpoints":[null],"label":null}`, `"serial":"QM0001","tran":null,"fstype":null,"mountpoints":[null],"label":null}`, 1)
	r := newRig(t, basePlan, "basalt.inst.plan=PLAN basalt.inst.confirm=vda")
	if a := r.run(t); a.State != AutoRefused || !strings.Contains(a.Error, "give QM0001") {
		t.Fatalf("kernel name accepted: %+v", a)
	}
}
