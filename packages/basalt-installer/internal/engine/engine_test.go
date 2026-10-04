package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/auditlog"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl lab"

// testSteps generates the real step list for a default plan, with the
// target and work directories inside a temporary directory so the file
// writes land there.
func testSteps(t *testing.T, mutate func(*plan.Plan)) ([]steps.Step, string) {
	t.Helper()
	dir := t.TempDir()
	p := plan.Default("/dev/vda")
	p.Target.Wipe = true
	p.Accounts.Root.SSHKeys = []string{key}
	p.Accounts.User = &plan.User{Name: "ana", Password: "user-password-1"}
	p.Repos.Basalt.URL = "http://10.0.2.2:8098"
	if mutate != nil {
		mutate(&p)
	}
	f := probe.Facts{UEFI: true, SecureBoot: "enabled", TPM2: true, Virt: "kvm", Arch: "x86_64", Release: 44,
		Disks: []probe.Disk{{Name: "vda", Path: "/dev/vda", SizeBytes: 30 << 30, Contents: "empty"}}}
	r, err := steps.Resolve(p, f, steps.Options{Root: filepath.Join(dir, "sysroot"), Work: filepath.Join(dir, "work"),
		Random: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 64))})
	if err != nil {
		t.Fatal(err)
	}
	list, err := steps.Generate(r)
	if err != nil {
		t.Fatal(err)
	}
	// The fake runner executes nothing, so create the directories the
	// commands would have created for the file writes.
	for _, d := range []string{"work/repos.d", "sysroot/etc/kernel", "sysroot/etc/default", "sysroot/etc/dnf/vars",
		"sysroot/etc/selinux", "sysroot/root/.ssh", "sysroot/etc/yum.repos.d", "sysroot/etc/pki/rpm-gpg",
		"sysroot/boot/efi/EFI/fedora", "sysroot/var/log/basalt-installer", "sysroot/home/ana/.ssh"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return list, dir
}

func newEngine(dir string, fr *FakeRunner) *Engine {
	return New(Options{Runner: fr, LogDir: filepath.Join(dir, "log"), LockPath: filepath.Join(dir, "lock")})
}

func collect(evs *[]Event) func(Event) { return func(e Event) { *evs = append(*evs, e) } }

