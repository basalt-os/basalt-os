package steps

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/pgpkey"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

var update = flag.Bool("update", false, "rewrite the golden previews in testdata/")

const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl lab"

func facts(virt string, tpm bool) probe.Facts {
	return probe.Facts{UEFI: true, SecureBoot: "enabled", TPM2: tpm, Virt: virt, Arch: "x86_64", MemoryMiB: 4096, Release: 44,
		Disks: []probe.Disk{
			{Name: "vda", Path: "/dev/vda", SizeBytes: 30 << 30, Contents: "empty"},
			{Name: "nvme0n1", Path: "/dev/nvme0n1", SizeBytes: 500 << 30, Contents: "empty"},
		}}
}

func basePlan(disk string) plan.Plan {
	p := plan.Default(disk)
	p.Target.Wipe = true
	p.Accounts.Root.SSHKeys = []string{key}
	p.Repos.Basalt.URL = "http://10.0.2.2:8098"
	return p
}

// fixed makes the generated identifiers deterministic.
func fixed() Options {
	return Options{Random: bytes.NewReader(bytes.Repeat([]byte{0xab, 0x12, 0x5c, 0x07}, 64)), InstallerVersion: "0.1.0"}
}

func generate(t *testing.T, p plan.Plan, f probe.Facts) (Resolved, []Step) {
	t.Helper()
	r, err := Resolve(p, f, fixed())
	if err != nil {
		t.Fatal(err)
	}
	list, err := Generate(r)
	if err != nil {
		t.Fatal(err)
	}
	return r, list
}

func find(list []Step, substr string) (int, Step) {
	for i, s := range list {
		if strings.Contains(s.Command(), substr) {
			return i, s
		}
	}
	return -1, Step{}
}

func mustFind(t *testing.T, list []Step, substr string) (int, Step) {
	t.Helper()
	i, s := find(list, substr)
	if i < 0 {
		t.Fatalf("no step with %q", substr)
	}
	return i, s
}

