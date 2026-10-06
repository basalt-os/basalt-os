package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
)

// testReg loads the shipped registry plus the test actions.
func testReg(t *testing.T) *registry.Registry {
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
				t.Fatalf("%s: %v", n, err)
			}
		}
	}
	if err := reg.Check(); err != nil {
		t.Fatal(err)
	}
	return reg
}

func testLimits(t *testing.T) *HardLimits {
	t.Helper()
	h, err := LoadHardLimits("../../hardlimits.json")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func home(uid int) (string, error) {
	switch uid {
	case 1000:
		return "/home/ana", nil
	case 1001:
		return "/home/bo", nil
	case 0:
		return "/root", nil
	}
	return "", os.ErrNotExist
}

// noon on a Friday, local time.
var friNoon = time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)

func env() Env { return Env{Now: friNoon, Power: "ac", Presence: Unknown, Network: Unknown} }

type fixture struct {
	t   *testing.T
	reg *registry.Registry
	eng *Engine
}

func newFixture(t *testing.T, rules ...string) *fixture {
	t.Helper()
	reg := testReg(t)
	eng := NewEngine(reg, testLimits(t), home)
	f := &fixture{t: t, reg: reg, eng: eng}
	var all []Rule
	for _, src := range rules {
		rs, err := ParseRules(src)
		if err != nil {
			t.Fatalf("parse: %v\n%s", err, src)
		}
		for i := range rs {
			scope := ScopeSystem
			if strings.Contains(rs[i].ID, "user") {
				scope = ScopeUser
			}
			if err := Validate(&rs[i], reg, scope, friNoon.Add(-time.Hour)); err != nil {
				t.Fatalf("validate %s: %v", rs[i].ID, err)
			}
			if scope == ScopeUser {
				rs[i].UID = 1000
			}
		}
		all = append(all, rs...)
	}
	eng.SetRules(all)
	return f
}

type req struct {
	kind, name string
	uid        int
	via        []proposal.Party
	taint      string
	origin     string
}

func (f *fixture) input(r req, calls ...proposal.Call) Input {
	f.t.Helper()
	if r.taint == "" {
		r.taint = "none"
	}
	if r.origin == "" {
		r.origin = "request"
	}
	p := &proposal.Proposal{Requester: proposal.Requester{Kind: r.kind, Name: r.name, UID: r.uid, Via: r.via,
		Taint: r.taint, Origin: r.origin}}
	var facts []CallFacts
	for _, c := range calls {
		a, err := f.reg.Lookup(c.Action)
		if err != nil {
			f.t.Fatal(err)
		}
		args, err := a.Validate(c.Args)
		if err != nil {
			f.t.Fatalf("%s: %v", c.Action, err)
		}
		res, err := a.Extract(args, r.uid, home)
		if err != nil {
			f.t.Fatal(err)
		}
		cls := a.Class
		if a.ClassFromHint {
			cls = proposal.MaxClass(cls, a.HintDefault)
			if h, _ := args["class_hint"].(string); h != "" {
				cls = proposal.MaxClass(cls, h)
			}
		}
		pc := proposal.Call{Action: a.ID, Args: args}
		p.Calls = append(p.Calls, pc)
		facts = append(facts, CallFacts{Call: pc, Class: cls, Resources: res, System: a.System})
	}
	return Input{P: p, Calls: facts, Env: env()}
}

func trash(paths ...string) proposal.Call {
	l := make([]any, len(paths))
	for i, p := range paths {
		l[i] = p
	}
	return proposal.Call{Action: "files.trash", Args: map[string]any{"paths": l}}
}

func toolExec(argv ...string) proposal.Call {
	l := make([]any, len(argv))
	for i, a := range argv {
		l[i] = a
	}
	return proposal.Call{Action: "tool.exec", Args: map[string]any{"tool": "tui-test", "operation": "run", "argv": l}}
}

const tidyRule = `
[[rule]]
id = "r-tidy"
effect = "allow-tell"
actions = ["group:files.tidy"]
requesters = ["assistant"]
resources = { path_beneath = ["~/Downloads"], exclude = ["~/Downloads/keep"] }
`

var (
	assistant = req{kind: "assistant", uid: 1000}
	agentReq  = req{kind: "agent", name: "claude", uid: 1000}
)

