package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/peer"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/server"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/store"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

type fixture struct {
	Name    string          `json:"name"`
	Request json.RawMessage `json:"request"`
	Reply   json.RawMessage `json:"reply"`
}

// The exchange of the third-party subset, in order.
var order = []string{"hello", "propose-asked", "propose-allowed", "propose-refused", "wait-allowed", "result"}

func fixtures(t *testing.T) []fixture {
	t.Helper()
	var out []fixture
	for _, n := range order {
		b, err := os.ReadFile(filepath.Join("../../testdata/protocol", n+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var f fixture
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out = append(out, f)
	}
	return out
}

func asMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

// The fake server answers exactly the fixtures, and the fixtures follow
// the protocol (no violations).
func TestFixturesAgainstFake(t *testing.T) {
	f := NewFakeServer()
	st := &Conn{}
	for _, fx := range fixtures(t) {
		got := asMap(t, f.Answer(st, fx.Request))
		want := asMap(t, fx.Reply)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %v\n want %v", fx.Name, got, want)
		}
	}
	if err := f.Check(); err != nil {
		t.Error(err)
	}
}

// The fake catches what a client gets wrong.
func TestFakeCatchesViolations(t *testing.T) {
	for name, lines := range map[string][]string{
		"propose before hello": {`{"op":"propose","calls":[{"action":"tool.exec","args":{"tool":"t","operation":"o","argv":["x"]}}]}`},
		"unknown op":           {`{"op":"hello","client":"t/1"}`, `{"op":"decide","id":"g-000000000001"}`},
		"missing argv":         {`{"op":"hello","client":"t/1"}`, `{"op":"propose","calls":[{"action":"tool.exec","args":{"tool":"t","operation":"o"}}]}`},
		"other action":         {`{"op":"hello","client":"t/1"}`, `{"op":"propose","calls":[{"action":"unit.restart","args":{"unit":"x"}}]}`},
		"unknown member":       {`{"op":"hello","client":"t/1","password":"x"}`},
		"decider role":         {`{"op":"hello","client":"t/1","role":"decider"}`},
		"result unknown id":    {`{"op":"hello","client":"t/1"}`, `{"op":"result","id":"g-00000000000f","ok":true}`},
	} {
		f := NewFakeServer()
		st := &Conn{}
		for _, l := range lines {
			f.Answer(st, []byte(l))
		}
		if f.Check() == nil {
			t.Errorf("%s: not caught", name)
		}
	}
}

// The reference client conforms: the whole subset over a real socket.
func TestReferenceClientConforms(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "gate.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := NewFakeServer()
	go f.Serve(ln)
	c, err := gate.Detect(sock, "tui-test/1.0", "requester")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(argv []string, hint string, destructive bool) gate.Call {
		return gate.Call{Action: "tool.exec", Args: map[string]any{"tool": "tui-test", "operation": "run", "argv": argv,
			"description": "test", "destructive": destructive, "class_hint": hint}}
	}
	rep, err := c.Propose(gate.Proposal{Calls: []gate.Call{call([]string{"systemctl", "restart", "x"}, "C2", false)}})
	if err != nil || rep.Decision != gate.Asked {
		t.Fatalf("%+v %v", rep, err)
	}
	w, err := c.Wait(rep.ID, 30)
	if err != nil || w.Decision != gate.Allowed {
		t.Fatalf("%+v %v", w, err)
	}
	if _, err := c.Result(rep.ID, true, 0, ""); err != nil {
		t.Fatal(err)
	}
	rep, _ = c.Propose(gate.Proposal{Calls: []gate.Call{call([]string{"x", "--decline"}, "C3", false)}})
	if w, _ := c.Wait(rep.ID, 30); w.Decision != gate.Declined {
		t.Errorf("%+v", w)
	}
	if rep, _ := c.Propose(gate.Proposal{Calls: []gate.Call{call([]string{"mkfs.ext4", "/dev/x"}, "C5", true)}}); rep.Decision != gate.Refused {
		t.Errorf("%+v", rep)
	}
	if err := f.Check(); err != nil {
		t.Error(err)
	}
}

// Feature detection: no socket, a silent socket, another protocol: the
// gate is absent and the tool goes on as on any other system.
func TestDetectAbsent(t *testing.T) {
	dir := t.TempDir()
	if _, err := gate.Detect(filepath.Join(dir, "none.sock"), "t/1", "requester"); err == nil {
		t.Error("no socket")
	}
	silent := filepath.Join(dir, "silent.sock")
	ln, _ := net.Listen("unix", silent)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	start := time.Now()
	if _, err := gate.Detect(silent, "t/1", "requester"); err == nil {
		t.Error("silent socket")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("detection took %v", d)
	}
	other := filepath.Join(dir, "other.sock")
	ln2, _ := net.Listen("unix", other)
	defer ln2.Close()
	go func() {
		for {
			c, err := ln2.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = bufio.NewReader(c).ReadBytes('\n')
				_, _ = c.Write([]byte(`{"ok":true,"protocol":"gate/2"}` + "\n"))
				c.Close()
			}()
		}
	}()
	if _, err := gate.Detect(other, "t/1", "requester"); err == nil {
		t.Error("another major version")
	}
}

