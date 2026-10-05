package steps

import (
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// windowsDisk is a 100 GiB disk with a Windows layout (EFI, MSR, NTFS,
// recovery) on its first 60 GiB, the rest free.
func windowsDisk() probe.Disk {
	const sec = 512
	d := probe.Disk{Name: "nvme0n1", Path: "/dev/nvme0n1", SizeBytes: 100 << 30, Contents: "4 partitions: vfat, ntfs, ntfs", Table: "gpt", SectorSize: sec}
	add := func(n int, start, bytes int64, typ, fs, label, uuid string) {
		d.Partitions = append(d.Partitions, probe.Partition{Path: PartitionPath(d.Path, n), Number: n, Start: start, Sectors: bytes / sec, Bytes: bytes,
			Type: typ, FSType: fs, Label: label, UUID: uuid, Disk: d.Path})
	}
	add(1, 2048, 100<<20, probe.TypeESP, "vfat", "SYSTEM", "1a2b-3c4d")
	add(2, 2048+(100<<20)/sec, 16<<20, probe.TypeMSR, "", "", "")
	add(3, 2048+(116<<20)/sec, 59<<30, probe.TypeWindows, "ntfs", "Windows", "01D8")
	add(4, 2048+(116<<20)/sec+(59<<30)/sec, 800<<20, probe.TypeWinRecov, "ntfs", "Recovery", "02D8")
	end := d.Partitions[3].Start + d.Partitions[3].Sectors
	start := (end + 2047) / 2048 * 2048
	last := (d.SizeBytes/sec - 34) / 2048 * 2048
	d.Free = []probe.Region{{Start: start, Sectors: last - start, Bytes: (last - start) * sec}}
	return d
}

// homeDisk is a second disk with an encrypted /home partition.
func homeDisk() probe.Disk {
	return probe.Disk{Name: "sda", Path: "/dev/sda", SizeBytes: 200 << 30, Table: "gpt", SectorSize: 512, Contents: "1 partition: crypto_LUKS",
		Partitions: []probe.Partition{{Path: "/dev/sda1", Number: 1, Start: 2048, Sectors: (200 << 30) / 512, Bytes: 199 << 30,
			FSType: "crypto_LUKS", UUID: "11111111-2222-3333-4444-555555555555", Disk: "/dev/sda"}}}
}

func TestDualBootKeepsWindows(t *testing.T) {
	f := facts("kvm", true)
	f.Disks = []probe.Disk{windowsDisk()}
	p := basePlan("/dev/nvme0n1")
	p.Target.Wipe = false
	p.ApplyDefaults()
	if is := plan.Validate(p, &f, ""); is.Err() != nil {
		t.Fatal(is.Err())
	}
	r, list := generate(t, p, f)
	text := Preview(list)
	for _, bad := range []string{"--zap-all", "--clear", "wipefs --all --force /dev/nvme0n1\n", "nvme0n1p1 ", "nvme0n1p3", "--delete=1 ", "--delete=3"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the preview touches the Windows layout (%q):\n%s", bad, text)
		}
	}
	if r.ESPDev != "/dev/nvme0n1p5" || r.BootDev != "/dev/nvme0n1p6" || r.SystemDev != "/dev/nvme0n1p7" || r.ESPNumber != 5 {
		t.Fatalf("new partitions: %+v", r.NewParts)
	}
	free := f.Disks[0].Free[0]
	if r.NewParts[0].Start != free.Start || r.NewParts[2].End != free.Start+free.Sectors-1 {
		t.Fatalf("not inside the free region %+v: %+v", free, r.NewParts)
	}
	_, s := mustFind(t, list, "sgdisk --new=5:")
	if !strings.Contains(strings.Join(s.Undo, " "), "--delete=5 --delete=6 --delete=7") || !strings.Contains(s.Note, "Windows data") {
		t.Fatalf("partition step: undo %v, note %q", s.Undo, s.Note)
	}
	mustFind(t, list, "efibootmgr --create --disk /dev/nvme0n1 --part 5")
	mustFind(t, list, "mkfs.vfat -F 32 -n EFI")

	// Sharing Windows' EFI system partition: not formatted, used by number.
	p.Target.ESP = "/dev/nvme0n1p1"
	r, list = generate(t, p, f)
	if !r.ESPShared || r.ESPDev != "/dev/nvme0n1p1" || r.BootDev != "/dev/nvme0n1p5" || r.ESPUUID() != "1A2B-3C4D" {
		t.Fatalf("shared ESP: %+v", r)
	}
	if i, _ := find(list, "mkfs.vfat"); i >= 0 {
		t.Fatal("a shared EFI system partition is formatted")
	}
	mustFind(t, list, "efibootmgr --create --disk /dev/nvme0n1 --part 1")
}