// The policy matrix: who asks, for what, where, under which rules.
func TestPolicyMatrix(t *testing.T) {
	f := newFixture(t, tidyRule)
	cases := []struct {
		name string
		r    req
		call proposal.Call
		want string
		by   string
	}{
		{"rule allows the assistant in Downloads", assistant, trash("~/Downloads/a.iso"), Allowed, "rule:r-tidy@"},
		{"absolute path inside the scope", assistant, trash("/home/ana/Downloads/x/y"), Allowed, "rule:r-tidy@"},
		{"excluded subfolder asks", assistant, trash("~/Downloads/keep/a"), Asked, "default"},
		{"outside the scope asks", assistant, trash("~/Documents/a"), Asked, "default"},
		{"one path outside makes the whole call ask", assistant, trash("~/Downloads/a", "~/Documents/b"), Asked, "default"},
		{"prefix trick is not beneath", assistant, trash("/home/ana/Downloads2/a"), Asked, "default"},
		{"an agent is not the assistant", agentReq, trash("~/Downloads/a"), Asked, "default"},
		{"an agent relayed through the assistant: least trusted party", req{kind: "assistant", uid: 1000,
			via: []proposal.Party{{Kind: "agent", Name: "claude"}}}, trash("~/Downloads/a"), Asked, "default"},
		{"another user's Downloads is not ~", req{kind: "assistant", uid: 1001}, trash("/home/ana/Downloads/a"), Asked, "default"},
		{"no rule: C2 asks", req{kind: "system-assistant", uid: 0}, proposal.Call{Action: "unit.restart",
			Args: map[string]any{"unit": "nginx.service"}}, Asked, "default"},
	}
	for _, c := range cases {
		d := f.eng.Decide(f.input(c.r, c.call), nil)
		if d.Outcome != c.want || !strings.HasPrefix(d.By, c.by) {
			t.Errorf("%s: got %s by %s (%s), want %s by %s", c.name, d.Outcome, d.By, d.Reason, c.want, c.by)
		}
	}
}

// The most restrictive matching effect wins, whoever wrote the rules.
func TestCarefulWins(t *testing.T) {
	f := newFixture(t, tidyRule, `
[[rule]]
id = "r-ask-keep"
effect = "ask"
actions = ["files.trash"]
requesters = ["any"]
resources = { path_beneath = ["~/Downloads/old"] }

[[rule]]
id = "r-never-iso"
effect = "refuse"
actions = ["files.trash"]
requesters = ["assistant"]
resources = { path_beneath = ["~/Downloads/isos"] }
`)
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a")), nil); d.Outcome != Allowed {
		t.Fatalf("plain: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/old/a")), nil); d.Outcome != Asked || !strings.HasPrefix(d.By, "rule:r-ask-keep@") {
		t.Errorf("always ask over allow: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/isos/a")), nil); d.Outcome != Refused {
		t.Errorf("never allow over allow: %+v", d)
	}
	// A refuse rule applies when any resource is in its scope.
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a", "~/Downloads/isos/b")), nil); d.Outcome != Refused {
		t.Errorf("one refused path refuses the call: %+v", d)
	}
	// Several calls: the most restrictive call decides.
	in := f.input(assistant, trash("~/Downloads/a"), proposal.Call{Action: "files.trash", Args: map[string]any{"paths": []any{"~/Documents/x"}}})
	if d := f.eng.Decide(in, nil); d.Outcome != Asked {
		t.Errorf("two calls, one asks: %+v", d)
	}
}

