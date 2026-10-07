package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

const lsblk = `{"blockdevices":[{"name":"vda","path":"/dev/vda","size":32212254720,"type":"disk","rm":false,"ro":false,"rota":true,"model":null,"serial":null,"tran":null,"fstype":null,"mountpoints":[null],"label":null}]}`

// fakeMachine builds a root tree with UEFI, Secure Boot on and a TPM 2.0.
func fakeMachine(t *testing.T) string {
	root := t.TempDir()
	for _, d := range []string{"sys/firmware/efi/efivars", "sys/class/tpm/tpm0", "etc", "proc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c": "\x06\x00\x00\x00\x01",
		"sys/class/tpm/tpm0/tpm_version_major":                                     "2\n",
		"etc/os-release":                                                           "NAME=Fedora\nVERSION_ID=44\n",
		"proc/meminfo":                                                             "MemTotal: 4000000 kB\n",
		"proc/cmdline":                                                             "console=ttyS0 basalt.inst.repo=http://10.0.2.2:8098 basalt.inst.hostname=lab1",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type client struct {
	t   *testing.T
	c   net.Conn
	sc  *bufio.Scanner
	id  int
	evs chan map[string]any
	res chan Response
}

func dial(t *testing.T, path string) *client {
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	cl := &client{t: t, c: c, sc: bufio.NewScanner(c), evs: make(chan map[string]any, 1024), res: make(chan Response, 16)}
	cl.sc.Buffer(make([]byte, 64*1024), 8<<20)
	go func() {
		for cl.sc.Scan() {
			var probeMsg map[string]json.RawMessage
			_ = json.Unmarshal(cl.sc.Bytes(), &probeMsg)
			if ev, ok := probeMsg["event"]; ok {
				var e map[string]any
				_ = json.Unmarshal(ev, &e)
				cl.evs <- e
				continue
			}
			var r Response
			_ = json.Unmarshal(cl.sc.Bytes(), &r)
			cl.res <- r
		}
	}()
	return cl
}

func (cl *client) call(op string, args any) Response {
	cl.id++
	b, _ := json.Marshal(map[string]any{"id": cl.id, "op": op, "args": args})
	if _, err := cl.c.Write(append(b, '\n')); err != nil {
		cl.t.Fatal(err)
	}
	select {
	case r := <-cl.res:
		return r
	case <-time.After(time.Minute):
		// A hang guard, not an expectation: slow CI runners stay green.
		cl.t.Fatalf("no answer to %s", op)
	}
	return Response{}
}

func TestInstallOverTheSocket(t *testing.T) {
	root := fakeMachine(t)
	dir := t.TempDir()
	var finishMu sync.Mutex
	finished := ""
	finishedAction := func() string {
		finishMu.Lock()
		defer finishMu.Unlock()
		return finished
	}
	fr := &engine.FakeRunner{Captured: "abcdefgh-ijklmnop-qrstuvwx\n", Delay: time.Millisecond}
	ss := session.New(session.Options{
		Prober: probe.Prober{Root: root, Read: func(_ context.Context, argv ...string) (string, error) {
			if argv[0] == "lsblk" {
				return lsblk, nil
			}
			return "kvm\n", nil
		}},
		Steps:   steps.Options{Root: filepath.Join(dir, "sysroot"), Work: filepath.Join(dir, "work")},
		Engine:  engine.Options{Runner: fr, LogDir: filepath.Join(dir, "log"), LockPath: filepath.Join(dir, "lock"), LogNoSync: true},
		Cmdline: filepath.Join(root, "proc/cmdline"),
		Finisher: func(_ context.Context, action string) error {
			finishMu.Lock()
			finished = action
			finishMu.Unlock()
			return nil
		},
	})
	// File writes need their directories (the fake runner creates nothing).
	for _, d := range []string{"work/repos.d", "sysroot/etc/kernel", "sysroot/etc/default", "sysroot/etc/dnf/vars", "sysroot/etc/selinux",
		"sysroot/root/.ssh", "sysroot/etc/yum.repos.d", "sysroot/etc/pki/rpm-gpg", "sysroot/boot/efi/EFI/fedora", "sysroot/var/log/basalt-installer"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	sock := filepath.Join(dir, "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &Server{Session: ss, AllowUIDs: []int{os.Getuid()}, Logf: t.Logf}
	// The socket exists before the server goroutine starts: no wait.
	l, err := Bind(sock, -1)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() { _ = srv.Serve(ctx, l); close(served) }()
	t.Cleanup(func() { cancel(); <-served })
	cl := dial(t, sock)

	r := cl.call("suggest", nil)
	if !r.OK {
		t.Fatal(r.Error)
	}
	var sug struct {
		Plan  plan.Plan
		Facts probe.Facts
	}
	b, _ := json.Marshal(r.Result)
	_ = json.Unmarshal(b, &sug)
	if sug.Plan.Target.Disk != "/dev/vda" || sug.Plan.Repos.Basalt.URL != "http://10.0.2.2:8098" || sug.Plan.Hostname != "lab1" || !sug.Facts.TPM2 {
		t.Fatalf("suggestion: %+v %+v", sug.Plan, sug.Facts)
	}
	p := sug.Plan
	if r := cl.call("preview", map[string]any{"plan": p}); r.OK || !strings.Contains(r.Error, "target.wipe") {
		t.Fatalf("a suggested plan must not be installable before the wipe is confirmed: %+v", r)
	}
	p.Target.Wipe = true
	p.Accounts.Root.SSHKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl lab"}
	p.Accounts.User = &plan.User{Name: "ana", Password: "secret-password"}
	r = cl.call("preview", map[string]any{"plan": p})
	if !r.OK {
		t.Fatal(r.Error)
	}
	res := r.Result.(map[string]any)
	token, text := res["token"].(string), res["text"].(string)
	if res["confirm_word"] != "vda" || !strings.Contains(text, "sgdisk --zap-all /dev/vda") || strings.Contains(text, "secret-password") {
		t.Fatalf("preview result: confirm %v, text has the plan: %v", res["confirm_word"], strings.Contains(text, "sgdisk"))
	}
	if strings.Contains(string(b), "secret-password") {
		t.Fatal("secret in a response")
	}
	if r := cl.call("install", map[string]any{"token": "stale", "confirm": "vda"}); r.OK {
		t.Fatal("an install with a stale token must be refused")
	}
	if r := cl.call("install", map[string]any{"token": token, "confirm": "yes"}); r.OK || !strings.Contains(r.Error, `"vda"`) {
		t.Fatalf("an install without typing the disk name must be refused: %+v", r)
	}
	if r := cl.call("subscribe", nil); !r.OK {
		t.Fatal(r.Error)
	}
	if r := cl.call("install", map[string]any{"token": token, "confirm": "vda"}); !r.OK {
		t.Fatal(r.Error)
	}
	gotKey := ""
	deadline := time.After(2 * time.Minute) // a hang guard only
wait:
	for {
		select {
		case e := <-cl.evs:
			if e["type"] == "secret" {
				gotKey = e["secret"].(string)
			}
			if e["type"] == "done" {
				if e["ok"] != true {
					t.Fatalf("install failed: %v", e)
				}
				break wait
			}
		case <-deadline:
			t.Fatal("no done event")
		}
	}
	if gotKey != "abcdefgh-ijklmnop-qrstuvwx" {
		t.Fatalf("recovery key event: %q", gotKey)
	}
	if r := cl.call("finish", nil); r.OK {
		t.Fatal("finish must wait for the recovery key acknowledgement")
	}
	if r := cl.call("ack_recovery_key", map[string]any{"proof": "ijklmnop"}); r.OK {
		t.Fatal("a wrong proof must be refused")
	}
	if r := cl.call("ack_recovery_key", map[string]any{"proof": "ABCDEFGH"}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := cl.call("recovery_key", nil); r.OK {
		t.Fatal("the key is forgotten after the acknowledgement")
	}
	if r := cl.call("finish", nil); !r.OK || finishedAction() != "reboot" {
		t.Fatalf("finish: %+v, action %q", r, finishedAction())
	}
	// A late subscriber gets the history without the secret. The replay
	// ends with the acknowledgement, after the secret and "done".
	cl2 := dial(t, sock)
	cl2.call("subscribe", nil)
	sawSecret := false
	deadline = time.After(2 * time.Minute)
replay:
	for {
		select {
		case e := <-cl2.evs:
			if e["type"] == "secret" {
				sawSecret = true
				if e["secret"] != nil {
					t.Fatal("the history replays the recovery key")
				}
			}
			if e["type"] == session.EvKeyAcked {
				break replay
			}
		case <-deadline:
			t.Fatal("the history was not replayed")
		}
	}
	if !sawSecret {
		t.Fatal("the history lost the (redacted) secret event")
	}
}
