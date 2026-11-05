package steps

import (
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

// dualFacts is a laptop with two NVMe disks: nvme0n1 holds Windows
// (ESP, MSR, a BitLocker C: and an NTFS data partition), an old Fedora
// (/boot ext4 and a btrfs root) and 100 GiB of free space; nvme1n1 holds
// a LUKS volume (another system's /home) and must never be touched.
func dualFacts() probe.Facts {
	sec := int64(512)
	part := func(disk string, n int, start, size int64, typ, fs, label string) probe.Partition {
		return probe.Partition{Path: PartitionPath(disk, n), Number: n, Start: start / sec, Sectors: size / sec, Bytes: size,
			Type: typ, FSType: fs, Label: label, UUID: strings.Repeat(string(rune('a'+n)), 8), PartUUID: strings.Repeat(string(rune('a'+n)), 8) + "-0000-0000-0000-000000000000", Disk: disk}
	}
	d0 := "/dev/nvme0n1"
	win := probe.Disk{Name: "nvme0n1", Path: d0, SizeBytes: 512 * gib, Model: "WD Black SN770", Serial: "W123", Tran: "nvme", Table: "gpt", SectorSize: 512,
		Contents: "6 partitions"}
	win.Partitions = []probe.Partition{
		part(d0, 1, 1*mib, 260*mib, probe.TypeESP, "vfat", "SYSTEM"),
		part(d0, 2, 261*mib, 16*mib, probe.TypeMSR, "", ""),
		part(d0, 3, 277*mib, 200*gib, probe.TypeWindows, "BitLocker", ""),
		part(d0, 4, 201*gib, 100*gib, probe.TypeWindows, "ntfs", "DATA"),
		part(d0, 5, 301*gib, 1*gib, "0fc63daf-8483-4772-8e79-3d69d8477de4", "ext4", "boot"),
		part(d0, 6, 302*gib, 100*gib, "0fc63daf-8483-4772-8e79-3d69d8477de4", "btrfs", "fedora"),
	}
	win.Free = []probe.Region{{Start: 402 * gib / sec, Sectors: 100 * gib / sec, Bytes: 100 * gib}}
	d1 := "/dev/nvme1n1"
	data := probe.Disk{Name: "nvme1n1", Path: d1, SizeBytes: 1900 * gib, Model: "Samsung 990", Serial: "S456", Tran: "nvme", Table: "gpt", SectorSize: 512,
		Contents: "1 partition: crypto_LUKS"}
	data.Partitions = []probe.Partition{part(d1, 1, 1*mib, 1400*gib, "0fc63daf-8483-4772-8e79-3d69d8477de4", "crypto_LUKS", "")}
	f := probe.Facts{UEFI: true, SecureBoot: "enabled", TPM2: true, Arch: "x86_64", MemoryMiB: 32768, Release: 44,
		Disks: []probe.Disk{win, data}, SecureBootDB: probe.SecureBootDB{Read: true, WindowsCA: true, UEFICA: true},
		BootEntries: []probe.BootEntry{{Num: "0000", Name: "Windows Boot Manager", PartUUID: win.Partitions[0].PartUUID, Loader: `\EFI\Microsoft\Boot\bootmgfw.efi`},
			{Num: "0001", Name: "Fedora", PartUUID: win.Partitions[0].PartUUID, Loader: `\EFI\fedora\shimx64.efi`}}}
	for i := range f.Disks {
		probe.DescribeHolds(&f.Disks[i], f.BootEntries)
	}
	return f
}

func dualPlan() plan.Plan {
	p := basePlan("/dev/nvme0n1")
	p.Target.Wipe = false
	return p
}

// Installing alongside writes only the new partitions; the Windows and
// Fedora partitions and the other disk appear only as untouched.
func TestAlongsideTouchesOnlyNewPartitions(t *testing.T) {
	f := dualFacts()
	r, list := generate(t, dualPlan(), f)
	if err := CheckSafety(r, list); err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		c := s.Command()
		for _, dev := range []string{"nvme0n1p1 ", "nvme0n1p3", "nvme0n1p4", "nvme0n1p5", "nvme0n1p6", "nvme1n1"} {
			if strings.Contains(c+" ", "/dev/"+dev) && !strings.HasPrefix(s.Title, "List the other systems") {
				t.Errorf("step %q names %s: %s", s.Title, dev, c)
			}
		}
		if strings.Contains(c, "--zap-all") || strings.Contains(c, "--clear") {
			t.Errorf("the partition table is rewritten: %s", c)
		}
	}
	if len(r.NewParts) != 3 || r.NewParts[0].Number != 7 {
		t.Fatalf("new partitions: %+v", r.NewParts)
	}
	plan := DiskPlan(r)
	for _, want := range []string{"WD Black SN770, serial W123", "untouched  /dev/nvme0n1p3", "untouched  /dev/nvme0n1p6", "Samsung 990, serial S456", "untouched (LUKS encrypted volume /dev/nvme1n1p1)",
		"new        /dev/nvme0n1p7"} {
		if !strings.Contains(plan, want) {
			t.Errorf("disk plan lacks %q:\n%s", want, plan)
		}
	}
	// The boot menu lists Windows and the old Fedora: their ESP is
	// searched read-only.
	_, menu := mustFind(t, list, "basalt-installer other-systems")
	if !strings.Contains(menu.Command(), "/dev/nvme0n1p1=") {
		t.Errorf("the Windows ESP is not searched: %s", menu.Command())
	}
	_, mk := find(list, "grub2-mkconfig")
	if i, _ := find(list, "other-systems"); i < 0 || mk.Title == "" {
		t.Fatal("no grub2-mkconfig after the other systems")
	}
}

