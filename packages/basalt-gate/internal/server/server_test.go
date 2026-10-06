package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/peer"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/polkit"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/store"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// ---------------------------------------------------------------------------
// Fixtures

type recorder struct {
	mu   sync.Mutex
	recs []ledger.Record
	now  func() time.Time
}

func (r *recorder) Send(x ledger.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x.Time = r.now().UTC().Format(time.RFC3339Nano)
	r.recs = append(r.recs, x)
}

func (r *recorder) find(event, outcome string, f func(map[string]any) bool) *ledger.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.recs) - 1; i >= 0; i-- {
		x := r.recs[i]
		if x.Event == event && (outcome == "" || x.Outcome == outcome) && (f == nil || f(x.Data)) {
			return &x
		}
	}
	return nil
}

type fakePolkit struct {
	mu    sync.Mutex
	deny  bool
	calls []string
}

func (p *fakePolkit) Check(_ context.Context, pid, uid int, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, action)
	if p.deny {
		return polkit.ErrNotAuthorized
	}
	return nil
}

func (p *fakePolkit) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return ""
	}
	return p.calls[len(p.calls)-1]
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	t   *testing.T
	s   *Server
	rec *recorder
	pk  *fakePolkit
	clk *clock
	dir string
	o   Options
}

func loadReg(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.New()
	for _, dir := range []string{"../../actions.d", "../../testdata/registry"} {
		names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			if err := reg.Add(filepath.Base(n), b); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := reg.Check(); err != nil {
		t.Fatal(err)
	}
	return reg
}

func homeOfTest(uid int) (string, error) {
	switch uid {
	case 1000:
		return "/home/ana", nil
	case 1001:
		return "/home/bo", nil
	}
	return "/root", nil
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	// A fake /proc: the agent process runs in a basalt-agent session slice.
	proc := filepath.Join(dir, "proc")
	_ = os.MkdirAll(filepath.Join(proc, "5000"), 0o755)
	_ = os.WriteFile(filepath.Join(proc, "5000", "cgroup"),
		[]byte("0::/user.slice/user-1000.slice/user@1000.service/basaltagent.slice/basaltagent-0123456789ab.slice/basaltagent-0123456789ab-agent.scope\n"), 0o644)
	peer.ProcRoot = proc
	t.Cleanup(func() { peer.ProcRoot = "/proc" })
	e := &env{t: t, rec: &recorder{}, pk: &fakePolkit{}, clk: &clock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)}, dir: dir}
	cfg := config.Default()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.RequestsPerMinute = 1000
	limits, err := policy.LoadHardLimits("../../hardlimits.json")
	if err != nil {
		t.Fatal(err)
	}
	presets, err := policy.LoadPresets("../../presets")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	e.rec.now = e.clk.now
	e.o = Options{Config: cfg, Reg: loadReg(t), Limits: limits, Presets: presets, Store: st, Ledger: e.rec, Polkit: e.pk,
		Now: e.clk.now, Home: homeOfTest, Logf: t.Logf,
		// Never the real systemctl: an approval starts the executor unit in a
		// goroutine, whose failure would write the queue into the test's
		// TempDir while it is removed (withUnits records the starts instead).
		StartUnit: func(string) error { return nil },
		Env: func(now time.Time) policy.Env {
			return policy.Env{Now: now, Power: "ac", Presence: policy.Unknown, Network: policy.Unknown}
		},
		AgentName: func(s string) string {
			if s == "s-0123456789ab" {
				return "claude"
			}
			return ""
		},
		History: func(uid int, since time.Time) ([]ledger.Stored, error) {
			e.rec.mu.Lock()
			defer e.rec.mu.Unlock()
			var out []ledger.Stored
			for _, r := range e.rec.recs {
				if !strings.HasPrefix(r.Event, "gate.") || (uid != 0 && r.UID != uid) {
					continue
				}
				data := map[string]any{}
				b, _ := json.Marshal(r.Data)
				_ = json.Unmarshal(b, &data)
				out = append(out, ledger.Stored{Time: r.Time, UID: r.UID, Event: r.Event, Outcome: r.Outcome, Data: data})
			}
			return out, nil
		}}
	e.restart()
	return e
}

func (e *env) restart() {
	e.t.Helper()
	s, err := New(e.o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s = s
}

var (
	pTool     = peer.Peer{UID: 1000, PID: 4242, Context: "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023"}
	pOther    = peer.Peer{UID: 1001, PID: 4343, Context: "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023"}
	pAgent    = peer.Peer{UID: 1000, PID: 5000, Context: "unconfined_u:unconfined_r:basalt_agent_t:s0:c1,c2"}
	pTTY      = peer.Peer{UID: 1000, PID: 6000, Context: "unconfined_u:unconfined_r:basalt_gate_tty_t:s0-s0:c0.c1023"}
	pShellUI  = peer.Peer{UID: 1000, PID: 7000, Context: "unconfined_u:unconfined_r:basalt_shell_ui_t:s0-s0:c0.c1023"}
	pShell    = peer.Peer{UID: 1000, PID: 7100, Context: "unconfined_u:unconfined_r:basalt_shell_t:s0-s0:c0.c1023"}
	pSysAsst  = peer.Peer{UID: 0, PID: 800, Context: "system_u:system_r:basalt_assistant_t:s0"}
	pExecutor = peer.Peer{UID: 0, PID: 900, Context: "system_u:system_r:test_executor_t:s0"}
	pRoot     = peer.Peer{UID: 0, PID: 1, Context: "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023"}
)

func (e *env) do(p peer.Peer, req gate.Request) gate.Reply {
	e.t.Helper()
	// Through JSON, as on the socket (numbers as json.Number).
	b, _ := json.Marshal(req)
	var r gate.Request
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		e.t.Fatal(err)
	}
	return e.s.Handle(context.Background(), e.s.NewConn(p, "test/1"), r)
}

