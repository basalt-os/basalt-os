package gatelink

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gateclient"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// fakeGate answers hello with an enforce list and records every request.
type fakeGate struct {
	mu      sync.Mutex
	reqs    []gateclient.Request
	enforce []string
}

func (f *fakeGate) serve(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gate.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadBytes('\n')
					if err != nil {
						return
					}
					var req gateclient.Request
					_ = json.Unmarshal(line, &req)
					f.mu.Lock()
					f.reqs = append(f.reqs, req)
					f.mu.Unlock()
					rep := gateclient.Reply{OK: true}
					switch req.Op {
					case "hello":
						rep.Protocol, rep.Enforce = gateclient.Protocol, f.enforce
					case "propose":
						rep.ID, rep.Decision = "g-000000000001", gateclient.Asked
					case "observe":
						rep.Decision = gateclient.Asked
					}
					b, _ := json.Marshal(rep)
					_, _ = c.Write(append(b, '\n'))
				}
			}(c)
		}
	}()
	return sock
}

func (f *fakeGate) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r.Op)
	}
	return out
}

func sample() *proposal.Proposal {
	return &proposal.Proposal{ID: "p-4f2a9c", Source: "cli", Title: "nginx stopped", Status: proposal.Pending,
		Actions: []action.Action{{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}}}}
}

// The queue's short code is the fingerprint basalt show prints.
func TestShortCodeIsTheFingerprint(t *testing.T) {
	p := sample()
	gp, err := Request(p)
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := p.Fingerprint()
	if code := gateclient.ShortCode("sha256:x", gp.Ref, gp.Preview.Commands); code != fp {
		t.Fatalf("code %s, fingerprint %s", code, fp)
	}
	if gp.Ref != p.ID || len(gp.Calls) != 1 || gp.Calls[0].Action != "unit.restart" || gp.Calls[0].Args["unit"] != "nginx.service" {
		t.Fatalf("%+v", gp)
	}
	// The executor rebuilds the same request from the stored proposal.
	again, _ := Request(sample())
	d1, _ := gateclient.Digest(gp.Calls, *gp.Preview, nil, gp.Ref)
	d2, _ := gateclient.Digest(again.Calls, *again.Preview, nil, again.Ref)
	if d1 != d2 {
		t.Error("the request is not rebuilt identically")
	}
}

// No gate: nothing changes (Absent, no error); an observation without a
// gate does nothing.
func TestNoGate(t *testing.T) {
	t.Setenv("BASALT_GATE_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	l := Detect("requester")
	if l.Mode != Absent || l.C != nil {
		t.Fatalf("%+v", l)
	}
	l.Observe(sample(), "approved", "root")
	if _, err := l.Submit(sample()); err == nil {
		t.Error("submit without a gate")
	}
}

// A gate that observes this path: the decision made here is reported.
func TestShadowMode(t *testing.T) {
	f := &fakeGate{enforce: []string{"skills"}}
	t.Setenv("BASALT_GATE_SOCKET", f.serve(t))
	l := Detect("requester")
	defer l.Close()
	if l.Mode != Observe {
		t.Fatalf("mode %q", l.Mode)
	}
	l.Observe(sample(), "approved", "root at a terminal")
	ops := f.ops()
	if len(ops) != 2 || ops[1] != "observe" {
		t.Fatalf("%v", ops)
	}
	f.mu.Lock()
	r := f.reqs[1]
	f.mu.Unlock()
	if r.Outcome != "approved" || r.Ref != "p-4f2a9c" || r.Preview == nil || len(r.Preview.Commands) != 1 {
		t.Fatalf("%+v", r)
	}
}

// A gate that decides this path: the proposal is submitted; observing is
// off.
func TestEnforced(t *testing.T) {
	f := &fakeGate{enforce: []string{"apply"}}
	t.Setenv("BASALT_GATE_SOCKET", f.serve(t))
	l := Detect("requester")
	defer l.Close()
	if l.Mode != Enforce {
		t.Fatalf("mode %q", l.Mode)
	}
	l.Observe(sample(), "approved", "x")
	rep, err := l.Submit(sample())
	if err != nil || rep.ID == "" || rep.Decision != gateclient.Asked {
		t.Fatalf("%+v %v", rep, err)
	}
	if ops := f.ops(); len(ops) != 2 || ops[1] != "propose" {
		t.Fatalf("%v", ops)
	}
}