func TestDefaultEncryptedTPM(t *testing.T) {
	r, list := generate(t, basePlan("/dev/vda"), facts("kvm", true))
	if r.Profile != "minimal" || r.ProfileReason != "auto, virtual machine: kvm" {
		t.Fatalf("auto profile on a VM: %s (%s)", r.Profile, r.ProfileReason)
	}
	// Nothing destructive before the preflight is done.
	zap, _ := mustFind(t, list, "sgdisk --zap-all /dev/vda")
	for _, s := range list[:zap] {
		if s.Phase != "preflight" {
			t.Fatalf("step before the disk wipe outside preflight: %s", s.Title)
		}
	}
	_, part := mustFind(t, list, "sgdisk --clear")
	want := "sgdisk --clear --new=1:0:+600M --typecode=1:EF00 '--change-name=1:EFI System Partition' --new=2:0:+1024M --typecode=2:8300 --change-name=2:boot --new=3:0:0 --typecode=3:8309 --change-name=3:basalt /dev/vda"
	if part.Command() != want {
		t.Fatalf("partitioning:\n got %s\nwant %s", part.Command(), want)
	}
	mustFind(t, list, "cryptsetup luksFormat --batch-mode --type luks2 --uuid "+r.LUKSUUID)
	mustFind(t, list, "mkfs.btrfs --force --label basalt --uuid "+r.BtrfsUUID+" /dev/mapper/luks-"+r.LUKSUUID)
	enroll, _ := mustFind(t, list, "--tpm2-device=auto --tpm2-pcrs=7 /dev/vda3")
	rec, recStep := mustFind(t, list, "--recovery-key /dev/vda3")
	if recStep.Capture != "recovery_key" {
		t.Fatal("the recovery key must be captured, not printed")
	}
	check, _ := mustFind(t, list, "--test-passphrase --disable-external-tokens --key-slot 0")
	wipe, _ := mustFind(t, list, "--wipe-slot=0")
	if !(enroll < rec && rec < check && check < wipe) {
		t.Fatalf("unlock order: enroll %d, recovery %d, check %d, wipe %d", enroll, rec, check, wipe)
	}
	dracut, _ := mustFind(t, list, "dracut --force --regenerate-all")
	snap, _ := mustFind(t, list, "basalt-snapshots-setup --no-initial-snapshot")
	relabel, _ := mustFind(t, list, "setfiles -F")
	umount, _ := mustFind(t, list, "umount --recursive /mnt/sysroot")
	if !(wipe < dracut && dracut < snap && snap < relabel && relabel < umount) {
		t.Fatalf("final order: wipe %d dracut %d snapshots %d relabel %d umount %d", wipe, dracut, snap, relabel, umount)
	}
	_, ct := mustFind(t, list, "write /mnt/sysroot/etc/crypttab")
	if ct.Write.Content != "luks-"+r.LUKSUUID+" UUID="+r.LUKSUUID+" none discard,tpm2-device=auto\n" {
		t.Fatalf("crypttab: %q", ct.Write.Content)
	}
	_, cmdline := mustFind(t, list, "write /mnt/sysroot/etc/kernel/cmdline")
	for _, a := range []string{"rd.luks.uuid=luks-" + r.LUKSUUID, "lockdown=integrity", "module.sig_enforce=1", "console=ttyS0,115200n8", "rootflags=subvol=root"} {
		if !strings.Contains(cmdline.Write.Content, a) {
			t.Fatalf("kernel command line lacks %s: %s", a, cmdline.Write.Content)
		}
	}
	_, fstab := mustFind(t, list, "write /mnt/sysroot/etc/fstab")
	for _, l := range []string{"UUID=" + r.BtrfsUUID + " / btrfs subvol=root,compress=zstd:1 0 0",
		"UUID=" + r.BtrfsUUID + " /var/lib/mysql btrfs subvol=var_lib_mysql,compress=zstd:1 0 0",
		"UUID=" + r.ESPUUID() + " /boot/efi vfat"} {
		if !strings.Contains(fstab.Write.Content, l) {
			t.Fatalf("fstab lacks %q:\n%s", l, fstab.Write.Content)
		}
	}
	_, dnf := mustFind(t, list, "dnf --assumeyes --installroot=/mnt/sysroot")
	for _, a := range []string{"--releasever=44", "--exclude=fedora-release", "--exclude=linux-firmware", "basalt-assistant", "basalt-assistant-selinux", "basalt-resolver-selinux", "basalt-ledger-selinux", "basalt-prompt", "shim-x64", "@core"} {
		if !strings.Contains(dnf.Command(), a) {
			t.Fatalf("dnf command lacks %s", a)
		}
	}
	mustFind(t, list, "systemctl set-default multi-user.target")
	_, svc := mustFind(t, list, "systemctl enable")
	for _, u := range []string{"basalt-assistantd", "basalt-initial-snapshot", "sshd", "firewalld", "basalt-audit-rotate.timer", "basalt-resolver", "basalt-ledger"} {
		if !strings.Contains(svc.Command(), u) {
			t.Fatalf("service %s not enabled", u)
		}
	}
	// The temporary key is removed and its undo released.
	_, del := mustFind(t, list, "rm -f /run/basalt-installer/luks.key")
	if del.Release != "luks-key" {
		t.Fatal("deleting the temporary key must release its undo")
	}
}

