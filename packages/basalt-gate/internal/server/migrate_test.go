package server

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/peer"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

var pGateExec = peer.Peer{UID: 0, PID: 950, Context: "system_u:system_r:basalt_gate_exec_t:s0"}

type units struct {
	mu    sync.Mutex
	names []string
}

func (u *units) start(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.names = append(u.names, name)
	return nil
}

// waitFor polls until the unit was started (startUnits runs it in the
// background).
func (u *units) waitFor(t *testing.T, name string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		u.mu.Lock()
		for _, n := range u.names {
			if n == name {
				u.mu.Unlock()
				return
			}
		}
		u.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("unit %s was not started (%v)", name, u.names)
}

func (u *units) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.names)
}

func withUnits(e *env) *units {
	u := &units{}
	e.o.StartUnit = u.start
	e.restart()
	return u
}

// The system assistant's proposal p-4f2a9c: restart nginx.
const asstRef = "p-4f2a9c"

var asstCmds = []string{"systemctl restart nginx.service"}

func asstCalls() []gate.Call {
	return []gate.Call{{Action: "unit.restart", Args: map[string]any{"unit": "nginx.service"}}}
}

func asstPreview() *gate.Preview {
	return &gate.Preview{TitleKey: "assistant.proposal.title", TitleArgs: map[string]any{"proposal": asstRef, "title": "nginx stopped"},
		Lines:    []gate.Line{{Key: "assistant.action.unit.restart", Args: map[string]any{"unit": "nginx.service"}}},
		Commands: asstCmds}
}

func fingerprint(ref string, cmds []string) string {
	sum := sha256.Sum256([]byte(ref + "\n" + strings.Join(cmds, "\n")))
	return hex.EncodeToString(sum[:])[:8]
}

// submitAsst queues the proposal as root's command line does (on behalf
// of the system assistant, with its reference and preview).
func submitAsst(e *env) gate.Reply {
	e.t.Helper()
	rep := e.do(pRoot, gate.Request{Op: "propose", Calls: asstCalls(), Preview: asstPreview(), Ref: asstRef,
		OnBehalf: &gate.OnBehalf{Kind: "system-assistant"}})
	if !rep.OK || rep.Decision != gate.Asked {
		e.t.Fatalf("submit: %+v", rep)
	}
	return rep
}

func TestHelloSaysWhatIsEnforced(t *testing.T) {
	e := newEnv(t)
	rep := e.do(pShell, gate.Request{Op: "hello", Role: "requester", Client: "basalt-shell/0.8.0", Protocol: gate.Protocol})
	if !rep.OK || strings.Join(rep.Enforce, ",") != "skills,models,consent" {
		t.Fatalf("default enforce: %+v", rep)
	}
	e.o.Config.Enforce = []string{"all"}
	e.restart()
	rep = e.do(pShell, gate.Request{Op: "hello", Role: "requester", Client: "basalt-shell/0.8.0", Protocol: gate.Protocol})
	if strings.Join(rep.Enforce, ",") != "all" {
		t.Fatalf("%+v", rep)
	}
}

// Shadow mode: the decision made elsewhere is recorded with what the gate
// would have decided; nothing waits in the queue.
func TestObserve(t *testing.T) {
	e := newEnv(t)
	rep := e.do(pShell, gate.Request{Op: "observe", Calls: echo("seen"), OnBehalf: &gate.OnBehalf{Kind: "person"},
		Outcome: "approved", DecidedBy: "shell sheet"})
	if !rep.OK || rep.Decision != gate.Asked || rep.ID == "" {
		t.Fatalf("%+v", rep)
	}
	if r := e.rec.find("gate.request", "ok", func(d map[string]any) bool { return d["id"] == rep.ID && d["observed"] == true }); r == nil {
		t.Fatal("observed request not recorded")
	}
	r := e.rec.find("gate.decision", "allowed", func(d map[string]any) bool { return d["id"] == rep.ID })
	if r == nil || r.Data["by"] != "observed:shell sheet" || r.Data["would"] != gate.Asked {
		t.Fatalf("observed decision: %+v", r)
	}
	if p := e.do(pShell, gate.Request{Op: "pending"}); len(p.Requests) != 0 {
		t.Errorf("an observation was queued: %+v", p.Requests)
	}
	// Agents never report decisions; a bad outcome is an error.
	if r := e.do(pAgent, gate.Request{Op: "observe", Calls: echo("x"), Outcome: "approved"}); r.OK {
		t.Error("an agent reported a decision")
	}
	if r := e.do(pTool, gate.Request{Op: "observe", Calls: echo("x"), Outcome: "maybe"}); r.OK {
		t.Error("unknown outcome accepted")
	}
}

