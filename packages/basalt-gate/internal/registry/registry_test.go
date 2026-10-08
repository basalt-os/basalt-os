package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T) *Registry {
	t.Helper()
	r := New()
	for _, dir := range []string{"../../actions.d", "../../testdata/registry"} {
		names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, n := range names {
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Add(filepath.Base(n), b); err != nil {
				t.Fatalf("%s: %v", n, err)
			}
		}
	}
	if err := r.Check(); err != nil {
		t.Fatal(err)
	}
	return r
}

func home(uid int) (string, error) { return "/home/ana", nil }

// The shipped registry describes today's actions: the system assistant's
// 11, the shell's 16 desktop actions plus 2 agent-only and 5 person-only.
func TestShipped(t *testing.T) {
	r, err := Load("../../actions.d")
	if err != nil {
		t.Fatal(err)
	}
	count := func(src string, f func(*Action) bool) int {
		n := 0
		for _, a := range r.Actions {
			if a.Source == src && f(a) {
				n++
			}
		}
		return n
	}
	all := func(*Action) bool { return true }
	if n := count("assistant.json", all); n != 22 {
		t.Errorf("assistant actions: %d", n)
	}
	if n := count("shell.json", func(a *Action) bool { return !a.PersonOnly && !a.AgentOnly }); n != 16 {
		t.Errorf("desktop actions: %d", n)
	}
	if n := count("shell.json", func(a *Action) bool { return a.AgentOnly }); n != 2 {
		t.Errorf("agent-only: %d", n)
	}
	if n := count("shell.json", func(a *Action) bool { return a.PersonOnly }); n != 5 {
		t.Errorf("person-only: %d", n)
	}
	for _, id := range []string{"gate.rule.change", "tool.exec", "agent.grant.host", "agent.grant.path", "agent.egress.change", "agent.egress.system",
		"model.download", "model.remove", "grant.folder", "grant.mailbox", "grant.site", "file.open", "knowledge.fetch", "remote.consent"} {
		if r.Actions[id] == nil {
			t.Errorf("%s not registered", id)
		}
	}
}