func TestPlainAndBareMetal(t *testing.T) {
	p := basePlan("/dev/nvme0n1")
	p.Encryption.Enabled = plan.Bool(false)
	r, list := generate(t, p, facts("", false))
	if r.Profile != "standard" {
		t.Fatalf("bare metal gets the standard profile, got %s", r.Profile)
	}
	if i, _ := find(list, "cryptsetup luksFormat"); i >= 0 {
		t.Fatal("no cryptsetup without encryption")
	}
	if i, _ := find(list, "crypttab"); i >= 0 {
		t.Fatal("no crypttab without encryption")
	}
	mustFind(t, list, "mkfs.vfat -F 32 -n EFI -i ")
	mustFind(t, list, "/dev/nvme0n1p1")
	mustFind(t, list, "mkfs.btrfs --force --label basalt --uuid "+r.BtrfsUUID+" /dev/nvme0n1p3")
	_, dnf := mustFind(t, list, "dnf --assumeyes")
	if strings.Contains(dnf.Command(), "linux-firmware") {
		t.Fatal("the standard profile keeps the firmware")
	}
	_, part := mustFind(t, list, "sgdisk --clear")
	if !strings.Contains(part.Command(), "--typecode=3:8300") {
		t.Fatal("a plain system partition is a Linux file system partition")
	}
}

func TestNoTPMIsRecoveryOnly(t *testing.T) {
	r, list := generate(t, basePlan("/dev/vda"), facts("kvm", false))
	if r.EnrollTPM {
		t.Fatal("no TPM, nothing to enroll")
	}
	if i, _ := find(list, "--tpm2-device"); i >= 0 {
		t.Fatal("no TPM enrollment without a TPM")
	}
	_, ct := mustFind(t, list, "write /mnt/sysroot/etc/crypttab")
	if strings.Contains(ct.Write.Content, "tpm2") {
		t.Fatalf("crypttab without TPM: %q", ct.Write.Content)
	}
	mustFind(t, list, "--recovery-key")
}

func TestTangAndManualLayout(t *testing.T) {
	p := basePlan("/dev/vda")
	p.Encryption.Unlock = "tpm2+tang"
	p.Encryption.Tang = plan.Tang{URL: "http://tang.example:7500", Thumbprint: "abc"}
	p.Layout = plan.Layout{Mode: "manual", ESPMiB: 512, BootMiB: 2048, RootGiB: 20,
		Subvolumes: []string{"root", "home", "var_log", "var_tmp"}}
	p.ApplyDefaults()
	r, list := generate(t, p, facts("kvm", true))
	_, bind := mustFind(t, list, "clevis luks bind -d /dev/vda3")
	if !strings.Contains(bind.Command(), `"pcr_ids":"7"`) || !strings.Contains(bind.Command(), `"thp":"abc"`) {
		t.Fatalf("sss binding: %s", bind.Command())
	}
	_, cmdline := mustFind(t, list, "etc/kernel/cmdline")
	if !strings.Contains(cmdline.Write.Content, "rd.neednet=1") {
		t.Fatal("Tang needs the network in the initramfs")
	}
	_, dnf := mustFind(t, list, "dnf --assumeyes")
	if !strings.Contains(dnf.Command(), "clevis-dracut") {
		t.Fatal("Clevis packages for Tang")
	}
	_, part := mustFind(t, list, "sgdisk --clear")
	if !strings.Contains(part.Command(), "--new=1:0:+512M") || !strings.Contains(part.Command(), "--new=3:0:+20G") {
		t.Fatalf("manual sizes: %s", part.Command())
	}
	if i, _ := find(list, "var_lib_mysql"); i >= 0 {
		t.Fatal("subvolume left out of a manual layout was created")
	}
	_ = r
}

func TestSecretsNeverInPreview(t *testing.T) {
	p := basePlan("/dev/vda")
	p.Accounts.Root.Password = "root-password-1"
	p.Accounts.User = &plan.User{Name: "ana", Password: "user-password-1", SSHKeys: []string{key}}
	p.Encryption.Passphrase = "disk passphrase 123"
	p.ApplyDefaults()
	_, list := generate(t, p, facts("kvm", true))
	text := Preview(list)
	for _, s := range []string{"root-password-1", "user-password-1", "disk passphrase 123"} {
		if strings.Contains(text, s) {
			t.Fatalf("preview leaks %q", s)
		}
	}
	_, pw := mustFind(t, list, "chpasswd  <<< ana:<password>")
	if pw.Stdin != "ana:user-password-1\n" {
		t.Fatalf("stdin of chpasswd: %q", pw.Stdin)
	}
	_, np := mustFind(t, list, "NEWPASSWORD='<passphrase>'")
	if np.Env[0].Value != "disk passphrase 123" {
		t.Fatal("the passphrase reaches systemd-cryptenroll through its environment")
	}
	mustFind(t, list, "useradd --create-home --groups wheel ana")
}

