package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/gateclient"
)

type fakeGate struct {
	mu       sync.Mutex
	enforce  []string
	decision string // propose's answer
	waitDec  string // wait's answer
	ops      []string
}

func (f *fakeGate) start(t *testing.T) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gate.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv("BASALT_GATE_SOCKET", sock)
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
					f.ops = append(f.ops, req.Op)
					rep := gateclient.Reply{OK: true, ID: "g-000000000001"}
					switch req.Op {
					case "hello":
						rep.Protocol, rep.Enforce = gateclient.Protocol, f.enforce
					case "propose":
						rep.Decision, rep.Reason = f.decision, "a rule"
					case "wait":
						rep.Decision = f.waitDec
					}
					f.mu.Unlock()
					b, _ := json.Marshal(rep)
					_, _ = c.Write(append(b, '\n'))
				}
			}(c)
		}
	}()
}

func (f *fakeGate) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

var hostCall = []gateclient.Call{{Action: "agent.grant.host", Args: map[string]any{"session": "s-0123456789ab", "host": "api.example.org"}}}

func TestGateAbsent(t *testing.T) {
	t.Setenv("BASALT_GATE_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	g := dialGate()
	if g.enforced() || g.c != nil {
		t.Fatal("a gate without a socket")
	}
	g.observe(hostCall, "approved", "x") // no gate: nothing happens
	g.result("", nil)
}

func TestGateShadow(t *testing.T) {
	f := &fakeGate{enforce: []string{"skills"}}
	f.start(t)
	g := dialGate()
	defer g.close()
	if g.enforced() || g.mode != "observe" {
		t.Fatalf("mode %q", g.mode)
	}
	g.observe(hostCall, "approved", "polkit at the terminal")
	if ops := f.seen(); len(ops) != 2 || ops[1] != "observe" {
		t.Fatalf("%v", ops)
	}
}

func TestGateDecides(t *testing.T) {
	f := &fakeGate{enforce: []string{"agent"}, decision: gateclient.Asked, waitDec: gateclient.Allowed}
	f.start(t)
	g := dialGate()
	defer g.close()
	if !g.enforced() {
		t.Fatal("not enforced")
	}
	id, err := g.decide(hostCall, false)
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	g.result(id, nil)
	ops := f.seen()
	if ops[len(ops)-1] != "result" || ops[len(ops)-2] != "wait" {
		t.Fatalf("%v", ops)
	}
	f.mu.Lock()
	f.waitDec = gateclient.Declined
	f.mu.Unlock()
	if _, err := g.decide(hostCall, false); !errors.Is(err, errDeclined) {
		t.Fatalf("declined: %v", err)
	}
	f.mu.Lock()
	f.decision = gateclient.Refused
	f.mu.Unlock()
	if _, err := g.decide(hostCall, false); err == nil {
		t.Fatal("refused but went on")
	}
}