// Hard limits: locked actions are refused whatever the rules say, always
// ask ones never become automatic, wrapping a command does not hide it.
func TestHardLimits(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-everything"
effect = "allow-tell"
actions = ["*"]
requesters = ["any"]
expires = "30d"
`)
	locked := []proposal.Call{
		{Action: "selinux.mode", Args: map[string]any{"mode": "permissive"}},
		toolExec("setenforce", "0"),
		toolExec("sudo", "-n", "setenforce", "0"),
		toolExec("/usr/bin/sudo", "env", "FOO=1", "/usr/sbin/setenforce", "0"),
		toolExec("semodule", "-X", "300", "-r", "basalt_agent"),
		toolExec("dd", "if=/dev/zero", "of=/dev/sda", "bs=1M"),
		toolExec("mkfs.ext4", "/dev/sdb1"),
		toolExec("cryptsetup", "luksKillSlot", "/dev/sda3", "0"),
		toolExec("mokutil", "--delete", "key.der"),
		toolExec("usermod", "-aG", "wheel", "mallory"),
		toolExec("systemctl", "stop", "basalt-ledger.service"),
		toolExec("systemctl", "mask", "basalt-gated.service"),
		{Action: "selinux.boolean", Args: map[string]any{"name": "basalt_agent_direct_egress", "value": "on"}},
	}
	for _, c := range locked {
		r := req{kind: "tool", name: "tui-test", uid: 1000}
		if c.Action == "selinux.boolean" {
			r = req{kind: "system-assistant", uid: 0}
		}
		d := f.eng.Decide(f.input(r, c), nil)
		if d.Outcome != Refused || !strings.HasPrefix(d.By, "hard-limit:locked:") {
			t.Errorf("%v: got %s by %s, want refused by a locked hard limit", c, d.Outcome, d.By)
		}
	}
	notLocked := []proposal.Call{toolExec("systemctl", "restart", "nginx"), toolExec("semodule", "-l"), toolExec("dd", "if=a", "of=b")}
	for _, c := range notLocked {
		if d := f.eng.Decide(f.input(req{kind: "tool", name: "tui-test", uid: 1000}, c), nil); d.Outcome == Refused {
			t.Errorf("%v: refused by %s", c, d.By)
		}
	}
	always := []struct {
		r    req
		call proposal.Call
	}{
		{req{kind: "system-assistant", uid: 0}, proposal.Call{Action: "snapshot.delete", Args: map[string]any{"snapshot": "12"}}},
		{assistant, proposal.Call{Action: "files.trash.empty", Args: map[string]any{}}},
		{agentReq, proposal.Call{Action: "screen.capture", Args: map[string]any{}}},
		{agentReq, proposal.Call{Action: "agent.control", Args: map[string]any{"reason": "click the button"}}},
		{req{kind: "tool", name: "tui-test", uid: 1000}, toolExec("rm", "-rf", "x")}, // destructive hint below
	}
	for _, c := range always[:4] {
		d := f.eng.Decide(f.input(c.r, c.call), nil)
		if d.Outcome != Asked || !strings.HasPrefix(d.By, "hard-limit:always-ask:") {
			t.Errorf("%s: got %s by %s, want asked by a hard limit", c.call.Action, d.Outcome, d.By)
		}
	}
}

// A request that follows untrusted content (prompt injection) does not
// reach allow rules; restrictive rules still apply to it.
func TestInjectionTaint(t *testing.T) {
	f := newFixture(t, tidyRule, `
[[rule]]
id = "r-uploads"
effect = "allow-tell"
actions = ["net.upload"]
requesters = ["assistant"]
resources = { hosts = ["backup.example.org"] }
expires = "30d"

[[rule]]
id = "r-web-ok"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
resources = { path_beneath = ["~/tmp"] }
taint_max = "web"
`)
	for _, taint := range []string{"web", "personal"} {
		r := assistant
		r.taint = taint
		if d := f.eng.Decide(f.input(r, trash("~/Downloads/a")), nil); d.Outcome != Asked {
			t.Errorf("%s-tainted request matched an allow rule: %+v", taint, d)
		}
		up := proposal.Call{Action: "net.upload", Args: map[string]any{"host": "backup.example.org"}}
		if d := f.eng.Decide(f.input(r, up), nil); d.Outcome != Asked {
			t.Errorf("%s-tainted upload allowed: %+v", taint, d)
		}
	}
	r := assistant
	r.taint = "system"
	if d := f.eng.Decide(f.input(r, trash("~/Downloads/a")), nil); d.Outcome != Allowed {
		t.Errorf("system taint is within the default taint_max: %+v", d)
	}
	r.taint = "web"
	if d := f.eng.Decide(f.input(r, trash("~/tmp/a")), nil); d.Outcome != Allowed {
		t.Errorf("a rule that accepts web taint: %+v", d)
	}
	// A destination from content is never one the rule lists.
	clean := proposal.Call{Action: "net.upload", Args: map[string]any{"host": "x.evil.example"}}
	if d := f.eng.Decide(f.input(assistant, clean), nil); d.Outcome != Asked {
		t.Errorf("an unlisted destination: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, proposal.Call{Action: "net.upload", Args: map[string]any{"host": "BACKUP.example.org."}}), nil); d.Outcome != Allowed {
		t.Errorf("a listed destination (case, trailing dot): %+v", d)
	}
}

// An allow rule without destinations never covers a call that sends
// somewhere: a new destination always asks.
func TestNewDestinationsAsk(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-c3"
effect = "allow-tell"
actions = ["class:C3"]
requesters = ["assistant"]
expires = "30d"
`)
	up := proposal.Call{Action: "net.upload", Args: map[string]any{"host": "a.example"}}
	if d := f.eng.Decide(f.input(assistant, up), nil); d.Outcome != Asked {
		t.Errorf("%+v", d)
	}
}

