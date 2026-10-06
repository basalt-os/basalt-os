package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil || c.Socket != "/run/basalt-gate/gate.sock" || c.DefaultPreset != "careful" || !c.AllowCodeConfirm {
		t.Fatalf("%+v %v", c, err)
	}
	f := filepath.Join(t.TempDir(), "gate.conf")
	_ = os.WriteFile(f, []byte("# x\nallow_code_confirm = no\ninteractive_expiry = 2m\nrelays = basalt_shell_t:person,agent\ndeciders = a_t b_t\nagent_taint_floor = personal\n"), 0o644)
	c, err = Load(f)
	if err != nil || c.AllowCodeConfirm || c.InteractiveExpiry != 2*time.Minute || len(c.Peers.Relays["basalt_shell_t"]) != 2 ||
		len(c.Peers.Deciders) != 2 || c.AgentTaintFloor != "personal" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"nope = 1\n", "allow_code_confirm = maybe\n", "interactive_expiry = soon\n", "garbage\n", "agent_taint_floor = clean\n"} {
		_ = os.WriteFile(f, []byte(bad), 0o644)
		if _, err := Load(f); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestMigrationKeys(t *testing.T) {
	c := Default()
	if !c.Enforced("skills") || c.Enforced("shell") || !c.ExecUnits || len(c.Peers.RootRelays) != 1 {
		t.Fatalf("defaults: %+v", c)
	}
	f := filepath.Join(t.TempDir(), "gate.conf")
	_ = os.WriteFile(f, []byte("enforce = apply, shell\nexec_units = no\nroot_relays = system-assistant person\nsystemctl = /bin/true\n"), 0o644)
	c, err := Load(f)
	if err != nil || !c.Enforced("apply") || !c.Enforced("shell") || c.Enforced("skills") || c.ExecUnits ||
		len(c.Peers.RootRelays) != 2 || c.Systemctl != "/bin/true" {
		t.Fatalf("%+v %v", c, err)
	}
	_ = os.WriteFile(f, []byte("enforce = none\n"), 0o644)
	if c, err := Load(f); err != nil || len(c.Enforce) != 0 {
		t.Fatalf("none: %+v %v", c.Enforce, err)
	}
	for _, bad := range []string{"enforce = everything\n", "exec_units = maybe\n", "systemctl = systemctl\n"} {
		_ = os.WriteFile(f, []byte(bad), 0o644)
		if _, err := Load(f); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
