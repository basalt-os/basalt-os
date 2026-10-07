package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
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
	// started is closed (and replaced) at each start, so waitFor wakes up
	// at once instead of polling against a short deadline.
	started chan struct{}
}

func (u *units) start(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.names = append(u.names, name)
	if u.started != nil {
		close(u.started)
	}
	u.started = make(chan struct{})
	return nil
}

// waitFor blocks until the unit was started (startUnits runs it in the
// background). The deadline is a hang guard, not part of the expectation.
func (u *units) waitFor(t *testing.T, name string) {
	t.Helper()
	deadline := time.After(time.Minute)
	for {
		u.mu.Lock()
		for _, n := range u.names {
			if n == name {
				u.mu.Unlock()
				return
			}
		}
		if u.started == nil {
			u.started = make(chan struct{})
		}
		next := u.started
		u.mu.Unlock()
		select {
		case <-next:
		case <-deadline:
			u.mu.Lock()
			defer u.mu.Unlock()
			t.Fatalf("unit %s was not started (%v)", name, u.names)
		}
	}
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
	// Each decision starts its unit from its own goroutine, in no set
	// order: wait for both before counting.
	u.waitFor(t, "basalt-gate-exec@"+rep.ID+".service")
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

// A skill grant: asked (it is not a mere look); the person's approval is
// kept as a rule ending with the grant, so asking again for the same
// folder is allowed quietly; another folder still asks; the rule can be
// removed at once from the shell UI.
func TestGrantBecomesARule(t *testing.T) {
	e := newEnv(t)
	docs := []gate.Call{{Action: "grant.folder", Args: map[string]any{"targets": []any{"/home/ana/Documents"}, "duration": "1h"}}}
	person := &gate.OnBehalf{Kind: "person"}
	r := e.do(pShell, gate.Request{Op: "propose", Calls: docs, OnBehalf: person})
	if !r.OK || r.Decision != gate.Asked || r.Class != "C1" {
		t.Fatalf("%+v", r)
	}
	if d := e.approve(pShellUI, r.ID); !d.OK || d.Decision != gate.Allowed {
		t.Fatalf("%+v", d)
	}
	rec := e.rec.find("gate.rule.add", "", func(d map[string]any) bool { return d["grant"] == r.ID })
	if rec == nil || rec.Data["rule"] != GrantRuleID(r.ID) {
		t.Fatalf("no grant rule: %+v", rec)
	}
	again := e.do(pShell, gate.Request{Op: "propose", Calls: docs, OnBehalf: person})
	if again.Decision != gate.Allowed || !strings.HasPrefix(again.By, "rule:"+GrantRuleID(r.ID)) {
		t.Fatalf("the same grant again: %+v", again)
	}
	other := []gate.Call{{Action: "grant.folder", Args: map[string]any{"targets": []any{"/home/ana/Private"}, "duration": "1h"}}}
	if o := e.do(pShell, gate.Request{Op: "propose", Calls: other, OnBehalf: person}); o.Decision != gate.Asked {
		t.Fatalf("another folder: %+v", o)
	}
	// An agent never gets it this way (person only).
	if a := e.propose(pAgent, docs); a.Decision != gate.Refused {
		t.Fatalf("agent: %+v", a)
	}
	// Revoked: the rule goes at once (tightening), the next ask asks.
	rm, _ := json.Marshal(map[string]any{"op": "remove", "scope": "user", "id": GrantRuleID(r.ID)})
	if x := e.do(pAgent, gate.Request{Op: "rules.apply", Rule: rm}); x.OK {
		t.Fatal("an agent changed the person's rules")
	}
	if x := e.do(pOther, gate.Request{Op: "rules.apply", Rule: rm}); x.OK {
		t.Fatal("another user removed the rule")
	}
	// The shell daemon (the person's own program) may take it away.
	if x := e.do(pShell, gate.Request{Op: "rules.apply", Rule: rm}); !x.OK {
		t.Fatalf("remove: %+v", x)
	}
	if x := e.do(pShell, gate.Request{Op: "propose", Calls: docs, OnBehalf: person}); x.Decision != gate.Asked {
		t.Fatalf("after removal: %+v", x)
	}
}

// A person's own rule pre-approves grants of a folder.
func TestRuleApprovesAGrant(t *testing.T) {
	e := newEnv(t)
	e.addRule("user", `
[[rule]]
id = "r-docs"
effect = "allow-tell"
actions = ["grant.folder"]
requesters = ["person"]
resources = { path_beneath = ["~/Documents"] }
expires = "7d"
`)
	docs := []gate.Call{{Action: "grant.folder", Args: map[string]any{"targets": []any{"/home/ana/Documents/taxes"}, "duration": "30m"}}}
	r := e.do(pShell, gate.Request{Op: "propose", Calls: docs, OnBehalf: &gate.OnBehalf{Kind: "person"}})
	if r.Decision != gate.Allowed || !strings.HasPrefix(r.By, "rule:r-docs@") {
		t.Fatalf("%+v", r)
	}
}

// Model downloads: the consent is the person's decision in the session
// (the executor still applies models.conf and polkit); never an agent.
func TestModelDownloadConsent(t *testing.T) {
	e := newEnv(t)
	dl := []gate.Call{{Action: "model.download", Args: map[string]any{"kind": "voice", "what": "english", "purpose": "voice"}}}
	pv := &gate.Preview{TitleKey: "models.download.title", TitleArgs: map[string]any{"kind": "voice", "what": "english", "mb": 148},
		Lines: []gate.Line{{Key: "models.download.consent", Args: map[string]any{"text": "Download the English speech model (148 MB)"}}}}
	if a := e.propose(pAgent, dl); a.Decision != gate.Refused {
		t.Fatalf("agent: %+v", a)
	}
	r := e.do(pShell, gate.Request{Op: "propose", Calls: dl, Preview: pv, OnBehalf: &gate.OnBehalf{Kind: "person"}})
	if r.Decision != gate.Asked {
		t.Fatalf("%+v", r)
	}
	n := len(e.pk.calls)
	if d := e.approve(pShellUI, r.ID); !d.OK || d.Decision != gate.Allowed || len(e.pk.calls) != n {
		t.Fatalf("%+v %v", d, e.pk.calls)
	}
	if c := e.do(pShell, gate.Request{Op: "claim", ID: r.ID, Calls: dl, Preview: pv, Executor: "basalt-shell"}); !c.OK {
		t.Fatalf("claim: %+v", c)
	}
	if e.rec.find("gate.decision", "allowed", func(d map[string]any) bool { return d["id"] == r.ID && d["by"] == "person:shell" }) == nil {
		t.Error("the consent was not recorded")
	}
}

// Knowledge packs and remote content: requests with the consent text as
// the preview; from the assistant or the person, never an agent.
func TestConsentRequests(t *testing.T) {
	e := newEnv(t)
	web := []gate.Call{{Action: "remote.consent", Args: map[string]any{"what": "web.search", "host": "search.example.org", "scope": "conversation"}}}
	r := e.do(pShell, gate.Request{Op: "propose", Calls: web, OnBehalf: &gate.OnBehalf{Kind: "assistant"}})
	if r.Decision != gate.Asked || r.Class != "C3" {
		t.Fatalf("%+v", r)
	}
	if a := e.propose(pAgent, web); a.Decision != gate.Refused {
		t.Fatalf("agent: %+v", a)
	}
	kp := []gate.Call{{Action: "knowledge.fetch", Args: map[string]any{"pack": "nginx", "topic": "nginx", "host": "obpkg.org", "bytes": 1048576}}}
	if k := e.do(pShell, gate.Request{Op: "propose", Calls: kp, OnBehalf: &gate.OnBehalf{Kind: "assistant"}}); k.Decision != gate.Asked {
		t.Fatalf("%+v", k)
	}
}

// basalt-agent grant and egress propose: requests of the person's own
// tool; an agent cannot ask; the person approves (their own password at
// the terminal; the root helper still asks for an administrator through
// pkexec); the tool runs it once allowed (claimed for it at wait).
func TestAgentGrantRequests(t *testing.T) {
	e := newEnv(t)
	host := []gate.Call{{Action: "agent.grant.host", Args: map[string]any{"session": "s-0123456789ab", "host": "api.example.org:443"}}}
	if a := e.propose(pAgent, host); a.Decision != gate.Refused {
		t.Fatalf("agent: %+v", a)
	}
	r := e.do(pTool, gate.Request{Op: "propose", Calls: host})
	if r.Decision != gate.Asked || r.Class != "C3" {
		t.Fatalf("%+v", r)
	}
	if d := e.approve(pTTY, r.ID); !d.OK || e.pk.last() != "org.basalt-os.gate.decide" {
		t.Fatalf("%+v %s", d, e.pk.last())
	}
	w := e.do(pTool, gate.Request{Op: "wait", ID: r.ID, Timeout: 1})
	if w.Decision != gate.Allowed || !e.s.entries[r.ID].Claimed {
		t.Fatalf("%+v", w)
	}
	exit := 0
	if x := e.do(pTool, gate.Request{Op: "result", ID: r.ID, OK: yes(), Exit: &exit}); !x.OK {
		t.Fatalf("%+v", x)
	}
	// The person's own allowlist override, and the system one.
	user := []gate.Call{{Action: "agent.egress.change", Args: map[string]any{"profile": "claude", "op": "add", "entry": "docs.example.org"}}}
	if u := e.do(pTool, gate.Request{Op: "propose", Calls: user}); u.Decision != gate.Asked {
		t.Fatalf("%+v", u)
	}
	sys := []gate.Call{{Action: "agent.egress.system", Args: map[string]any{"profile": "claude", "op": "add", "entry": "docs.example.org"}}}
	if s := e.do(pTool, gate.Request{Op: "propose", Calls: sys}); s.Decision != gate.Asked {
		t.Fatalf("%+v", s)
	}
	// Shadow mode: the tool reports what the terminal decided.
	if o := e.do(pTool, gate.Request{Op: "observe", Calls: user, Outcome: "approved", DecidedBy: "the person at the terminal"}); !o.OK {
		t.Fatalf("%+v", o)
	}
}

// The gate starts only executor units of request ids, never asking
// anyone (no polkit prompt reaches the person who already approved), and
// the shipped polkit rule allows exactly those units for root only.
func TestExecUnitStartNeverAsks(t *testing.T) {
	argv, err := systemctlArgv("/usr/bin/systemctl", "basalt-gate-exec@g-0123456789ab.service")
	if err != nil || strings.Join(argv, " ") != "/usr/bin/systemctl --no-ask-password --no-block start -- basalt-gate-exec@g-0123456789ab.service" {
		t.Fatalf("%v %v", argv, err)
	}
	for _, bad := range []string{"sshd.service", "basalt-gate-exec@.service", "basalt-gate-exec@g-0123456789ab.service; rm -rf /",
		"basalt-gate-exec@../x.service", "basalt-gate-exec@g-0123456789AB.service"} {
		if _, err := systemctlArgv("/usr/bin/systemctl", bad); err == nil {
			t.Errorf("would start %q", bad)
		}
	}
	b, err := os.ReadFile("../../dist/49-basalt-gate-exec.rules")
	if err != nil {
		t.Fatal(err)
	}
	rule := string(b)
	for _, need := range []string{`subject.user == "root"`, `action.lookup("verb") == "start"`,
		`"org.freedesktop.systemd1.manage-units"`} {
		if !strings.Contains(rule, need) {
			t.Errorf("the polkit rule lacks %s", need)
		}
	}
	m := regexp.MustCompile(`/(\^basalt-gate-exec[^/]*)/\.test`).FindStringSubmatch(rule)
	if m == nil {
		t.Fatal("no unit pattern in the polkit rule")
	}
	re := regexp.MustCompile(strings.ReplaceAll(m[1], `\.`, `\.`))
	for unit, want := range map[string]bool{"basalt-gate-exec@g-0123456789ab.service": true, "sshd.service": false,
		"basalt-gate-exec@g-0123456789ab.service.d": false, "xbasalt-gate-exec@g-0123456789ab.service": false} {
		if re.MatchString(unit) != want {
			t.Errorf("polkit rule pattern on %q: %v", unit, !want)
		}
	}
}