// The emergency stop suspends every allow rule; refusals stay refusals.
func TestStop(t *testing.T) {
	f := newFixture(t, tidyRule)
	in := f.input(assistant, trash("~/Downloads/a"))
	in.Stopped = true
	if d := f.eng.Decide(in, nil); d.Outcome != Asked || d.By != "stop" {
		t.Errorf("stopped: %+v", d)
	}
	in = f.input(req{kind: "tool", name: "x", uid: 1000}, toolExec("setenforce", "0"))
	in.Stopped = true
	if d := f.eng.Decide(in, nil); d.Outcome != Refused {
		t.Errorf("locked while stopped: %+v", d)
	}
}

type fakeCounter struct {
	n      map[string]int
	paused map[string]bool
}

func (c fakeCounter) Count(rule, chain string, since time.Time) int {
	return c.n[rule+"|"+chain] + c.n[rule+"|*"]
}
func (c fakeCounter) Paused(rule string) bool { return c.paused[rule] }

// Limits turn an allow into ask and name the rule; a paused rule allows
// nothing until a person looks.
func TestLimits(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-lim"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
resources = { path_beneath = ["~/Downloads"] }
limits = { per_run_items = 2, per_day = 3 }
`)
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a", "~/Downloads/b", "~/Downloads/c")), fakeCounter{}); d.Outcome != Asked || d.Limit == nil {
		t.Errorf("per_run_items: %+v", d)
	}
	c := fakeCounter{n: map[string]int{"system/r-lim|*": 3}}
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a")), c); d.Outcome != Asked || d.Limit == nil || !strings.Contains(d.Limit.Limit, "per_day") {
		t.Errorf("per_day: %+v", d)
	}
	c = fakeCounter{paused: map[string]bool{"system/r-lim": true}}
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a")), c); d.Outcome != Asked {
		t.Errorf("paused rule: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a")), fakeCounter{}); d.Outcome != Allowed {
		t.Errorf("within limits: %+v", d)
	}
}

// Conditions: outside the window the rule does not match; a value the
// gate cannot read never lets an allow rule match but keeps a
// restrictive one in force.
func TestConditions(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-weekend"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
when = { days = "sat,sun" }

[[rule]]
id = "r-office"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
when = { days = "mon-fri", hours = "09:00-18:00", power = "ac" }
resources = { path_beneath = ["~/work"] }

[[rule]]
id = "r-away"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
when = { presence = "away" }
resources = { path_beneath = ["~/away"] }

[[rule]]
id = "r-never-away"
effect = "refuse"
actions = ["files.trash"]
requesters = ["assistant"]
when = { presence = "away" }
resources = { path_beneath = ["~/precious"] }
`)
	if d := f.eng.Decide(f.input(assistant, trash("~/x")), nil); d.Outcome != Asked {
		t.Errorf("weekend rule on a Friday: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/work/x")), nil); d.Outcome != Allowed {
		t.Errorf("office hours on AC: %+v", d)
	}
	in := f.input(assistant, trash("~/work/x"))
	in.Env.Power = "battery"
	if d := f.eng.Decide(in, nil); d.Outcome != Asked {
		t.Errorf("on battery: %+v", d)
	}
	in = f.input(assistant, trash("~/work/x"))
	in.Env.Now = time.Date(2026, 10, 9, 20, 0, 0, 0, time.Local)
	if d := f.eng.Decide(in, nil); d.Outcome != Asked {
		t.Errorf("after hours: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/away/x")), nil); d.Outcome != Asked {
		t.Errorf("unknown presence let an allow rule match: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/precious/x")), nil); d.Outcome != Refused {
		t.Errorf("unknown presence must keep a refuse rule: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/x")), nil); d.Outcome != Asked {
		t.Errorf("%+v", d)
	}
}

