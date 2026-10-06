package peer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassify(t *testing.T) {
	c := DefaultConfig()
	cases := []struct {
		ctx              string
		kind             string
		decider, polkit  bool
		agent, relayNone bool
	}{
		{"unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023", "tool", false, false, false, true},
		{"unconfined_u:unconfined_r:basalt_agent_t:s0:c1,c2", "agent", false, false, true, true},
		{"unconfined_u:unconfined_r:basalt_agent_mcp_t:s0", "agent", false, false, true, true},
		{"system_u:system_r:container_t:s0:c1,c2", "agent", false, false, true, true},
		{"unconfined_u:unconfined_r:basalt_skill_t:s0", "agent", false, false, true, true},
		{"unconfined_u:unconfined_r:basalt_shell_ui_t:s0", "tool", true, false, false, true},
		{"unconfined_u:unconfined_r:basalt_gate_tty_t:s0", "tool", true, true, false, true},
		{"unconfined_u:unconfined_r:basalt_shell_t:s0", "tool", false, false, false, false},
		{"system_u:system_r:basalt_assistant_t:s0", "system-assistant", false, false, false, false},
		{"unconfined_u:unconfined_r:basalt_app_files_t:s0", "app", false, false, false, true},
		{"", "tool", false, false, false, true},
	}
	for _, x := range cases {
		r := c.Classify(Peer{UID: 1000, Context: x.ctx}, "tui-systemd/0.4")
		if r.Kind != x.kind || r.Decider != x.decider || r.Polkit != x.polkit || r.Agent != x.agent || (len(r.Relay) == 0) != x.relayNone {
			t.Errorf("%s: %+v", x.ctx, r)
		}
	}
	// A client name never makes an agent anything else.
	if r := c.Classify(Peer{Context: "u:r:basalt_agent_t:s0"}, "basalt-gate-tty"); r.Decider || r.Kind != "agent" {
		t.Errorf("%+v", r)
	}
	if n := ToolName("TUI-Systemd/0.4.0"); n != "tui-systemd" {
		t.Error(n)
	}
	if n := ToolName("/x"); n != "cli" {
		t.Error(n)
	}
}

func TestProc(t *testing.T) {
	dir := t.TempDir()
	ProcRoot = dir
	defer func() { ProcRoot = "/proc" }()
	_ = os.MkdirAll(filepath.Join(dir, "42"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "42", "cgroup"), []byte("0::/user.slice/basaltagent-0123456789ab.slice/basaltagent-0123456789ab-agent.scope\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "42", "stat"), []byte("42 (a b) c) S 1 42 42 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 98765 1000 100\n"), 0o644)
	if s := SessionOf(42); s != "s-0123456789ab" {
		t.Error(s)
	}
	if s := SessionOf(43); s != "" {
		t.Error(s)
	}
	if st, err := StartTime(42); err != nil || st != 98765 {
		t.Errorf("%d %v", st, err)
	}
	ProcRoot = "/proc"
	if _, err := StartTime(os.Getpid()); err != nil {
		t.Error(err)
	}
}