// basalt apply ID --yes --confirm CODE through the gate: the code is the
// fingerprint basalt show prints; only root confirms; the gate then starts
// the executor unit, which sees the request, claims it with what it is
// about to run, once.
func TestRootConfirmsWithTheCode(t *testing.T) {
	e := newEnv(t)
	u := withUnits(e)
	rep := submitAsst(e)
	code := fingerprint(asstRef, asstCmds)
	if dv := e.do(pTTY, gate.Request{Op: "status", ID: rep.ID}); dv.Request.Code != code || dv.Request.Ref != asstRef {
		t.Fatalf("the queue shows another code: %+v", dv.Request)
	}
	for name, p := range map[string]peer.Peer{"a user": pTool, "an agent": pAgent, "the shell": pShell} {
		if r := e.do(p, gate.Request{Op: "confirm", ID: rep.ID, Code: code}); r.OK {
			t.Errorf("%s confirmed", name)
		}
	}
	if r := e.do(pRoot, gate.Request{Op: "confirm", ID: rep.ID, Code: "00000000"}); r.OK {
		t.Error("a wrong code confirmed")
	}
	r := e.do(pRoot, gate.Request{Op: "confirm", ID: rep.ID, Code: code})
	if !r.OK || r.Decision != gate.Allowed || r.By != "person:tty-root" {
		t.Fatalf("confirm: %+v", r)
	}
	if e.rec.find("gate.decision", "allowed", func(d map[string]any) bool { return d["id"] == rep.ID && d["by"] == "person:tty-root" }) == nil {
		t.Error("the person's decision was not recorded")
	}
	u.waitFor(t, "basalt-gate-exec@"+rep.ID+".service")

	// The executor sees the request (reference and calls) before claiming.
	st := e.do(pGateExec, gate.Request{Op: "status", ID: rep.ID})
	if !st.OK || st.Request.Ref != asstRef || len(st.Request.Calls) != 1 || st.Request.Code != "" {
		t.Fatalf("executor view: %+v", st)
	}
	// Another program cannot claim; the executor claims with its calls.
	if c := e.do(pRoot, gate.Request{Op: "claim", ID: rep.ID, Calls: asstCalls(), Preview: asstPreview()}); c.OK {
		t.Error("root outside the executor domain claimed")
	}
	c := e.do(pGateExec, gate.Request{Op: "claim", ID: rep.ID, Calls: asstCalls(), Preview: asstPreview(), Executor: "basalt-gate-exec"})
	if !c.OK {
		t.Fatalf("claim: %+v", c)
	}
	if c := e.do(pGateExec, gate.Request{Op: "claim", ID: rep.ID, Calls: asstCalls(), Preview: asstPreview()}); c.OK {
		t.Error("a second claim")
	}
	exit := 0
	if r := e.do(pGateExec, gate.Request{Op: "result", ID: rep.ID, OK: yes(), Exit: &exit, Snapshots: []string{"41", "42"}}); !r.OK {
		t.Fatalf("result: %+v", r)
	}
}

// What the executor would run differs from what was approved: refused,
// and the decision is void.
func TestExecutorClaimWithOtherCalls(t *testing.T) {
	e := newEnv(t)
	withUnits(e)
	rep := submitAsst(e)
	if r := e.do(pRoot, gate.Request{Op: "confirm", ID: rep.ID, Code: fingerprint(asstRef, asstCmds), Mode: "terminal"}); !r.OK {
		t.Fatalf("%+v", r)
	}
	other := []gate.Call{{Action: "unit.restart", Args: map[string]any{"unit": "sshd.service"}}}
	if c := e.do(pGateExec, gate.Request{Op: "claim", ID: rep.ID, Calls: other, Preview: asstPreview()}); c.OK || !strings.Contains(c.Error, "digest mismatch") {
		t.Fatalf("%+v", c)
	}
	if c := e.do(pGateExec, gate.Request{Op: "claim", ID: rep.ID, Calls: asstCalls(), Preview: asstPreview()}); c.OK {
		t.Error("the voided decision was claimed")
	}
}