func echo(text string) []gate.Call {
	return []gate.Call{{Action: "lab.echo", Args: map[string]any{"text": text}}}
}

func (e *env) propose(p peer.Peer, calls []gate.Call) gate.Reply {
	e.t.Helper()
	rep := e.do(p, gate.Request{Op: "propose", Calls: calls})
	if !rep.OK {
		e.t.Fatalf("propose: %s", rep.Error)
	}
	return rep
}

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

func (e *env) approve(p peer.Peer, id string) gate.Reply {
	e.t.Helper()
	return e.do(p, gate.Request{Op: "decide", ID: id, Approve: yes()})
}

// addRule adds a rule through the gate itself: a request approved at the
// terminal (as an administrator for system rules).
func (e *env) addRule(scope, src string) {
	e.t.Helper()
	rs, err := policy.ParseRules(src)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rs {
		raw, _ := json.Marshal(r)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		requester := pTool
		if scope == policy.ScopeSystem {
			requester = pRoot
		}
		rep := e.propose(requester, []gate.Call{{Action: "gate.rule.change", Args: map[string]any{"op": "add", "scope": scope, "rule": m}}})
		if rep.Decision != gate.Asked {
			e.t.Fatalf("rule change: %+v", rep)
		}
		decider := pTTY
		if scope == policy.ScopeSystem {
			decider = peer.Peer{UID: 0, PID: 6001, Context: "unconfined_u:unconfined_r:basalt_gate_tty_t:s0-s0:c0.c1023"}
		}
		if d := e.approve(decider, rep.ID); !d.OK || d.Decision != gate.Allowed {
			e.t.Fatalf("approving the rule: %+v", d)
		}
		if res := e.s.entries[rep.ID].Result; res != "the rule set was updated" {
			e.t.Fatalf("rule not applied: %s", res)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests

func TestCarefulByDefault(t *testing.T) {
	e := newEnv(t)
	st := e.s.Status()
	if st.Preset != "careful" || !st.SealOK || st.Rules == 0 {
		t.Fatalf("%+v", st)
	}
	rep := e.propose(pTool, echo("hi"))
	if rep.Decision != gate.Asked || rep.Class != "C1" || rep.ID == "" {
		t.Fatalf("%+v", rep)
	}
	if r := e.rec.find("gate.request", "ok", func(d map[string]any) bool { return d["id"] == rep.ID }); r == nil || r.UID != 1000 {
		t.Fatalf("request not recorded: %+v", r)
	}
	if r := e.rec.find("gate.decision", "ok", func(d map[string]any) bool { return d["id"] == rep.ID && d["outcome"] == "asked" }); r == nil {
		t.Fatal("decision not recorded")
	}
	// The requester sees its request without the code; a decider sees the code.
	own := e.do(pTool, gate.Request{Op: "status", ID: rep.ID})
	if !own.OK || own.Request.Code != "" {
		t.Fatalf("requester view: %+v", own)
	}
	dv := e.do(pTTY, gate.Request{Op: "status", ID: rep.ID})
	if !dv.OK || len(dv.Request.Code) != 8 {
		t.Fatalf("decider view: %+v", dv)
	}
	// Someone else (another user, an agent) does not see it.
	if r := e.do(pOther, gate.Request{Op: "status", ID: rep.ID}); r.OK {
		t.Error("another user saw the request")
	}
	if r := e.do(pAgent, gate.Request{Op: "status", ID: rep.ID}); r.OK {
		t.Error("an agent saw someone else's request")
	}
}

func TestOnlyDecidersDecide(t *testing.T) {
	e := newEnv(t)
	rep := e.propose(pAgent, echo("let me"))
	for name, p := range map[string]peer.Peer{"agent": pAgent, "tool": pTool, "root outside the decider domain": pRoot, "relay": pShell} {
		if d := e.approve(p, rep.ID); d.OK {
			t.Errorf("%s decided", name)
		}
	}
	if r := e.rec.find("gate.decision", "denied", func(d map[string]any) bool {
		v, _ := d["verdict"].(string)
		return strings.Contains(v, "only a trusted surface decides")
	}); r == nil {
		t.Error("the refused decision was not recorded")
	}
	if st := e.do(pTool, gate.Request{Op: "status", ID: rep.ID}); st.OK {
		t.Error("tool saw the agent's request")
	}
	if st := e.do(pTTY, gate.Request{Op: "status", ID: rep.ID}); st.Request.Decision != gate.Asked {
		t.Errorf("still pending: %+v", st)
	}
}

func TestPolkitOnEveryTerminalDecision(t *testing.T) {
	e := newEnv(t)
	rep := e.propose(pTool, echo("a"))
	e.pk.deny = true
	if d := e.approve(pTTY, rep.ID); d.OK {
		t.Fatal("approved without authentication")
	}
	e.pk.deny = false
	if d := e.approve(pTTY, rep.ID); !d.OK || d.Decision != gate.Allowed {
		t.Fatalf("%+v", d)
	}
	if e.pk.last() != polkit.Decide {
		t.Errorf("polkit action %s", e.pk.last())
	}
	if r := e.rec.find("gate.decision", "allowed", func(d map[string]any) bool { return d["id"] == rep.ID }); r == nil || r.Data["by"] != "person:tty" {
		t.Errorf("approval record: %+v", r)
	}
	// Declining never needs a password (it only tightens).
	rep2 := e.propose(pTool, echo("b"))
	e.pk.deny = true
	if d := e.do(pTTY, gate.Request{Op: "decide", ID: rep2.ID, Approve: no()}); !d.OK || d.Decision != gate.Declined {
		t.Fatalf("%+v", d)
	}
	// The shell UI decides an undoable request without polkit.
	rep3 := e.propose(pTool, echo("c"))
	n := len(e.pk.calls)
	if d := e.approve(pShellUI, rep3.ID); !d.OK || len(e.pk.calls) != n {
		t.Fatalf("shell UI: %+v (polkit calls %d -> %d)", d, n, len(e.pk.calls))
	}
}

func TestSystemRequestsNeedAdmin(t *testing.T) {
	e := newEnv(t)
	rep := e.propose(pSysAsst, []gate.Call{{Action: "unit.restart", Args: map[string]any{"unit": "nginx.service"}}})
	if rep.Decision != gate.Asked || rep.Class != "C2" {
		t.Fatalf("%+v", rep)
	}
	if d := e.approve(pShellUI, rep.ID); !d.OK || e.pk.last() != polkit.DecideAdmin {
		t.Fatalf("%+v, polkit %s", d, e.pk.last())
	}
	// The system assistant's own kind only: a user tool cannot ask for it.
	if r := e.propose(pTool, []gate.Call{{Action: "unit.restart", Args: map[string]any{"unit": "nginx.service"}}}); r.Decision != gate.Refused {
		t.Errorf("a tool asked for a system assistant action: %+v", r)
	}
}

func approved(t *testing.T, e *env, calls []gate.Call) gate.Reply {
	t.Helper()
	rep := e.propose(pTool, calls)
	if d := e.approve(pTTY, rep.ID); !d.OK {
		t.Fatalf("%+v", d)
	}
	st := e.do(pTool, gate.Request{Op: "status", ID: rep.ID})
	st.Digest = st.Request.Digest
	return st
}

func TestClaims(t *testing.T) {
	e := newEnv(t)
	rep := approved(t, e, echo("run me"))
	id, digest := rep.ID, rep.Digest
	// Not the executor: refused, whoever it is.
	for name, p := range map[string]peer.Peer{"the requester": pTool, "an agent": pAgent, "root elsewhere": pRoot,
		"the right type as another uid": {UID: 1000, PID: 901, Context: pExecutor.Context}} {
		if r := e.do(p, gate.Request{Op: "claim", ID: id, Digest: digest}); r.OK {
			t.Errorf("%s claimed", name)
		}
	}
	// The executor with the approved digest: once.
	if r := e.do(pExecutor, gate.Request{Op: "claim", ID: id, Digest: digest}); !r.OK || r.Request == nil || len(r.Request.Calls) != 1 {
		t.Fatalf("claim: %+v", r)
	}
	if r := e.do(pExecutor, gate.Request{Op: "claim", ID: id, Digest: digest}); r.OK || !strings.Contains(r.Error, "already claimed") {
		t.Errorf("replayed claim: %+v", r)
	}
	if e.rec.find("gate.claim", "ok", func(d map[string]any) bool { return d["id"] == id }) == nil {
		t.Error("claim not recorded")
	}
	if e.rec.find("gate.claim", "denied", func(d map[string]any) bool { return d["id"] == id }) == nil {
		t.Error("refused claim not recorded")
	}
	// The result, by the claimer only.
	if r := e.do(pTool, gate.Request{Op: "result", ID: id, OK: yes()}); r.OK {
		t.Error("someone else reported the result")
	}
	exit := 0
	if r := e.do(pExecutor, gate.Request{Op: "result", ID: id, OK: yes(), Exit: &exit, Detail: "echoed"}); !r.OK {
		t.Fatalf("%+v", r)
	}
	if r := e.rec.find("gate.result", "ok", func(d map[string]any) bool { return d["id"] == id }); r == nil {
		t.Error("result not recorded")
	}
	// A pending or declined request cannot be claimed.
	p2 := e.propose(pTool, echo("pending"))
	st := e.do(pTool, gate.Request{Op: "status", ID: p2.ID})
	if r := e.do(pExecutor, gate.Request{Op: "claim", ID: p2.ID, Digest: st.Request.Digest}); r.OK {
		t.Error("claimed a pending request")
	}
}

// Approve A, run B: the claim carries the digest of what would run; a
// different one voids the decision.
func TestDigestMismatch(t *testing.T) {
	e := newEnv(t)
	rep := approved(t, e, echo("approved text"))
	if r := e.do(pExecutor, gate.Request{Op: "claim", ID: rep.ID, Digest: "sha256:" + strings.Repeat("0", 64)}); r.OK || !strings.Contains(r.Error, "digest mismatch") {
		t.Fatalf("%+v", r)
	}
	if r := e.do(pExecutor, gate.Request{Op: "claim", ID: rep.ID, Digest: rep.Digest}); r.OK {
		t.Error("the voided decision was claimed afterwards")
	}
	if r := e.rec.find("gate.decision", "denied", func(d map[string]any) bool { return d["id"] == rep.ID && d["by"] == "stale" }); r == nil {
		t.Error("the voided decision was not recorded")
	}
}

func TestClaimWindow(t *testing.T) {
	e := newEnv(t)
	rep := approved(t, e, echo("later"))
	e.clk.add(11 * time.Minute)
	if r := e.do(pExecutor, gate.Request{Op: "claim", ID: rep.ID, Digest: rep.Digest}); r.OK || !strings.Contains(r.Error, "too old") {
		t.Errorf("%+v", r)
	}
}

// A rule that allows, added through the gate, then deciding by itself;
// the decision names the rule.
func TestRuleAutoApproves(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-echo"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["tool:test"]
`)
	if e.pk.last() != polkit.Decide {
		t.Errorf("a person's own user rule: polkit %s", e.pk.last())
	}
	if r := e.rec.find("gate.rule.add", "", func(d map[string]any) bool { return d["rule"] == "r-echo" }); r == nil {
		t.Fatal("rule add not recorded")
	}
	rep := e.propose(pTool, echo("auto"))
	if rep.Decision != gate.Allowed || !strings.HasPrefix(rep.By, "rule:r-echo@") {
		t.Fatalf("%+v", rep)
	}
	if r := e.rec.find("gate.decision", "allowed", func(d map[string]any) bool { return d["id"] == rep.ID }); r == nil ||
		!strings.HasPrefix(r.Data["by"].(string), "rule:r-echo@") {
		t.Errorf("%+v", r)
	}
	// It covers this user's tool only: an agent of the same user asks.
	if r := e.propose(pAgent, echo("auto")); r.Decision != gate.Asked {
		t.Errorf("agent: %+v", r)
	}
	if r := e.propose(pOther, echo("auto")); r.Decision != gate.Asked {
		t.Errorf("other user: %+v", r)
	}
	// rules.list shows it to its owner, not to agents.
	if r := e.do(pTool, gate.Request{Op: "rules.list"}); !r.OK || !strings.Contains(string(mustJSON(r.Rules)), "r-echo") {
		t.Errorf("%+v", r)
	}
	if r := e.do(pOther, gate.Request{Op: "rules.list"}); strings.Contains(string(mustJSON(r.Rules)), "r-echo") {
		t.Error("another user saw the rule")
	}
	if r := e.do(pAgent, gate.Request{Op: "rules.list"}); r.OK {
		t.Error("an agent read the rules")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// Agents cannot change rules, even by asking: the request is refused
// before it reaches a person.
func TestAgentsCannotChangeRules(t *testing.T) {
	e := newEnv(t)
	rule := map[string]any{"id": "r-mine", "effect": "allow-quiet", "actions": []any{"lab.echo"}, "requesters": []any{"agent"}}
	rep := e.propose(pAgent, []gate.Call{{Action: "gate.rule.change", Args: map[string]any{"op": "add", "scope": "user", "rule": rule}}})
	if rep.Decision != gate.Refused {
		t.Fatalf("%+v", rep)
	}
	apply, _ := json.Marshal(map[string]any{"op": "add", "scope": "user", "rule": rule})
	for _, op := range []string{"rules.apply", "rules.draft", "rules.unpause"} {
		if r := e.do(pAgent, gate.Request{Op: op, Rule: apply, ID: "user/1000/r-mine"}); r.OK {
			t.Errorf("%s from an agent", op)
		}
	}
	// A tool may only tighten directly through a trusted surface.
	if r := e.do(pTool, gate.Request{Op: "rules.apply", Rule: apply}); r.OK {
		t.Error("a tool applied a rule")
	}
	if r := e.do(pTTY, gate.Request{Op: "rules.apply", Rule: apply}); r.OK || !strings.Contains(r.Error, "loosens") {
		t.Errorf("a loosening rule applied at once: %+v", r)
	}
}

func TestTighteningIsImmediate(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-echo"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["tool"]
`)
	never, _ := json.Marshal(map[string]any{"op": "add", "scope": "user", "rule": map[string]any{"id": "r-never",
		"effect": "refuse", "actions": []any{"lab.echo"}, "requesters": []any{"any"}}})
	if r := e.do(pTTY, gate.Request{Op: "rules.apply", Rule: never}); !r.OK {
		t.Fatalf("%+v", r)
	}
	if r := e.propose(pTool, echo("x")); r.Decision != gate.Refused {
		t.Errorf("after never allow: %+v", r)
	}
	// Revoking an allow rule is a tightening too.
	rm, _ := json.Marshal(map[string]any{"op": "remove", "scope": "user", "id": "r-echo"})
	if r := e.do(pShellUI, gate.Request{Op: "rules.apply", Rule: rm}); !r.OK {
		t.Fatalf("%+v", r)
	}
	if e.rec.find("gate.rule.remove", "", func(d map[string]any) bool { return d["rule"] == "r-echo" }) == nil {
		t.Error("removal not recorded")
	}
}

func TestLockedIsRefused(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeSystem, `
[[rule]]
id = "r-all-tools"
effect = "allow-tell"
actions = ["tool.exec"]
requesters = ["tool"]
expires = "7d"
`)
	exec := func(argv ...any) []gate.Call {
		return []gate.Call{{Action: "tool.exec", Args: map[string]any{"tool": "tui-test", "operation": "run", "argv": argv}}}
	}
	rep := e.propose(pTool, exec("sudo", "setenforce", "0"))
	if rep.Decision != gate.Refused || !strings.HasPrefix(rep.By, "hard-limit:locked:") {
		t.Fatalf("%+v", rep)
	}
	if r := e.rec.find("gate.decision", "denied", func(d map[string]any) bool { return d["id"] == rep.ID }); r == nil {
		t.Error("refusal not recorded")
	}
	// The unlock does not exist yet: refused and recorded.
	if r := e.do(pTTY, gate.Request{Op: "unlock", ID: rep.ID}); r.OK {
		t.Error("unlocked")
	}
	if e.rec.find("gate.unlock", "denied", nil) == nil {
		t.Error("unlock attempt not recorded")
	}
	// The rule still allows what is not locked, and the tool runs it itself.
	ok := e.propose(pTool, exec("systemctl", "restart", "nginx"))
	if ok.Decision != gate.Allowed {
		t.Fatalf("%+v", ok)
	}
	if e.rec.find("gate.claim", "ok", func(d map[string]any) bool { return d["id"] == ok.ID && d["executor"] == "requester" }) == nil {
		t.Error("the requester's implicit claim was not recorded")
	}
	exit := 0
	if r := e.do(pTool, gate.Request{Op: "result", ID: ok.ID, Exit: &exit}); !r.OK {
		t.Errorf("%+v", r)
	}
	// A destructive tool call is Cannot be undone, and always asks.
	d := e.propose(pTool, []gate.Call{{Action: "tool.exec", Args: map[string]any{"tool": "tui-test", "operation": "prune",
		"argv": []any{"snapper", "delete", "5"}, "destructive": true}}})
	if d.Decision != gate.Asked || d.Class != "C5" {
		t.Errorf("%+v", d)
	}
}

func TestPersonOnlyAndRelays(t *testing.T) {
	e := newEnv(t)
	insert := []gate.Call{{Action: "text.insert", Args: map[string]any{"text": "hello", "field": "1"}}}
	if r := e.propose(pAgent, insert); r.Decision != gate.Refused || !strings.Contains(r.Reason, "only by the person") {
		t.Errorf("agent: %+v", r)
	}
	if r := e.propose(pTool, insert); r.Decision != gate.Refused {
		t.Errorf("tool: %+v", r)
	}
	person := &gate.OnBehalf{Kind: "person"}
	r := e.do(pShell, gate.Request{Op: "propose", Calls: insert, OnBehalf: person})
	if !r.OK || r.Decision != gate.Asked {
		t.Errorf("the shell relaying the person: %+v", r)
	}
	// Only a configured relay may say on whose behalf it asks.
	if r := e.do(pTool, gate.Request{Op: "propose", Calls: insert, OnBehalf: person}); r.Decision != gate.Refused {
		t.Errorf("a tool claimed to be the person: %+v", r)
	}
	if r := e.do(pAgent, gate.Request{Op: "propose", Calls: insert, OnBehalf: person}); r.Decision != gate.Refused {
		t.Errorf("an agent claimed to be the person: %+v", r)
	}
	if r := e.do(pSysAsst, gate.Request{Op: "propose", Calls: insert, OnBehalf: person}); r.Decision != gate.Refused {
		t.Errorf("the assistant reported a person: %+v", r)
	}
	// The person relayed with an agent in the chain is not the person.
	via := &gate.OnBehalf{Kind: "person", Via: []gate.Party{{Kind: "agent", Name: "claude"}}}
	if r := e.do(pShell, gate.Request{Op: "propose", Calls: insert, OnBehalf: via}); r.Decision != gate.Refused {
		t.Errorf("person through an agent: %+v", r)
	}
	// Agent-only actions refuse everyone else.
	capture := []gate.Call{{Action: "screen.capture", Args: map[string]any{}}}
	if r := e.propose(pTool, capture); r.Decision != gate.Refused {
		t.Errorf("tool asked for an agent-only action: %+v", r)
	}
	if r := e.propose(pAgent, capture); r.Decision != gate.Asked {
		t.Errorf("agent capture: %+v", r)
	}
	// Unknown actions are refused, and a scheduled origin cannot be claimed.
	if r := e.propose(pTool, []gate.Call{{Action: "nuke.everything", Args: map[string]any{}}}); r.Decision != gate.Refused {
		t.Errorf("unknown action: %+v", r)
	}
	if r := e.do(pTool, gate.Request{Op: "propose", Calls: echo("x"), Origin: "schedule"}); r.Decision != gate.Refused {
		t.Errorf("schedule origin from a client: %+v", r)
	}
	if e.rec.find("gate.request", "denied", nil) == nil {
		t.Error("registry refusals not recorded")
	}
}

// What an agent asks for is shown as the registry builds it, never in the
// agent's own words; agents carry the web taint.
func TestAgentRequests(t *testing.T) {
	e := newEnv(t)
	rep := e.do(pAgent, gate.Request{Op: "propose", Calls: echo("x"), Taint: "none",
		Preview: &gate.Preview{TitleKey: "lab.harmless", TitleArgs: map[string]any{"text": "nothing to see"}}})
	st := e.do(pTTY, gate.Request{Op: "status", ID: rep.ID})
	v := st.Request
	if v.Preview.TitleKey != "lab.echo.title" || v.Taint != "web" || v.Requester.Name != "claude" || v.Requester.Session != "s-0123456789ab" {
		t.Fatalf("%+v", v)
	}
	// An agent allow rule with the default taint_max does not match; one
	// that accepts web content does.
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-agent-echo"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["agent:claude"]
`)
	if r := e.propose(pAgent, echo("y")); r.Decision != gate.Asked {
		t.Errorf("agent matched a rule that does not accept web taint: %+v", r)
	}
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-agent-echo-web"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["agent:claude"]
taint_max = "web"
`)
	if r := e.propose(pAgent, echo("y2")); r.Decision != gate.Allowed {
		t.Errorf("%+v", r)
	}
	// A trusted relay's preview is kept (the planner built it).
	r := e.do(pShell, gate.Request{Op: "propose", Calls: echo("z"), OnBehalf: &gate.OnBehalf{Kind: "assistant"},
		Preview: &gate.Preview{TitleKey: "lab.echo.planned", TitleArgs: map[string]any{"text": "z"}}})
	st = e.do(pTTY, gate.Request{Op: "status", ID: r.ID})
	if st.Request.Preview.TitleKey != "lab.echo.planned" || st.Request.Requester.Kind != "assistant" {
		t.Errorf("%+v", st.Request)
	}
}