func TestRunSuccessRecoveryKeyAndLog(t *testing.T) {
	list, dir := testSteps(t, nil)
	const rk = "fjkl-dhgr-uvbn-tckr-hnil-jbvc-ldie-kgnb"
	fr := &FakeRunner{Captured: rk + "\n",
		Output: map[string][]string{"dnf --assumeyes": {"[ 1/4] bash 100%", "[ 4/4] Installing bash", "echo user-password-1 leaked"}}}
	var evs []Event
	if err := newEngine(dir, fr).Run(context.Background(), list, collect(&evs)); err != nil {
		t.Fatal(err)
	}
	secrets, last := 0, evs[len(evs)-1]
	if last.Type != EvDone || !last.OK {
		t.Fatalf("last event: %+v", last)
	}
	var progress float64
	for _, e := range evs {
		if e.Type == EvSecret {
			secrets++
			if e.Secret != rk || e.Kind != SecretRecKey {
				t.Fatalf("secret event: %+v", e)
			}
		}
		if e.Type == EvOutput && strings.Contains(e.Text, "user-password-1") {
			t.Fatal("a password reached the output events")
		}
		if e.Type == EvProgress {
			if e.Fraction+1e-9 < progress {
				t.Fatalf("progress went backwards: %f after %f", e.Fraction, progress)
			}
			progress = e.Fraction
		}
	}
	if secrets != 1 || progress < 0.999 {
		t.Fatalf("secrets %d, final progress %f", secrets, progress)
	}
	logData, err := os.ReadFile(last.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{rk, "user-password-1"} {
		if strings.Contains(string(logData), s) {
			t.Fatalf("the install log contains a secret: %q", s)
		}
	}
	n, err := auditlog.Verify(bytes.NewReader(logData))
	if err != nil || n < len(list) {
		t.Fatalf("log verify: %d records, %v", n, err)
	}
	// The copy written into the target is a valid chain too.
	copyData, err := os.ReadFile(filepath.Join(dir, "sysroot/var/log/basalt-installer/install.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auditlog.Verify(bytes.NewReader(copyData)); err != nil {
		t.Fatalf("target copy of the log: %v", err)
	}
	// The temporary key file was written with random content and mode 0600.
	st, err := os.Stat(filepath.Join(dir, "work/luks.key"))
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 64 {
		t.Fatalf("temporary key: %v %v", st, err)
	}
	// Commands ran in the generated order, with the real argv.
	cmds := fr.Commands()
	if !strings.HasPrefix(cmds[0], "udevadm settle") || !strings.Contains(strings.Join(cmds, "\n"), "chroot "+filepath.Join(dir, "sysroot")+" chpasswd") {
		t.Fatalf("commands: %v", cmds[:3])
	}
}

func TestFailureRollsBackInReverse(t *testing.T) {
	list, dir := testSteps(t, nil)
	fr := &FakeRunner{Fail: map[string]string{"dnf --assumeyes": "no more mirrors to try"}}
	var evs []Event
	err := newEngine(dir, fr).Run(context.Background(), list, collect(&evs))
	if err == nil || !strings.Contains(err.Error(), "no more mirrors") {
		t.Fatalf("expected the dnf failure, got %v", err)
	}
	cmds := fr.Commands()
	var rb []string
	for i, c := range cmds {
		if strings.HasPrefix(c, "dnf --assumeyes") {
			rb = cmds[i+1:]
		}
	}
	sysroot := filepath.Join(dir, "sysroot")
	want := []string{"umount --recursive " + sysroot, "cryptsetup close luks-", "rm -f " + filepath.Join(dir, "work/luks.key")}
	if len(rb) != len(want) {
		t.Fatalf("rollback commands: %v", rb)
	}
	for i := range want {
		if !strings.HasPrefix(rb[i], want[i]) {
			t.Fatalf("rollback %d: got %q, want prefix %q", i, rb[i], want[i])
		}
	}
	for _, c := range rb {
		if strings.Contains(c, "btrfs-top") {
			t.Fatal("the top-level mount was released by its unmount step and must not be undone again")
		}
	}
	last := evs[len(evs)-1]
	if last.Type != EvDone || last.OK {
		t.Fatalf("last event: %+v", last)
	}
}

func TestOptionalFailureIsAWarning(t *testing.T) {
	list, dir := testSteps(t, nil)
	fr := &FakeRunner{Captured: "x-key", Fail: map[string]string{"efibootmgr --create": "no EFI variables"}}
	var evs []Event
	if err := newEngine(dir, fr).Run(context.Background(), list, collect(&evs)); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Type == EvWarning && strings.Contains(e.Text, "no EFI variables") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a warning event for the optional step")
	}
}

func TestCancelRollsBack(t *testing.T) {
	list, dir := testSteps(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	fr := &FakeRunner{}
	var evs []Event
	err := newEngine(dir, fr).Run(ctx, list, func(e Event) {
		evs = append(evs, e)
		if e.Type == EvStepStart && strings.Contains(e.Command, "mkfs.btrfs") {
			cancel()
		}
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected a cancellation, got %v", err)
	}
	cmds := fr.Commands()
	if !strings.HasPrefix(cmds[len(cmds)-2], "cryptsetup close") || !strings.HasPrefix(cmds[len(cmds)-1], "rm -f") {
		t.Fatalf("rollback after cancel: %v", cmds[len(cmds)-3:])
	}
}

func TestSecondInstallIsRefused(t *testing.T) {
	list, dir := testSteps(t, nil)
	lockPath := filepath.Join(dir, "lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := newEngine(dir, &FakeRunner{}).Run(context.Background(), list, nil); err != ErrBusy {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
}

func TestDNFProgressParser(t *testing.T) {
	p := progressParser("dnf")
	f, ok := p("[ 10/100] kernel-core-6.19 100% | 30 MiB/s")
	if !ok || f < 0.03 || f > 0.04 {
		t.Fatalf("download line: %f %v", f, ok)
	}
	f, _ = p("[100/100] glibc 100%")
	f2, _ := p("[ 50/100] Installing bash-5.3")
	if f2 <= f || f2 < 0.67 || f2 > 0.68 {
		t.Fatalf("install line: %f after %f", f2, f)
	}
	if _, ok := p("Transaction Summary:"); ok {
		t.Fatal("not a counter line")
	}
}