// allow_code_confirm = no: the code no longer confirms; typing yes at a
// root terminal still does; approving in the queue (polkit, admin) too.
func TestCodeConfirmCanBeTurnedOff(t *testing.T) {
	e := newEnv(t)
	e.o.Config.AllowCodeConfirm = false
	u := withUnits(e)
	rep := submitAsst(e)
	code := fingerprint(asstRef, asstCmds)
	if r := e.do(pRoot, gate.Request{Op: "confirm", ID: rep.ID, Code: code}); r.OK || !strings.Contains(r.Error, "turned off") {
		t.Fatalf("%+v", r)
	}
	if r := e.do(pRoot, gate.Request{Op: "confirm", ID: rep.ID, Code: code, Mode: "terminal"}); !r.OK {
		t.Fatalf("%+v", r)
	}
	// The shell UI approves another one: an administrator (polkit), then
	// the unit starts.
	rep2 := e.do(pRoot, gate.Request{Op: "propose", Calls: []gate.Call{{Action: "dnf.clean", Args: map[string]any{}}},
		Preview: &gate.Preview{TitleKey: "assistant.proposal.title", Commands: []string{"dnf clean packages"}}, Ref: "p-000001",
		OnBehalf: &gate.OnBehalf{Kind: "system-assistant"}})
	if d := e.approve(pShellUI, rep2.ID); !d.OK || d.Decision != gate.Allowed {
		t.Fatalf("%+v", d)
	}
	if e.pk.last() != "org.basalt-os.gate.decide-admin" {
		t.Errorf("polkit: %s", e.pk.last())
	}
	u.waitFor(t, "basalt-gate-exec@"+rep2.ID+".service")
	if n := u.count(); n != 2 {
		t.Errorf("units started: %d", n)
	}
}

// A reference belongs to the system assistant: nobody else sends one, and
// an agent cannot pass as the system assistant.
func TestReferenceIsTheSystemAssistants(t *testing.T) {
	e := newEnv(t)
	if r := e.do(pTool, gate.Request{Op: "propose", Calls: asstCalls(), Ref: asstRef}); r.Decision != gate.Refused {
		t.Errorf("a user sent a reference: %+v", r)
	}
	if r := e.do(pAgent, gate.Request{Op: "propose", Calls: asstCalls(), Ref: asstRef, OnBehalf: &gate.OnBehalf{Kind: "system-assistant"}}); r.Decision != gate.Refused {
		t.Errorf("an agent passed as the system assistant: %+v", r)
	}
	if r := e.do(pTool, gate.Request{Op: "propose", Calls: asstCalls(), OnBehalf: &gate.OnBehalf{Kind: "system-assistant"}}); r.Decision != gate.Refused {
		t.Errorf("a user passed as the system assistant: %+v", r)
	}
	// The system assistant's own domain asks as itself.
	if r := e.do(pSysAsst, gate.Request{Op: "propose", Calls: asstCalls(), Preview: asstPreview(), Ref: asstRef}); r.Decision != gate.Asked {
		t.Errorf("the system assistant: %+v", r)
	}
}

// The person's own power request from the command bar: the shell UI
// decides it in the session (no administrator), as today; an agent
// cannot ask for it at all.
func TestPersonOwnPowerInTheSession(t *testing.T) {
	e := newEnv(t)
	power := []gate.Call{{Action: "session.power", Args: map[string]any{"op": "poweroff"}}}
	if r := e.propose(pAgent, power); r.Decision != gate.Refused {
		t.Fatalf("agent: %+v", r)
	}
	r := e.do(pShell, gate.Request{Op: "propose", Calls: power, OnBehalf: &gate.OnBehalf{Kind: "person"}})
	if !r.OK || r.Decision != gate.Asked || r.Class != "C4" {
		t.Fatalf("%+v", r)
	}
	n := len(e.pk.calls)
	if d := e.approve(pShellUI, r.ID); !d.OK || d.Decision != gate.Allowed {
		t.Fatalf("%+v", d)
	}
	if len(e.pk.calls) != n {
		t.Errorf("polkit asked: %v", e.pk.calls[n:])
	}
	// The same from the terminal decider: the person's own password.
	r2 := e.do(pShell, gate.Request{Op: "propose", Calls: []gate.Call{{Action: "session.power", Args: map[string]any{"op": "lock"}}},
		OnBehalf: &gate.OnBehalf{Kind: "person"}})
	if d := e.approve(pTTY, r2.ID); !d.OK || e.pk.last() != "org.basalt-os.gate.decide" {
		t.Fatalf("%+v %s", d, e.pk.last())
	}
	// A shell request of an agent's critical action still needs an
	// administrator.
	ctl := e.do(pShell, gate.Request{Op: "propose", Calls: []gate.Call{{Action: "agent.control", Args: map[string]any{"reason": "fix the layout", "minutes": 5}}},
		OnBehalf: &gate.OnBehalf{Kind: "agent", Name: "claude"}})
	if d := e.approve(pShellUI, ctl.ID); !d.OK || e.pk.last() != "org.basalt-os.gate.decide-admin" {
		t.Fatalf("%+v %s", d, e.pk.last())
	}
}