// Reusing the old Fedora root for the system formats exactly that
// partition and nothing else.
func TestCustomReuseFormatsOnlyTheChosenPartition(t *testing.T) {
	f := dualFacts()
	p := dualPlan()
	p.Target.System = "/dev/nvme0n1p6"
	p.Target.Boot = "/dev/nvme0n1p5"
	p.Target.ESP = "/dev/nvme0n1p1"
	// Sharing an ESP that carries another Fedora's boot loader is refused.
	if is := plan.Validate(p, &f, ""); !hasErr(is, "target.esp") {
		t.Fatalf("an ESP with another Fedora's EFI/fedora must not be shared: %v", is)
	}
	p.Target.ESP = ""
	r, list := generate(t, p, f)
	if r.SystemDev != "/dev/nvme0n1p6" || r.BootDev != "/dev/nvme0n1p5" || len(r.NewParts) != 1 || r.NewParts[0].Role != "esp" {
		t.Fatalf("resolved: system %s boot %s new %+v", r.SystemDev, r.BootDev, r.NewParts)
	}
	mustFind(t, list, "cryptsetup luksFormat --batch-mode --type luks2 --uuid "+r.LUKSUUID+" --label basalt --key-file /run/basalt-installer/luks.key /dev/nvme0n1p6")
	mustFind(t, list, "mkfs.ext4 -q -F -L boot -U "+r.BootUUID+" /dev/nvme0n1p5")
	for _, s := range list {
		for _, dev := range []string{"/dev/nvme0n1p3", "/dev/nvme0n1p4", "/dev/nvme1n1"} {
			if strings.Contains(s.Command(), dev) {
				t.Errorf("step %q names %s", s.Title, dev)
			}
		}
	}
	if dp := DiskPlan(r); !strings.Contains(dp, "FORMATTED  /dev/nvme0n1p6") || !strings.Contains(dp, "FORMATTED  /dev/nvme0n1p5") {
		t.Errorf("the disk plan does not show the formatted partitions:\n%s", dp)
	}
}

func hasErr(is plan.Issues, field string) bool {
	for _, i := range is {
		if i.Severity == plan.Error && i.Field == field {
			return true
		}
	}
	return false
}

