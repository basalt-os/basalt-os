package apply

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gateclient"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gatelink"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// fakeGate: confirm succeeds only with the right code; status reports a
// result once confirmed.
func fakeGate(t *testing.T, code string) (*gatelink.Link, *[]gateclient.Request) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gate.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var seen []gateclient.Request
	confirmed := false
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			var req gateclient.Request
			_ = json.Unmarshal(line, &req)
			seen = append(seen, req)
			rep := gateclient.Reply{OK: true}
			switch req.Op {
			case "hello":
				rep.Protocol, rep.Enforce = gateclient.Protocol, []string{"apply"}
			case "confirm":
				if req.Code != code {
					rep = gateclient.Reply{Error: "refused: the code does not match the commands of this request"}
				} else {
					confirmed, rep.Decision = true, gateclient.Allowed
				}
			case "cancel":
				rep.Decision = gateclient.Cancelled
			case "status":
				rep.Request = &gateclient.View{ID: req.ID}
				if confirmed {
					rep.Request.Result = "exit code 0: applied and verified"
				}
			}
			b, _ := json.Marshal(rep)
			_, _ = c.Write(append(b, '\n'))
		}
	}()
	t.Setenv("BASALT_GATE_SOCKET", sock)
	l := gatelink.Detect("requester")
	if l.Mode != gatelink.Enforce {
		t.Fatalf("mode %q", l.Mode)
	}
	t.Cleanup(l.Close)
	return l, &seen
}

func testProposal() *proposal.Proposal {
	return &proposal.Proposal{ID: "p-4f2a9c", Title: "nginx stopped", Status: proposal.Pending,
		Actions: []action.Action{{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}}}}
}

func TestGateConfirmWithCode(t *testing.T) {
	p := testProposal()
	fp, _ := p.Fingerprint()
	l, _ := fakeGate(t, fp)
	var out bytes.Buffer
	a := &Applier{Store: proposal.Store{Dir: t.TempDir()}, Out: &out, Gate: l, FollowEvery: time.Millisecond, FollowFor: time.Second}
	if err := a.confirmAtGate("g-000000000001", p, Options{Yes: true, Confirm: "deadbeef"}, fp); err == nil || !strings.Contains(err.Error(), "--confirm "+fp) {
		t.Fatalf("wrong code: %v", err)
	}
	if err := a.confirmAtGate("g-000000000001", p, Options{Yes: true, Confirm: fp}, fp); err != nil {
		t.Fatal(err)
	}
	if err := a.follow(context.Background(), "g-000000000001", p); err != nil {
		t.Fatalf("follow: %v (%s)", err, out.String())
	}
}

func TestGateTypedNoCancels(t *testing.T) {
	p := testProposal()
	fp, _ := p.Fingerprint()
	l, seen := fakeGate(t, fp)
	var out bytes.Buffer
	a := &Applier{Store: proposal.Store{Dir: t.TempDir()}, Out: &out, In: strings.NewReader("no\n"), Interactive: true, Gate: l}
	if err := a.confirmAtGate("g-000000000001", p, Options{}, fp); !errors.Is(err, ErrCancelled) {
		t.Fatalf("%v", err)
	}
	last := (*seen)[len(*seen)-1]
	if last.Op != "cancel" {
		t.Errorf("the request was not withdrawn: %+v", last)
	}
}
