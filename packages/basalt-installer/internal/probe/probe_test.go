package probe

import (
	"os"
	"strings"
	"testing"
)

func TestParseLsblk(t *testing.T) {
	data, err := os.ReadFile("testdata/lsblk.json")
	if err != nil {
		t.Fatal(err)
	}
	disks, err := ParseLsblk(data)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	if _, ok := byName["loop0"]; ok {
		t.Fatal("loop devices are not disks")
	}
	if _, ok := byName["zram0"]; ok {
		t.Fatal("zram is not an install target")
	}
	if _, ok := byName["sr0"]; ok {
		t.Fatal("optical drives are not install targets")
	}
	vda := byName["vda"]
	if vda.Contents != "empty" || vda.InUse != "" || vda.Complex != "" || vda.SizeGiB() != 30 {
		t.Fatalf("vda: %+v", vda)
	}
	nv := byName["nvme0n1"]
	if !strings.Contains(nv.InUse, "crypt") || nv.Contents != "2 partitions: vfat, crypto_LUKS" || nv.Model != "Samsung SSD 980" || nv.Rotational {
		t.Fatalf("nvme0n1 (old-style string booleans): %+v", nv)
	}
	if byName["sda"].Complex != "member of a RAID array" {
		t.Fatalf("sda: %+v", byName["sda"])
	}
	if byName["sdb"].Complex != "iSCSI disk" {
		t.Fatalf("sdb: %+v", byName["sdb"])
	}
	sdc := byName["sdc"]
	if !sdc.Removable || !strings.Contains(sdc.InUse, "mounted on /run/media/key") {
		t.Fatalf("sdc: %+v", sdc)
	}
}

func TestParseKeyMedia(t *testing.T) {
	data, err := os.ReadFile("testdata/lsblk.json")
	if err != nil {
		t.Fatal(err)
	}
	media, err := ParseKeyMedia(data)
	if err != nil {
		t.Fatal(err)
	}
	// Only the USB stick's FAT partition: not the installer media (sr0,
	// BASALT-INST), not fixed disks, not a whole disk without a file system.
	if len(media) != 1 {
		t.Fatalf("key media: %+v", media)
	}
	m := media[0]
	if m.Path != "/dev/sdc1" || m.Disk != "/dev/sdc" || m.Label != "KEY" || m.FSType != "vfat" || m.Mountpoint != "/run/media/key" || m.Model != "USB Stick" {
		t.Fatalf("key medium: %+v", m)
	}
	if !strings.Contains(m.Describe(), "\"KEY\"") {
		t.Fatal(m.Describe())
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{512: "512 B", 32212254720: "30.0 GiB", 1000204886016: "931.5 GiB"} {
		if got := HumanSize(in); got != want {
			t.Errorf("%d: %s, want %s", in, got, want)
		}
	}
}