// The validator refuses to format Windows, NTFS or BitLocker data, the
// ESP and the adopted /home; LUKS and Linux file systems need a warning.
func TestReuseValidation(t *testing.T) {
	f := dualFacts()
	for _, c := range []struct {
		dev  string
		err  bool
		what string
	}{
		{"/dev/nvme0n1p3", true, "BitLocker C:"},
		{"/dev/nvme0n1p4", true, "NTFS data"},
		{"/dev/nvme0n1p2", true, "Microsoft reserved"},
		{"/dev/nvme0n1p1", true, "the ESP"},
		{"/dev/nvme1n1p1", true, "a partition of another disk"},
		{"/dev/nvme0n1p6", false, "the old Linux root (warning)"},
	} {
		p := dualPlan()
		p.Target.System = c.dev
		is := plan.Validate(p, &f, "")
		if got := hasErr(is, "target.system"); got != c.err {
			t.Errorf("%s (%s): error %v, want %v: %v", c.dev, c.what, got, c.err, is)
		}
	}
	p := dualPlan()
	p.Target.System = "/dev/nvme0n1p6"
	warned := false
	for _, i := range plan.Validate(p, &f, "") {
		warned = warned || (i.Severity == plan.Warning && i.Field == "target.system" && strings.Contains(i.Message, "everything on it is lost"))
	}
	if !warned {
		t.Error("reusing a Linux root needs a warning that its files are lost")
	}
	// A LUKS volume on the target disk: a warning, it is chosen.
	f.Disks[0].Partitions[5].FSType = "crypto_LUKS"
	if hasErr(plan.Validate(p, &f, ""), "target.system") {
		t.Error("a chosen LUKS partition may be reused (with a warning)")
	}
	// Reuse with wipe is refused.
	p.Target.Wipe = true
	if !hasErr(plan.Validate(p, &f, ""), "target.boot") {
		t.Error("reuse together with wipe must be refused")
	}
}

// CheckSafety catches any step that would write to an unchosen partition,
// another disk, or rewrite the table of a disk that keeps its partitions.
func TestCheckSafetyRefuses(t *testing.T) {
	f := dualFacts()
	r, list := generate(t, dualPlan(), f)
	for _, bad := range []Step{
		{Title: "format Windows", Argv: []string{"mkfs.btrfs", "--force", "/dev/nvme0n1p3"}},
		{Title: "wipe the data disk", Argv: []string{"wipefs", "--all", "/dev/nvme1n1"}},
		{Title: "zap", Argv: []string{"sgdisk", "--zap-all", "/dev/nvme0n1"}},
		{Title: "delete Fedora", Argv: []string{"sgdisk", "--delete=6", "/dev/nvme0n1"}},
		{Title: "other table", Argv: []string{"sgdisk", "--new=2:0:+1G", "/dev/nvme1n1"}},
		{Title: "luks over the home", Argv: []string{"cryptsetup", "luksFormat", "/dev/nvme1n1p1"}},
		{Title: "enroll on the home", Argv: []string{"systemd-cryptenroll", "--tpm2-device=auto", "/dev/nvme1n1p1"}},
		{Title: "dd", Argv: []string{"dd", "if=/dev/zero", "of=/dev/nvme0n1p4"}},
		{Title: "boot order", Argv: []string{"efibootmgr", "--bootorder", "0001"}},
		{Title: "mount NTFS", Argv: []string{"mount", "/dev/nvme0n1p4", "/mnt"}},
		{Title: "in chroot", Argv: []string{"chroot", "/mnt/sysroot", "mkfs.ext4", "/dev/nvme0n1p5"}},
	} {
		if err := CheckSafety(r, append(append([]Step{}, list...), bad)); err == nil {
			t.Errorf("%q passed the safety check", bad.Title)
		}
	}
	if err := CheckSafety(r, list); err != nil {
		t.Fatalf("the generated list itself: %v", err)
	}
}

// Erasing a disk that holds Windows says so, with model and serial.
func TestWipeNamesWhatIsLost(t *testing.T) {
	f := dualFacts()
	p := dualPlan()
	p.Target.Wipe = true
	found := false
	for _, i := range plan.Validate(p, &f, "") {
		found = found || (i.Field == "target.disk" && strings.Contains(i.Message, "WD Black SN770, serial W123") && strings.Contains(i.Message, "Windows"))
	}
	if !found {
		t.Error("no warning naming the disk and the system that is lost")
	}
	r, _ := generate(t, p, f)
	if dp := DiskPlan(r); !strings.Contains(dp, "ERASED") || !strings.Contains(dp, `lost: the system "Windows Boot Manager"`) {
		t.Errorf("disk plan:\n%s", dp)
	}
	// The other disk is untouched and its ESP-less LUKS volume is not
	// searched for boot loaders.
	for _, e := range r.OtherESPs {
		if e.Disk == p.Target.Disk {
			t.Errorf("the erased disk's ESP %s is searched", e.Path)
		}
	}
}