func TestStopAndResume(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-echo"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["tool"]
`)
	// Anyone may stop, an agent too, without authentication.
	n := len(e.pk.calls)
	if r := e.do(pAgent, gate.Request{Op: "stop", Reason: "it looks wrong"}); !r.OK {
		t.Fatal(r.Error)
	}
	if len(e.pk.calls) != n || !e.s.Status().Stopped {
		t.Fatal("stop needed authentication or did not stop")
	}
	if r := e.rec.find("gate.stop", "", nil); r == nil || !strings.Contains(r.Data["who"].(string), "agent") {
		t.Errorf("%+v", r)
	}
	if r := e.propose(pTool, echo("x")); r.Decision != gate.Asked || r.By != "stop" {
		t.Errorf("an allow rule while stopped: %+v", r)
	}
	// Stopped survives a restart.
	e.restart()
	if !e.s.Status().Stopped {
		t.Fatal("the stop did not survive a restart")
	}
	// Resuming is a loosening: never an agent or a plain tool.
	for name, p := range map[string]peer.Peer{"agent": pAgent, "tool": pTool} {
		if r := e.do(p, gate.Request{Op: "resume"}); r.OK {
			t.Errorf("%s resumed", name)
		}
	}
	if e.rec.find("gate.resume", "denied", nil) == nil {
		t.Error("refused resume not recorded")
	}
	e.pk.deny = true
	if r := e.do(pTTY, gate.Request{Op: "resume"}); r.OK {
		t.Error("resumed without authentication at the terminal")
	}
	e.pk.deny = false
	if r := e.do(pTTY, gate.Request{Op: "resume"}); !r.OK || e.s.Status().Stopped {
		t.Fatalf("%+v", r)
	}
	if r := e.propose(pTool, echo("x2")); r.Decision != gate.Allowed {
		t.Errorf("after resume: %+v", r)
	}
}

func TestRemember(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-echo-once"
effect = "ask-remember"
actions = ["lab.echo"]
requesters = ["tool"]
remember = "1h"
`)
	rep := e.propose(pTool, echo("same"))
	st := e.do(pTTY, gate.Request{Op: "status", ID: rep.ID})
	if st.Request.Remember != "1h" {
		t.Fatalf("%+v", st.Request)
	}
	if d := e.do(pTTY, gate.Request{Op: "decide", ID: rep.ID, Approve: yes(), Remember: true}); !d.OK {
		t.Fatalf("%+v", d)
	}
	if r := e.propose(pTool, echo("same")); r.Decision != gate.Allowed {
		t.Errorf("remembered: %+v", r)
	}
	// Remembered: the same action, requester and resources (lab.echo
	// touches none, so any text); another requester still asks.
	if r := e.propose(pTool, echo("different")); r.Decision != gate.Allowed {
		t.Errorf("same action and requester: %+v", r)
	}
	if r := e.propose(pAgent, echo("same")); r.Decision != gate.Asked {
		t.Errorf("another requester: %+v", r)
	}
	e.clk.add(61 * time.Minute)
	if r := e.propose(pTool, echo("same again")); r.Decision != gate.Asked {
		t.Errorf("after the remember time: %+v", r)
	}
	// Remember is offered only by ask-remember rules.
	plain := e.propose(pAgent, echo("plain"))
	if plain.ID == "" {
		t.Fatal(plain)
	}
	if d := e.do(pTTY, gate.Request{Op: "decide", ID: plain.ID, Approve: yes(), Remember: true}); d.OK {
		t.Error("remember without an ask-remember rule")
	}
}