type allowPolkit struct{}

func (allowPolkit) Check(context.Context, int, int, string) error { return nil }

// The real gate answers the fixture requests with the same decisions,
// classes and members (ids and reasons may differ).
func TestFixturesAgainstRealGate(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	reg, err := registry.Load("../../actions.d")
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := policy.LoadHardLimits("../../hardlimits.json")
	presets, _ := policy.LoadPresets("../../presets")
	st, _ := store.Open(cfg.StateDir)
	now := time.Now()
	rules := presets["careful"]
	for i := range rules {
		if err := policy.Validate(&rules[i], reg, policy.ScopeSystem, now); err != nil {
			t.Fatal(err)
		}
	}
	extra, _ := policy.ParseRules(`
[[rule]]
id = "r-tui-status"
effect = "allow-tell"
actions = ["class:C1"]
requesters = ["tool:tui-systemd"]
`)
	if err := policy.Validate(&extra[0], reg, policy.ScopeUser, now); err != nil {
		t.Fatal(err)
	}
	extra[0].UID = os.Getuid()
	if err := st.SaveRules(append(rules, extra[0]), "careful"); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(server.Options{Config: cfg, Reg: reg, Limits: limits, Presets: presets, Store: st, Polkit: allowPolkit{}})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "gate.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go s.Serve(ln)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	send := func(raw json.RawMessage) map[string]any {
		_, _ = conn.Write(append(append([]byte{}, raw...), '\n'))
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		return asMap(t, line)
	}
	ids := map[string]string{}
	for _, fx := range fixtures(t) {
		want := asMap(t, fx.Reply)
		req := asMap(t, fx.Request)
		if id, ok := req["id"].(string); ok {
			req["id"] = ids[id]
		}
		if fx.Name == "wait-allowed" {
			// A person approves at the terminal meanwhile.
			tty := peer.Peer{UID: os.Getuid(), PID: os.Getpid(), Context: "unconfined_u:unconfined_r:basalt_gate_tty_t:s0"}
			approve := true
			if r := s.Handle(context.Background(), s.NewConn(tty, "basalt-gate-tty"), gate.Request{Op: "decide", ID: ids["g-000000000001"], Approve: &approve}); !r.OK {
				t.Fatalf("approve: %+v", r)
			}
		}
		raw, _ := json.Marshal(req)
		got := send(raw)
		if id, ok := want["id"].(string); ok {
			ids[id], _ = got["id"].(string)
		}
		for _, k := range []string{"ok", "decision", "class", "protocol", "kind"} {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("%s: %s = %v, the fixture says %v (reply %v)", fx.Name, k, got[k], want[k], got)
			}
		}
		if fx.Name == "hello" {
			continue
		}
		if keys(got) != keys(want) {
			t.Errorf("%s: members %s, the fixture has %s", fx.Name, keys(got), keys(want))
		}
	}
}

func keys(m map[string]any) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	b, _ := json.Marshal(out)
	return string(b)
}
