package session

import (
	"context"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// The suggestion never offers a disk that holds another system or an
// encrypted volume, whatever the disk order (B5: on a laptop whose first
// NVMe holds the LUKS /home).
func TestSuggestDiskSkipsDisksWithData(t *testing.T) {
	luks := probe.Disk{Name: "nvme0n1", Path: "/dev/nvme0n1", SizeBytes: 1900 << 30, Contents: "1 partition: crypto_LUKS",
		Partitions: []probe.Partition{{Path: "/dev/nvme0n1p1", FSType: "crypto_LUKS"}}}
	probe.DescribeHolds(&luks, nil)
	empty := probe.Disk{Name: "nvme1n1", Path: "/dev/nvme1n1", SizeBytes: 500 << 30, Contents: "empty"}
	f := probe.Facts{Disks: []probe.Disk{luks, empty}}
	// Not even the blank-looking disk: it may hold headerless encrypted
	// data the probe cannot see. The person always picks the disk.
	if got := SuggestDisk(f); got != "" {
		t.Fatalf("suggested %q", got)
	}
}

// pt_BR on the boot line or the live session gives the ABNT2 keyboard and
// the São Paulo time zone; an empty suggested disk is erased.
func TestSuggestLocalizes(t *testing.T) {
	r := newRig(t, "", "quiet basalt.inst.locale=pt_BR.UTF-8")
	p, _, err := r.ss.SuggestWith(context.Background(), Hints{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Locale != "pt_BR.UTF-8" || p.Keymap != "br" || p.Timezone != "America/Sao_Paulo" {
		t.Fatalf("boot line: %s %s %s", p.Locale, p.Keymap, p.Timezone)
	}
	if p.Target.Disk != "" || p.Target.Wipe {
		t.Fatalf("nothing is chosen or erased by the suggestion: %+v", p.Target)
	}
	r = newRig(t, "", "quiet")
	p, _, _ = r.ss.SuggestWith(context.Background(), Hints{Locale: "pt_BR.utf8", XKBLayout: "us", XKBVariant: "intl"})
	if p.Locale != "pt_BR.UTF-8" || p.Keymap != "us-intl" {
		t.Fatalf("session hints: %s %s", p.Locale, p.Keymap)
	}
	p, _, _ = r.ss.SuggestWith(context.Background(), Hints{Locale: "C.UTF-8"})
	if p.Locale != "en_US.UTF-8" || p.Keymap != "us" || p.Timezone != "Etc/UTC" {
		t.Fatalf("C locale keeps the defaults: %s %s %s", p.Locale, p.Keymap, p.Timezone)
	}
}