func TestQueue(t *testing.T) {
	e := newEnv(t)
	a := e.propose(pTool, echo("dup"))
	b := e.propose(pTool, echo("dup"))
	if a.ID != b.ID {
		t.Errorf("identical pending requests: %s %s", a.ID, b.ID)
	}
	def := e.do(pTool, gate.Request{Op: "propose", Calls: echo("later"), Deferrable: true})
	// Survives a restart.
	e.restart()
	if r := e.do(pTool, gate.Request{Op: "status", ID: a.ID}); !r.OK || r.Decision != gate.Asked {
		t.Fatalf("after restart: %+v", r)
	}
	e.clk.add(6 * time.Minute)
	e.s.ExpireNow()
	if r := e.do(pTool, gate.Request{Op: "wait", ID: a.ID, Timeout: 1}); r.Decision != gate.Expired {
		t.Errorf("interactive request after 6 minutes: %+v", r)
	}
	if r := e.do(pTool, gate.Request{Op: "status", ID: def.ID}); r.Decision != gate.Asked {
		t.Errorf("deferrable request after 6 minutes: %+v", r)
	}
	if e.rec.find("gate.decision", "denied", func(d map[string]any) bool { return d["outcome"] == "expired" }) == nil {
		t.Error("expiry not recorded")
	}
	// The requester withdraws its own request, nobody else's.
	c := e.propose(pTool, echo("withdraw"))
	if r := e.do(pOther, gate.Request{Op: "cancel", ID: c.ID}); r.OK {
		t.Error("someone else cancelled")
	}
	if r := e.do(pTool, gate.Request{Op: "cancel", ID: c.ID}); !r.OK || r.Decision != gate.Cancelled {
		t.Errorf("%+v", r)
	}
	// pending lists what waits for this decider.
	if r := e.do(pTTY, gate.Request{Op: "pending"}); !r.OK || len(r.Requests) != 1 || r.Requests[0].ID != def.ID {
		t.Errorf("%+v", r.Requests)
	}
}