func TestDualBootNeedsSpace(t *testing.T) {
	f := facts("kvm", true)
	d := windowsDisk()
	d.Free[0].Bytes, d.Free[0].Sectors = 4<<30, (4<<30)/512
	f.Disks = []probe.Disk{d}
	p := basePlan("/dev/nvme0n1")
	p.Target.Wipe = false
	p.ApplyDefaults()
	if err := plan.Validate(p, &f, "").Err(); err == nil || !strings.Contains(err.Error(), "largest free space") {
		t.Fatalf("too little free space: %v", err)
	}
}

func TestEncryptHomeOnly(t *testing.T) {
	f := facts("kvm", true)
	p := basePlan("/dev/nvme0n1")
	p.Encryption.Scope = "home"
	p.Layout.Subvolumes = nil
	p.ApplyDefaults()
	if is := plan.Validate(p, &f, ""); is.Err() != nil {
		t.Fatal(is.Err())
	}
	r, list := generate(t, p, f)
	text := Preview(list)
	if r.HomeDev != "/dev/nvme0n1p4" || r.LUKSDev() != "/dev/nvme0n1p4" || r.BtrfsDev() != "/dev/nvme0n1p3" {
		t.Fatalf("devices: home %s luks %s btrfs %s", r.HomeDev, r.LUKSDev(), r.BtrfsDev())
	}
	for _, want := range []string{"--new=3:0:+64G --typecode=3:8300", "--new=4:0:0 --typecode=4:8309", "luksFormat --batch-mode --type luks2",
		"--label basalt-home", "/home btrfs compress=zstd:1", "setfiles"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "rd.luks.uuid") || strings.Contains(text, "subvolume create /run/basalt-installer/btrfs-top/home") {
		t.Fatal("home-only encryption must not unlock in the initramfs or make a home subvolume")
	}
	mustFind(t, list, "--recovery-key /dev/nvme0n1p4")
}

func TestAdoptExistingHome(t *testing.T) {
	f := facts("kvm", true)
	f.Disks = append(f.Disks, homeDisk())
	p := basePlan("/dev/vda")
	p.Encryption.Enabled, p.Encryption.Scope = plan.Bool(false), ""
	p.Home.Existing = &plan.ExistingHome{Device: "/dev/sda1", Passphrase: "home passphrase 1", TPM2: true}
	p.Accounts.User = &plan.User{Name: "edimar", Password: "pw-1234567", HomeDir: "/home/edimar-basalt", UID: 1000, GID: 1000}
	p.Layout.Subvolumes = nil
	p.ApplyDefaults()
	if is := plan.Validate(p, &f, ""); is.Err() != nil {
		t.Fatal(is.Err())
	}
	r, list := generate(t, p, f)
	text := Preview(list)
	if strings.Contains(text, "home passphrase 1") {
		t.Fatal("the passphrase is in the preview")
	}
	i0, check := mustFind(t, list, "cryptsetup open --test-passphrase --key-file=- /dev/sda1")
	if check.Stdin != "home passphrase 1" {
		t.Fatal("the check reads the passphrase on standard input")
	}
	iWipe, _ := mustFind(t, list, "sgdisk --zap-all /dev/vda")
	if i0 > iWipe {
		t.Fatal("the passphrase is checked before anything is written")
	}
	for _, bad := range []string{"mkfs.btrfs --force --label basalt-home", "mkfs.ext4 -q -F -L boot -U " + r.BootUUID + " /dev/sda", "wipefs --all --force /dev/sda", "luksFormat"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the existing /home is written to (%q)", bad)
		}
	}
	for _, want := range []string{
		"cryptsetup open --key-file=- /dev/sda1 luks-11111111-2222-3333-4444-555555555555",
		"/dev/mapper/luks-11111111-2222-3333-4444-555555555555 /home auto defaults 0 2",
		"luks-11111111-2222-3333-4444-555555555555 UUID=11111111-2222-3333-4444-555555555555 none discard,tpm2-device=auto",
		"groupadd --gid 1000 edimar", "useradd --create-home --home-dir /home/edimar-basalt --uid 1000 --gid 1000 --groups wheel edimar",
		"PASSWORD='<passphrase>' systemd-cryptenroll --tpm2-device=auto --tpm2-pcrs=7 /dev/sda1",
		"restorecon -R -F /home/edimar-basalt", "cryptsetup close luks-11111111",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if _, s := mustFind(t, list, "setfiles -F"); strings.Contains(strings.Join(s.Argv, " "), " /home") {
		t.Fatal("the full relabel must not touch the existing /home")
	}
	if strings.Contains(text, "subvolume create /run/basalt-installer/btrfs-top/home") {
		t.Fatal("no home subvolume with an existing /home")
	}
}