func TestTUIToolsKeyIsPinned(t *testing.T) {
	fpr, err := pgpkey.Fingerprint(TUIToolsKey)
	if err != nil {
		t.Fatal(err)
	}
	if fpr != TUIToolsFingerprint {
		t.Fatalf("embedded tui-tools key %s, pinned %s", fpr, TUIToolsFingerprint)
	}
	p := basePlan("/dev/vda")
	p.Repos.ThirdParty.TUITools = plan.Bool(false)
	p.Repos.Tools = plan.Bool(false)
	_, list := generate(t, p, facts("kvm", true))
	if i, _ := find(list, "tui-tools"); i >= 0 {
		t.Fatal("tui-tools disabled in the plan but configured")
	}
	_, tools := mustFind(t, list, "repos.override.d/80-basalt-installer.repo")
	if !strings.Contains(tools.Write.Content, "[basalt-tools]\nenabled=0") {
		t.Fatal("basalt-tools disabled in the plan")
	}
	// With the default plan basalt-release's own [basalt-tools] stays on.
	_, def := generate(t, basePlan("/dev/vda"), facts("kvm", true))
	if i, _ := find(def, "repos.override.d"); i >= 0 {
		t.Fatal("basalt-tools turned off in the default plan")
	}
	if i, _ := find(def, "basalt-tools.repo"); i >= 0 {
		t.Fatal("the installer writes its own basalt-tools.repo next to basalt-release's")
	}
}

func TestPartitionPath(t *testing.T) {
	for in, want := range map[string]string{"/dev/vda": "/dev/vda3", "/dev/sda": "/dev/sda3", "/dev/nvme0n1": "/dev/nvme0n1p3", "/dev/mmcblk0": "/dev/mmcblk0p3"} {
		if got := PartitionPath(in, 3); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestResolveRefusesInvalidPlans(t *testing.T) {
	p := basePlan("/dev/vda")
	p.Target.Wipe = false
	if _, err := Resolve(p, facts("kvm", true), fixed()); err == nil {
		t.Fatal("an invalid plan must not resolve")
	}
}

// TestGoldenPreview keeps the full step list of the default plan in
// testdata/, so a change to the generated installation shows up in review.
func TestGoldenPreview(t *testing.T) {
	_, list := generate(t, basePlan("/dev/vda"), facts("kvm", true))
	got := Preview(list)
	path := filepath.Join("testdata", "preview-default.txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/steps -update)", err)
	}
	if got != string(want) {
		t.Fatalf("preview changed; review and run go test ./internal/steps -update\n%s", got)
	}
}

// The OpenBasalt release key (public, from https://obpkg.org/keys/) is
// recognised; any other key is reported as not the release key.
func TestDescribeBasaltKey(t *testing.T) {
	got := describeBasaltKey(filepath.Join("testdata", "openbasalt-release-key.asc"))
	if got != "OpenBasalt release key "+OpenBasaltReleaseFingerprint {
		t.Fatalf("release key: %q", got)
	}
	other := describeBasaltKey(filepath.Join("RPM-GPG-KEY-tui-tools"))
	if !strings.Contains(other, TUIToolsFingerprint) || !strings.Contains(other, "not the OpenBasalt release key") {
		t.Fatalf("other key: %q", other)
	}
	if missing := describeBasaltKey("/nonexistent/key"); !strings.Contains(missing, "not readable") {
		t.Fatalf("missing key: %q", missing)
	}
}