// Expired rules allow nothing.
func TestExpiry(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-old"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
expires = "2026-10-01"
`)
	if d := f.eng.Decide(f.input(assistant, trash("~/x")), nil); d.Outcome != Asked {
		t.Errorf("expired rule matched: %+v", d)
	}
}

// Scheduled requests match only the rule that started them, and a rule
// with a trigger matches nothing else (content cannot trigger it).
func TestSchedule(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-friday"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
trigger = { schedule = "fri 18:00" }
resources = { path_beneath = ["~/Downloads"] }
`)
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/a")), nil); d.Outcome != Asked {
		t.Errorf("a triggered rule matched a normal request: %+v", d)
	}
	s := req{kind: "schedule", name: "r-friday", uid: 1000, origin: "schedule"}
	if d := f.eng.Decide(f.input(s, trash("~/Downloads/a")), nil); d.Outcome != Allowed {
		t.Errorf("its own schedule: %+v", d)
	}
	s.name = "r-other"
	if d := f.eng.Decide(f.input(s, trash("~/Downloads/a")), nil); d.Outcome != Asked {
		t.Errorf("another schedule: %+v", d)
	}
}

// User rules cover only their user's own C0 to C3 actions.
func TestUserScope(t *testing.T) {
	f := newFixture(t, `
[[rule]]
id = "r-user-tidy"
effect = "allow-tell"
actions = ["files.trash"]
requesters = ["assistant"]
`)
	if d := f.eng.Decide(f.input(assistant, trash("~/a")), nil); d.Outcome != Allowed {
		t.Errorf("own user: %+v", d)
	}
	if d := f.eng.Decide(f.input(req{kind: "assistant", uid: 1001}, trash("/home/bo/a")), nil); d.Outcome != Asked {
		t.Errorf("another user: %+v", d)
	}
	reg := testReg(t)
	bad := []string{
		`[[rule]]
id = "r-u1"
effect = "allow-tell"
actions = ["unit.restart"]
requesters = ["system-assistant"]`,
		`[[rule]]
id = "r-u2"
effect = "allow-tell"
actions = ["class:C4"]
requesters = ["person"]`,
		`[[rule]]
id = "r-u3"
effect = "allow-quiet"
actions = ["*"]
requesters = ["person"]`,
	}
	for _, src := range bad {
		rs, err := ParseRules(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(&rs[0], reg, ScopeUser, friNoon); err == nil {
			t.Errorf("user rule accepted:\n%s", src)
		}
	}
	// Tightening rules may name anything.
	rs, _ := ParseRules(`[[rule]]
id = "r-u4"
effect = "refuse"
actions = ["*"]
requesters = ["agent"]`)
	if err := Validate(&rs[0], reg, ScopeUser, friNoon); err != nil {
		t.Errorf("a user refuse rule: %v", err)
	}
}

func TestValidate(t *testing.T) {
	reg := testReg(t)
	check := func(src, scope string) (Rule, error) {
		rs, err := ParseRules(src)
		if err != nil {
			return Rule{}, err
		}
		err = Validate(&rs[0], reg, scope, friNoon)
		return rs[0], err
	}
	// Above C1 an allow rule expires: 90 days by default, at most a year.
	r, err := check(`[[rule]]
id = "r-v1"
effect = "allow-tell"
actions = ["unit.restart"]
requesters = ["system-assistant"]`, ScopeSystem)
	if err != nil || !r.ExpiresAt().Equal(friNoon.Add(90*24*time.Hour)) {
		t.Errorf("default expiry: %v %q", err, r.Expires)
	}
	if _, err := check(`[[rule]]
id = "r-v2"
effect = "allow-tell"
actions = ["unit.restart"]
requesters = ["system-assistant"]
expires = "400d"`, ScopeSystem); err == nil {
		t.Error("an expiry beyond a year was accepted")
	}
	if _, err := check(`[[rule]]
id = "r-v3"
effect = "allow-quiet"
actions = ["unit.restart"]
requesters = ["system-assistant"]`, ScopeSystem); err == nil {
		t.Error("allow-quiet above C1 was accepted")
	}
	for _, src := range []string{
		"[[rule]]\nid = \"bad id\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]",
		"[[rule]]\nid = \"r-a\"\neffect = \"maybe\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = []\nrequesters = [\"any\"]",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"robot\"]",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]\ntaint_max = \"dirty\"",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]\nwhen = { hours = \"25:00-26:00\" }",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]\ntrigger = { schedule = \"someday\" }",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]\nresources = { path_beneath = [\"relative\"] }",
		"[[rule]]\nid = \"r-a\"\neffect = \"ask\"\nactions = [\"x.y\"]\nrequesters = [\"any\"]\nscope = \"system\"\n",
	} {
		if _, err := check(src, ScopeSystem); err == nil {
			t.Errorf("accepted:\n%s", src)
		}
	}
}

func TestPresets(t *testing.T) {
	reg := testReg(t)
	ps, err := LoadPresets("../../presets")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"careful", "balanced", "hands-off"} {
		rules, ok := ps[n]
		if !ok {
			t.Fatalf("preset %s missing", n)
		}
		for i := range rules {
			if err := Validate(&rules[i], reg, ScopeSystem, friNoon); err != nil {
				t.Errorf("%s %s: %v", n, rules[i].ID, err)
			}
		}
	}
	// Careful asks before every change, from anyone, including the person.
	eng := NewEngine(reg, testLimits(t), home)
	careful := ps["careful"]
	for i := range careful {
		_ = Validate(&careful[i], reg, ScopeSystem, friNoon)
	}
	eng.SetRules(careful)
	f := &fixture{t: t, reg: reg, eng: eng}
	for _, r := range []req{assistant, agentReq, {kind: "person", uid: 1000}} {
		if d := eng.Decide(f.input(r, trash("~/a")), nil); d.Outcome != Asked {
			t.Errorf("careful, %s: %+v", r.kind, d)
		}
	}
	// Balanced: the assistant's undoable requests run and tell; agents ask.
	bal := ps["balanced"]
	for i := range bal {
		_ = Validate(&bal[i], reg, ScopeSystem, friNoon)
	}
	eng.SetRules(bal)
	if d := eng.Decide(f.input(assistant, trash("~/a")), nil); d.Outcome != Allowed || d.Effect != AllowTell {
		t.Errorf("balanced, assistant: %+v", d)
	}
	if d := eng.Decide(f.input(agentReq, trash("~/a")), nil); d.Outcome != Asked {
		t.Errorf("balanced, agent: %+v", d)
	}
	sa := req{kind: "system-assistant", uid: 0}
	if d := eng.Decide(f.input(sa, proposal.Call{Action: "unit.restart", Args: map[string]any{"unit": "nginx.service"}}), nil); d.Outcome != Asked || d.Effect != AskRemember || d.Remember != "8h" {
		t.Errorf("balanced, system fix: %+v", d)
	}
}

