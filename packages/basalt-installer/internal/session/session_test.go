package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

const lsblk = `{"blockdevices":[{"name":"vda","path":"/dev/vda","size":32212254720,"type":"disk","rm":false,"ro":false,"rota":true,"model":null,"serial":null,"tran":null,"fstype":null,"mountpoints":[null],"label":null},
{"name":"sdb","path":"/dev/sdb","size":8012345344,"type":"disk","rm":true,"ro":false,"rota":false,"model":"USB stick","serial":"U","tran":"usb","fstype":null,"mountpoints":[null],"label":null,
 "children":[{"name":"sdb1","path":"/dev/sdb1","size":8011296768,"type":"part","rm":true,"ro":false,"rota":false,"model":null,"serial":null,"tran":"usb","fstype":"vfat","mountpoints":[null],"label":"KEYS"}]}]}`

const key = "abcdefgh-ijklmnop-qrstuvwx"

type rig struct {
	ss       *Session
	cmdline  string
	written  map[string]string
	finished string
	mu       sync.Mutex
}

// newRig is a session on a made-up machine with a fake runner: nothing is
// executed, file writes land in a temporary directory.
func newRig(t *testing.T, planYAML, cmdline string) *rig {
	t.Helper()
	root, dir := t.TempDir(), t.TempDir()
	pf := filepath.Join(dir, "plan.yaml")
	files := map[string]string{
		"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c": "\x06\x00\x00\x00\x01",
		"sys/class/tpm/tpm0/tpm_version_major":                                     "2\n",
		"etc/os-release":                                                           "VERSION_ID=44\n",
		"proc/meminfo":                                                             "MemTotal: 4000000 kB\n",
		"proc/cmdline":                                                             strings.ReplaceAll(cmdline, "PLAN", pf),
	}
	for name, content := range files {
		_ = os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755)
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(pf, []byte(planYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"work/repos.d", "sysroot/etc/kernel", "sysroot/etc/default", "sysroot/etc/dnf/vars", "sysroot/etc/selinux",
		"sysroot/root/.ssh", "sysroot/etc/yum.repos.d", "sysroot/etc/pki/rpm-gpg", "sysroot/boot/efi/EFI/fedora", "sysroot/var/log/basalt-installer"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	r := &rig{written: map[string]string{}}
	r.ss = New(Options{
		Prober: probe.Prober{Root: root, Read: func(_ context.Context, argv ...string) (string, error) {
			if argv[0] == "lsblk" {
				return lsblk, nil
			}
			return "kvm\n", nil
		}},
		Steps:   steps.Options{Root: filepath.Join(dir, "sysroot"), Work: filepath.Join(dir, "work"), MediaDir: filepath.Join(dir, "media")},
		Engine:  engine.Options{Runner: &engine.FakeRunner{Captured: key + "\n"}, LogDir: filepath.Join(dir, "log"), LockPath: filepath.Join(dir, "lock"), LogNoSync: true},
		Cmdline: filepath.Join(root, "proc/cmdline"),
		Finisher: func(_ context.Context, action string) error {
			r.mu.Lock()
			r.finished = action
			r.mu.Unlock()
			return nil
		},
		KeyWriter: func(_ context.Context, m probe.KeyMedium, name string, content []byte) (string, error) {
			r.mu.Lock()
			r.written[m.Path+"/"+name] = string(content)
			r.mu.Unlock()
			return m.Path + ": " + name, nil
		},
	})
	return r
}

const basePlan = `apiVersion: basalt-install-plan/v1
target: {disk: /dev/vda, wipe: true}
accounts:
  root:
    ssh_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl lab"]
finish: poweroff
`

// run starts the unattended installation and returns its status once it
// has ended or is waiting for the person (the recovery key on the screen).
// It waits on the session's change notifications, never on a fixed delay.
func (r *rig) run(t *testing.T) AutoStatus {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { r.ss.RunUnattended(ctx, time.Second); close(done) }()
	r.waitFor(t, done, func(st Status) bool {
		return st.Unattended.State == AutoRunning && st.State == Succeeded && st.RecoveryKey
	})
	return r.ss.Status().Unattended
}

// waitFor blocks until cond holds for the session status or stop is
// closed (nil: never). The deadline only turns a hang into a failure; it
// is not part of the expectation.
func (r *rig) waitFor(t *testing.T, stop <-chan struct{}, cond func(Status) bool) {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	for {
		changed := r.ss.changes()
		if cond(r.ss.Status()) {
			return
		}
		select {
		case <-changed:
		case <-stop:
			return
		case <-deadline:
			t.Fatalf("timed out; status %+v", r.ss.Status())
		}
	}
}

func (r *rig) finishedAction() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

func TestUnattendedNeedsTheConfirmation(t *testing.T) {
	// The plan alone only prefills the wizard.
	r := newRig(t, basePlan, "basalt.inst.plan=PLAN")
	if r.ss.UnattendedRequested() {
		t.Fatal("a plan without basalt.inst.confirm must not be unattended")
	}
	r.ss.RunUnattended(context.Background(), time.Second)
	if st := r.ss.Status(); st.State != Idle || st.Unattended.Requested {
		t.Fatalf("status: %+v", st)
	}
	// A confirmation that names another disk is refused before anything runs.
	r = newRig(t, basePlan, "basalt.inst.plan=PLAN basalt.inst.confirm=sdb")
	a := r.run(t)
	if a.State != AutoRefused || !strings.Contains(a.Error, "/dev/vda") || r.ss.Status().State != Idle {
		t.Fatalf("mismatched confirmation: %+v", a)
	}
	// A confirmation without a plan is refused.
	r = newRig(t, basePlan, "basalt.inst.confirm=vda")
	if a := r.run(t); a.State != AutoRefused || !strings.Contains(a.Error, "basalt.inst.plan") {
		t.Fatalf("confirmation without a plan: %+v", a)
	}
}

func TestUnattendedWaitsForTheRecoveryKey(t *testing.T) {
	r := newRig(t, basePlan, "basalt.inst.plan=PLAN basalt.inst.confirm=vda")
	a := r.run(t)
	st := r.ss.Status()
	if a.State != AutoRunning || st.State != Succeeded || !st.RecoveryKey {
		t.Fatalf("unattended status %+v, session %+v", a, st)
	}
	if r.finishedAction() != "" {
		t.Fatal("finished before the recovery key was acknowledged")
	}
	r.mu.Lock()
	written := len(r.written)
	r.mu.Unlock()
	if written != 0 {
		t.Fatal("the key was written somewhere without being asked to")
	}
	// A copy on a USB stick on request; not an acknowledgement.
	where, err := r.ss.SaveRecoveryKey(context.Background(), "/dev/sdb1")
	if err != nil || !strings.Contains(where, "/dev/sdb1") {
		t.Fatalf("save: %q %v", where, err)
	}
	if _, err := r.ss.SaveRecoveryKey(context.Background(), "/dev/vda"); err == nil {
		t.Fatal("the target disk is not removable media")
	}
	r.mu.Lock()
	for _, c := range r.written {
		if !strings.Contains(c, "\n"+key+"\n") {
			t.Fatalf("key file: %q", c)
		}
	}
	r.mu.Unlock()
	if !r.ss.Status().RecoveryKey {
		t.Fatal("saving a copy must not count as the acknowledgement")
	}
	if err := r.ss.AckRecoveryKey("abcdefgh"); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, nil, func(st Status) bool { return st.Unattended.State == AutoDone || st.Unattended.State == AutoFailed })
	if r.finishedAction() != "poweroff" || r.ss.Status().Unattended.State != AutoDone {
		t.Fatalf("end action %q, status %+v", r.finishedAction(), r.ss.Status().Unattended)
	}
}

func TestUnattendedWithKeyMediaNeedsNobody(t *testing.T) {
	r := newRig(t, basePlan+"encryption: {recovery_key_media: KEYS}\n", "basalt.inst.plan=PLAN basalt.inst.confirm=vda")
	a := r.run(t)
	st := r.ss.Status()
	if a.State != AutoDone || r.finishedAction() != "poweroff" || !st.RecoveryAcked || len(st.KeySaved) != 1 {
		t.Fatalf("unattended %+v, session %+v, finished %q", a, st, r.finishedAction())
	}
	if r.ss.RecoveryKey() != "" {
		t.Fatal("the key stays in memory after it was written")
	}
}

func TestKeyMediaMustExist(t *testing.T) {
	r := newRig(t, basePlan+"encryption: {recovery_key_media: ELSEWHERE}\n", "basalt.inst.plan=PLAN basalt.inst.confirm=vda")
	if a := r.run(t); a.State != AutoRefused || !strings.Contains(a.Error, "ELSEWHERE") {
		t.Fatalf("missing key medium: %+v", a)
	}
}

// A frontend reacts to the "done" event (finish, status, retry): by then
// the session must already report the end, never "installing". The state
// is read the moment the event arrives, while the installation goroutine
// may still be running.
func TestStateIsSettledWhenDoneIsHeard(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := newRig(t, basePlan, "basalt.inst.plan=PLAN basalt.inst.confirm=vda")
		events, stop := r.ss.Subscribe()
		ctx, cancel := context.WithCancel(context.Background())
		ended := make(chan struct{})
		go func() { r.ss.RunUnattended(ctx, time.Second); close(ended) }()
		deadline := time.After(2 * time.Minute) // a hang guard only
	events:
		for {
			select {
			case e := <-events:
				if e.Type != engine.EvDone {
					continue
				}
				if st := r.ss.Status(); !e.OK || st.State != Succeeded || st.FinishBlockers != "confirm that you stored the recovery key" {
					t.Fatalf("done %+v heard with session %+v", e, st)
				}
				break events
			case <-deadline:
				t.Fatalf("no done event; session %+v", r.ss.Status())
			}
		}
		stop()
		cancel()
		<-ended
	}
}