func TestValidateArgs(t *testing.T) {
	r := load(t)
	a, _ := r.Lookup("unit.restart")
	if _, err := a.Validate(map[string]any{"unit": "nginx.service"}); err != nil {
		t.Error(err)
	}
	for _, bad := range []map[string]any{
		{},
		{"unit": "nginx.service; rm -rf /"},
		{"unit": "nginx.service", "extra": "x"},
		{"unit": 5},
		{"unit": "a\x00.service"},
	} {
		if _, err := a.Validate(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	te, _ := r.Lookup("tool.exec")
	args, err := te.Validate(map[string]any{"tool": "tui-systemd", "operation": "unit.restart", "argv": []any{"systemctl", "restart", "nginx"}})
	if err != nil || len(args["argv"].([]any)) != 3 {
		t.Errorf("%v %v", args, err)
	}
	if _, err := te.Validate(map[string]any{"tool": "x", "operation": "y", "argv": []any{"a", 1}}); err == nil {
		t.Error("argv with a number accepted")
	}
	ma, _ := r.Lookup("agent.control")
	if _, err := ma.Validate(map[string]any{"reason": "r", "minutes": float64(99)}); err == nil {
		t.Error("minutes out of range accepted")
	}
	if args, err := ma.Validate(map[string]any{"reason": "r", "minutes": float64(5)}); err != nil || args["minutes"] != int64(5) {
		t.Errorf("%v %v", args, err)
	}
	// Open actions keep unknown arguments, numbers as integers only.
	w, _ := r.Lookup("window.move")
	if args, err := w.Validate(map[string]any{"window": "focused", "x": float64(10)}); err != nil || args["x"] != int64(10) {
		t.Errorf("%v %v", args, err)
	}
	if _, err := w.Validate(map[string]any{"x": 1.5}); err == nil {
		t.Error("a fraction accepted")
	}
}

func TestExtract(t *testing.T) {
	r := load(t)
	fm, _ := r.Lookup("files.move")
	args, err := fm.Validate(map[string]any{"moves": []any{map[string]any{"from": "/home/ana/a", "to": "/home/ana/b/a"}}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := fm.Extract(args, 1000, home)
	if err != nil || len(res) != 2 || res[0].Value != "/home/ana/a" || res[1].Value != "/home/ana/b/a" {
		t.Fatalf("%v %v", res, err)
	}
	ft, _ := r.Lookup("files.trash")
	args, _ = ft.Validate(map[string]any{"paths": []any{"~/Downloads//x/"}})
	res, err = ft.Extract(args, 1000, home)
	if err != nil || res[0].Value != "/home/ana/Downloads/x" {
		t.Fatalf("%v %v", res, err)
	}
	for _, p := range []string{"~/../bo/x", "relative/x", "/home/ana/../../etc/shadow"} {
		args, _ = ft.Validate(map[string]any{"paths": []any{p}})
		if _, err := ft.Extract(args, 1000, home); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	up, _ := r.Lookup("net.upload")
	args, _ = up.Validate(map[string]any{"host": "Backup.Example.org."})
	res, _ = up.Extract(args, 1000, home)
	if res[0].Value != "backup.example.org" {
		t.Error(res)
	}
	di, _ := r.Lookup("driver.install")
	args, _ = di.Validate(map[string]any{"driver": "nvidia", "variant": "display", "kernel": "6.19.10-200.fc44.x86_64", "license": strings.Repeat("a", 64)})
	res, _ = di.Extract(args, 0, home)
	if len(res) != 1 || res[0].String() != "package:basalt-nonfree-release" {
		t.Error(res)
	}
}

func TestBadFiles(t *testing.T) {
	for name, src := range map[string]string{
		"version":       `{"registry": 2, "actions": []}`,
		"unknown field": `{"registry": 1, "actions": [], "extra": 1}`,
		"bad class":     `{"registry": 1, "executors": {"e": {}}, "actions": [{"id": "a.b", "class": "C9", "args": {}, "executor": "e"}]}`,
		"bad id":        `{"registry": 1, "executors": {"e": {}}, "actions": [{"id": "Ab", "class": "C1", "args": {}, "executor": "e"}]}`,
		"both only":     `{"registry": 1, "executors": {"e": {}}, "actions": [{"id": "a.b", "class": "C1", "args": {}, "executor": "e", "person_only": true, "agent_only": true}]}`,
		"bad pattern":   `{"registry": 1, "executors": {"e": {}}, "actions": [{"id": "a.b", "class": "C1", "args": {"x": {"type": "string", "pattern": "("}}, "executor": "e"}]}`,
		"bad kind":      `{"registry": 1, "executors": {"e": {}}, "actions": [{"id": "a.b", "class": "C1", "args": {}, "executor": "e", "resources": [{"kind": "soul", "arg": "x"}]}]}`,
	} {
		if err := New().Add(name, []byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := New()
	if err := r.Add("a", []byte(`{"registry": 1, "actions": [{"id": "a.b", "class": "C1", "args": {}, "executor": "nobody"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(); err == nil {
		t.Error("unknown executor accepted")
	}
	r = load(t)
	if err := r.Add("dup", []byte(`{"registry": 1, "actions": [{"id": "unit.restart", "class": "C0", "args": {}, "executor": "basalt-gate"}]}`)); err == nil {
		t.Error("an action defined twice (to lower its class) was accepted")
	}
}

// keyboard.system: the system's keyboard, decided like any system change.
func TestKeyboardSystemArgs(t *testing.T) {
	r := load(t)
	a, err := r.Lookup("keyboard.system")
	if err != nil || a.Class != "C2" || !a.System {
		t.Fatalf("%+v %v", a, err)
	}
	for _, good := range []map[string]any{
		{"layouts": "br,us(intl)", "options": "grp:alt_shift_toggle,compose:ralt", "model": "pc105", "keymap": "br"},
		{"layouts": "br", "options": "", "model": "pc105", "keymap": "br-abnt2"},
		{"layouts": "de(nodeadkeys)", "model": "pc105", "keymap": "de-nodeadkeys"},
	} {
		if _, err := a.Validate(good); err != nil {
			t.Errorf("%v: %v", good, err)
		}
	}
	for _, bad := range []map[string]any{
		{"layouts": "br; reboot", "model": "pc105", "keymap": "br"},
		{"layouts": "br,us,de,fr,it", "model": "pc105", "keymap": "br"},
		{"layouts": "br", "options": "x", "model": "pc105", "keymap": "br"},
		{"layouts": "br", "model": "pc105", "keymap": "../shadow"},
		{"layouts": "br", "model": "pc105"},
		{"layouts": "br", "model": "pc105", "keymap": "br", "file": "/etc/passwd"},
	} {
		if _, err := a.Validate(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
