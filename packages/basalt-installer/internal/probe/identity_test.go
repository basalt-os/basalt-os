package probe

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSameDiskAndStableNames(t *testing.T) {
	a := Disk{Name: "nvme0n1", Path: "/dev/nvme0n1", Serial: "S1", WWN: "eui.1", Model: "M", SizeBytes: 100, Table: "gpt", PTUUID: "p",
		Partitions: []Partition{{Number: 1, PartUUID: "u1", Type: TypeESP, Start: 2048, Sectors: 100}}}
	if why := SameDisk(a, a); why != "" {
		t.Fatal(why)
	}
	for name, change := range map[string]func(*Disk){
		"another disk under the same kernel name": func(d *Disk) { d.Serial = "S2" },
		"another table": func(d *Disk) { d.PTUUID = "q" },
		"a partition moved": func(d *Disk) {
			d.Partitions = []Partition{{Number: 1, PartUUID: "u1", Type: TypeESP, Start: 4096, Sectors: 100}}
		},
		"a partition added": func(d *Disk) { d.Partitions = append(d.Partitions, Partition{Number: 2}) },
		"another size":      func(d *Disk) { d.SizeBytes = 200 },
	} {
		b := a
		b.Partitions = append([]Partition(nil), a.Partitions...)
		change(&b)
		if SameDisk(a, b) == "" {
			t.Errorf("%s: not noticed", name)
		}
	}
	f := Facts{Disks: []Disk{{Name: "vda", Path: "/dev/vda"}, {Name: "nvme0n1", Path: "/dev/nvme0n1", Serial: "S1", ByID: []string{"/dev/disk/by-id/nvme-M_S1"}}}}
	for _, n := range []string{"/dev/nvme0n1", "serial:S1", "/dev/disk/by-id/nvme-M_S1"} {
		if d, ok := f.Disk(n); !ok || d.Name != "nvme0n1" {
			t.Errorf("%s: %+v", n, d)
		}
	}
	if _, ok := f.Disk("serial:"); ok {
		t.Error("an empty serial matches a disk without one")
	}
	// by-id links of whole disks only.
	root := t.TempDir()
	dir := filepath.Join(root, "dev/disk/by-id")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.Symlink("../../nvme0n1", filepath.Join(dir, "nvme-M_S1"))
	_ = os.Symlink("../../nvme0n1p1", filepath.Join(dir, "nvme-M_S1-part1"))
	if got := (Prober{Root: root}).byID(); len(got["nvme0n1"]) != 1 || len(got["nvme0n1p1"]) != 0 {
		t.Fatalf("%v", got)
	}
}