// Windows with TPM unlock: the review warns about PCR 7 changes; a db
// without Microsoft's certificates is reported.
func TestOtherSystemsWarnings(t *testing.T) {
	f := dualFacts()
	f.SecureBootDB = probe.SecureBootDB{Read: true}
	var fields []string
	for _, i := range plan.Validate(dualPlan(), &f, "") {
		fields = append(fields, i.Field+": "+i.Message)
	}
	all := strings.Join(fields, "\n")
	for _, want := range []string{"PCR 7", "no Microsoft Windows certificate", "no Microsoft UEFI CA"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestDesktopCmdlineEndsOnTheScreen(t *testing.T) {
	p := basePlan("/dev/vda")
	p.Edition = "desktop"
	r, _ := generate(t, p, facts("kvm", true))
	g := &gen{r: r}
	args := strings.Fields(g.kernelCmdline(false))
	last := ""
	for _, a := range args {
		if strings.HasPrefix(a, "console=") {
			last = a
		}
	}
	if last != "console=tty0" {
		t.Fatalf("desktop: last console is %q: %v", last, args)
	}
	p.Edition = "server"
	r, _ = generate(t, p, facts("kvm", true))
	g = &gen{r: r}
	if !strings.HasSuffix(strings.Split(g.kernelCmdline(false), " lockdown")[0], "console=ttyS0,115200n8") {
		t.Fatalf("server: %s", g.kernelCmdline(false))
	}
}

func TestStandardDesktopPackagesAndLanguage(t *testing.T) {
	r := Resolved{Profile: "standard", Plan: plan.Plan{Edition: "desktop", Locale: "pt_BR.UTF-8"}}
	install, _ := Packages(r)
	have := map[string]bool{}
	for _, p := range install {
		have[p] = true
	}
	for _, want := range append(append([]string{"glibc-langpack-pt", "glibc-langpack-en", "basalt-gate", "basalt-security-activity", "basalt-audit-suite"},
		StandardPackages...), StandardDesktopPackages...) {
		if !have[want] {
			t.Errorf("missing %s", want)
		}
	}
	r.Profile = "minimal"
	install, exclude := Packages(r)
	for _, p := range install {
		for _, e := range exclude {
			if p == e || (strings.HasSuffix(e, "*") && strings.HasPrefix(p, strings.TrimSuffix(strings.TrimPrefix(e, "*"), "*"))) ||
				(strings.HasPrefix(e, "*") && strings.HasSuffix(p, strings.TrimPrefix(e, "*"))) {
				t.Errorf("minimal installs %s and excludes %s", p, e)
			}
		}
	}
	if Langpack("C.UTF-8") != "" || Langpack("de_DE.UTF-8") != "glibc-langpack-de" {
		t.Error("Langpack")
	}
}

func TestBrazilianKeyboard(t *testing.T) {
	p := basePlan("/dev/vda")
	p.Keymap = "br-abnt2" // the legacy name works too
	_, list := generate(t, p, facts("kvm", true))
	_, vc := mustFind(t, list, "vconsole.conf")
	if !strings.Contains(vc.Write.Content, "KEYMAP=br\n") || !strings.Contains(vc.Write.Content, "XKBLAYOUT=br\n") || !strings.Contains(vc.Write.Content, "XKBMODEL=abnt2") {
		t.Fatalf("vconsole.conf: %q", vc.Write.Content)
	}
	_, x := mustFind(t, list, "00-keyboard.conf")
	if !strings.Contains(x.Write.Content, `Option "XkbLayout" "br"`) || !x.Write.Parents {
		t.Fatalf("00-keyboard.conf: %q", x.Write.Content)
	}
}