func TestBatches(t *testing.T) {
	e := newEnv(t)
	a := e.do(pTool, gate.Request{Op: "propose", Calls: echo("1"), Group: "tidy"})
	b := e.do(pTool, gate.Request{Op: "propose", Calls: echo("2"), Group: "tidy"})
	if d := e.do(pTTY, gate.Request{Op: "decide", Group: "tidy", Approve: yes()}); !d.OK || len(d.Decided) != 2 {
		t.Fatalf("%+v", d)
	}
	for _, id := range []string{a.ID, b.ID} {
		if r := e.do(pTool, gate.Request{Op: "status", ID: id}); r.Decision != gate.Allowed {
			t.Errorf("%s: %+v", id, r)
		}
	}
	// Critical requests are never approved in a batch.
	x := e.propose(pAgent, []gate.Call{{Action: "agent.control", Args: map[string]any{"reason": "click"}}})
	y := e.propose(pTool, echo("3"))
	if d := e.do(pTTY, gate.Request{Op: "decide", IDs: []string{x.ID, y.ID}, Approve: yes()}); d.OK {
		t.Error("a Critical request was batched")
	}
	// Different requesters, one tainted: one by one.
	z := e.propose(pAgent, echo("4"))
	if d := e.do(pTTY, gate.Request{Op: "decide", IDs: []string{y.ID, z.ID}, Approve: yes()}); d.OK {
		t.Error("a tainted request was batched with another requester's")
	}
	// Critical calls cannot be mixed with others in one request.
	if r := e.propose(pAgent, []gate.Call{{Action: "agent.control", Args: map[string]any{"reason": "x"}}, echo("y")[0]}); r.Decision != gate.Refused {
		t.Errorf("%+v", r)
	}
}