func TestNarrowRule(t *testing.T) {
	f := newFixture(t)
	in := f.input(assistant, trash("~/Downloads/a"))
	src := Rule{ID: "r-src", Effect: AskRemember, Remember: "8h", Scope: ScopeUser, UID: 1000}
	r, err := NarrowRule(in.P, in.Calls, src, "r-tmp-1", friNoon)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(&r, f.reg, ScopeUser, friNoon); err != nil {
		t.Fatal(err)
	}
	r.UID = 1000
	f.eng.SetRules([]Rule{r})
	if d := f.eng.Decide(in, nil); d.Outcome != Allowed {
		t.Errorf("remembered: %+v", d)
	}
	if d := f.eng.Decide(f.input(assistant, trash("~/Downloads/b")), nil); d.Outcome != Asked {
		t.Errorf("another file is not remembered: %+v", d)
	}
	later := f.input(assistant, trash("~/Downloads/a"))
	later.Env.Now = friNoon.Add(9 * time.Hour)
	if d := f.eng.Decide(later, nil); d.Outcome != Asked {
		t.Errorf("after 8 hours: %+v", d)
	}
}

func TestSentence(t *testing.T) {
	rs, _ := ParseRules(tidyRule)
	got := rs[0].Sentence()
	want := "Let the assistant do group:files.tidy without asking, and tell you in ~/Downloads except ~/Downloads/keep"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPowerFrom(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, typ, online string) {
		d := filepath.Join(dir, name)
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "type"), []byte(typ+"\n"), 0o644)
		if online != "" {
			_ = os.WriteFile(filepath.Join(d, "online"), []byte(online+"\n"), 0o644)
		}
	}
	if p := PowerFrom(dir); p != "ac" {
		t.Errorf("no supplies (a desktop): %s", p)
	}
	mk("BAT0", "Battery", "")
	mk("AC", "Mains", "0")
	if p := PowerFrom(dir); p != "battery" {
		t.Errorf("on battery: %s", p)
	}
	mk("AC", "Mains", "1")
	if p := PowerFrom(dir); p != "ac" {
		t.Errorf("plugged in: %s", p)
	}
	if p := PowerFrom(filepath.Join(dir, "missing")); p != Unknown {
		t.Errorf("unreadable: %s", p)
	}
}