func TestLimitCircuitBreaker(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-once"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["tool"]
limits = { per_day = 1 }
`)
	if r := e.propose(pTool, echo("1")); r.Decision != gate.Allowed {
		t.Fatalf("%+v", r)
	}
	r := e.propose(pTool, echo("2"))
	if r.Decision != gate.Asked || !strings.HasPrefix(r.By, "limit:") {
		t.Fatalf("%+v", r)
	}
	if e.rec.find("gate.limit", "", func(d map[string]any) bool { return d["rule"] == "r-once" }) == nil {
		t.Error("limit not recorded")
	}
	// Paused until a person looks, even on another day.
	e.clk.add(24 * time.Hour)
	if r := e.propose(pTool, echo("3")); r.Decision != gate.Asked {
		t.Errorf("paused rule allowed: %+v", r)
	}
	if r := e.do(pTool, gate.Request{Op: "rules.unpause", ID: "user/1000/r-once"}); r.OK {
		t.Error("a tool unpaused a rule")
	}
	if r := e.do(pTTY, gate.Request{Op: "rules.unpause", ID: "user/1000/r-once"}); !r.OK {
		t.Fatalf("%+v", r)
	}
	if r := e.propose(pTool, echo("4")); r.Decision != gate.Allowed {
		t.Errorf("after a person looked: %+v", r)
	}
}

// Someone with root offline edits the rule file: the gate notices, loads
// only the careful rules and records it as critical.
func TestSealTamper(t *testing.T) {
	e := newEnv(t)
	e.addRule(policy.ScopeUser, `
[[rule]]
id = "r-echo"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["tool"]
`)
	f := filepath.Join(e.o.Config.StateDir, "rules.json")
	b, _ := os.ReadFile(f)
	b = []byte(strings.Replace(string(b), `"lab.echo"`, `"tool.exec"`, 1))
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.restart()
	if e.s.Status().SealOK || e.s.Status().Preset != "careful" {
		t.Fatalf("%+v", e.s.Status())
	}
	if e.rec.find("gate.seal_error", "error", nil) == nil {
		t.Error("seal error not recorded")
	}
	if r := e.propose(pTool, echo("x")); r.Decision != gate.Asked {
		t.Errorf("tampered rule in force: %+v", r)
	}
	bad, _ := filepath.Glob(filepath.Join(e.o.Config.StateDir, "rules.json.bad-*"))
	if len(bad) != 1 {
		t.Errorf("the tampered file was not kept aside: %v", bad)
	}
}

// Dry run: replay what the gate recorded against draft rules.
func TestDryRun(t *testing.T) {
	e := newEnv(t)
	for _, s := range []string{"a", "b", "c"} {
		e.propose(pTool, echo(s))
	}
	e.propose(pAgent, echo("agent"))
	rules, _ := json.Marshal(`
[[rule]]
id = "r-echo"
effect = "allow-tell"
actions = ["lab.echo"]
requesters = ["tool"]
`)
	r := e.do(pTTY, gate.Request{Op: "rules.simulate", Rules: rules, Scope: "user", Since: "1h"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	// The user sees its own records: 3 by the tool would be allowed, the
	// agent's would still ask.
	s := r.Simulation
	if s.Total != 4 || s.Allowed != 3 || s.Asked != 1 || s.Changed != 3 {
		t.Fatalf("%+v", s)
	}
	draft, _ := json.Marshal(map[string]any{"id": "r-draft", "effect": "allow-tell", "actions": []any{"lab.echo"}, "requesters": []any{"agent"}, "taint_max": "web"})
	d := e.do(pTool, gate.Request{Op: "rules.draft", Rule: draft, Since: "1h"})
	if !d.OK || d.Rules[0].Error != "" || d.Simulation.Allowed != 1 {
		t.Fatalf("%+v %+v", d.Rules, d.Simulation)
	}
	if !strings.Contains(d.Rules[0].Sentence, "any agent") {
		t.Error(d.Rules[0].Sentence)
	}
	// Records given by the client are replayed only for its own uid.
	recs, _ := json.Marshal([]map[string]any{{"time": e.clk.now().UTC().Format(time.RFC3339Nano), "uid": 1001, "event": "gate.request", "outcome": "ok",
		"data": map[string]any{"id": "g-000000000001", "requester": map[string]any{"kind": "tool", "name": "x"}, "taint": "none", "origin": "request",
			"calls": []any{map[string]any{"action": "lab.echo", "class": "C1"}}}}})
	r = e.do(pTTY, gate.Request{Op: "rules.simulate", Rules: rules, Scope: "user", Since: "1h", Records: recs})
	if !r.OK || r.Simulation.Total != 0 {
		t.Errorf("another user's records replayed: %+v", r.Simulation)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	e.s.cfg.RequestsPerMinute = 10
	refused := 0
	for i := 0; i < 40; i++ {
		r := e.do(pAgent, gate.Request{Op: "propose", Calls: echo(strings.Repeat("x", i+1))})
		if r.Decision == gate.Refused && strings.Contains(r.Reason, "too many") {
			refused++
		}
	}
	if refused == 0 {
		t.Error("no request was rate limited")
	}
}

func TestHelloAndCheck(t *testing.T) {
	e := newEnv(t)
	h := e.do(pTool, gate.Request{Op: "hello", Client: "tui-systemd/0.4.0", Role: "requester"})
	if !h.OK || h.Protocol != gate.Protocol || h.Kind != "tool" {
		t.Fatalf("%+v", h)
	}
	if h := e.do(pAgent, gate.Request{Op: "hello", Role: "decider"}); strings.Contains(strings.Join(h.Roles, ","), "decider") || h.Kind != "agent" {
		t.Errorf("an agent became a decider by asking: %+v", h)
	}
	if h := e.do(pTTY, gate.Request{Op: "hello"}); !strings.Contains(strings.Join(h.Roles, ","), "decider") {
		t.Errorf("%+v", h)
	}
	n := len(e.s.entries)
	c := e.do(pTool, gate.Request{Op: "check", Calls: echo("x")})
	if !c.OK || c.Decision != gate.Asked || len(e.s.entries) != n {
		t.Errorf("check queued something: %+v", c)
	}
	if r := e.do(pTool, gate.Request{Op: "delete-everything"}); r.OK {
		t.Error("unknown op")
	}
}

func TestWaitWakes(t *testing.T) {
	e := newEnv(t)
	rep := e.propose(pTool, echo("wait"))
	done := make(chan gate.Reply)
	go func() { done <- e.do(pTool, gate.Request{Op: "wait", ID: rep.ID, Timeout: 30}) }()
	time.Sleep(50 * time.Millisecond)
	e.approve(pTTY, rep.ID)
	select {
	case r := <-done:
		if r.Decision != gate.Allowed || r.TimedOut {
			t.Errorf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not wake")
	}
}

var _ = errors.New
