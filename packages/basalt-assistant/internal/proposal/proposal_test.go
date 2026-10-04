package proposal

import (
	"errors"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

func TestFingerprintBindsCommands(t *testing.T) {
	p := &Proposal{ID: "p-abc123", Status: Pending, Key: "k",
		Actions: []action.Action{{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}}}}
	a, err := p.Fingerprint()
	if err != nil || len(a) != 8 {
		t.Fatalf("fingerprint %q %v", a, err)
	}
	p.Actions[0].Params["unit"] = "sshd.service"
	b, _ := p.Fingerprint()
	if a == b {
		t.Fatal("fingerprint did not change with the command")
	}
	s := Store{Dir: t.TempDir()}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	q, err := s.Load(p.ID)
	if err != nil || q.Actions[0].Params["unit"] != "sshd.service" {
		t.Fatalf("load %+v %v", q, err)
	}
	if s.FindOpen("k") == nil {
		t.Error("FindOpen")
	}
	if _, err := s.Load("../../etc/passwd"); err == nil {
		t.Error("path traversal in id accepted")
	}
}

// The confined view (daemon, MCP) proposes no file restore or rollback: it
// is held as a hint, and storing or applying one is refused.
func TestConfinedSnapshotActionsAreHints(t *testing.T) {
	restore := []action.Action{
		{Kind: action.FileRestore, Params: map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "27"}},
		{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}},
	}
	s := Store{Dir: t.TempDir()}
	for _, src := range []string{"daemon", "mcp"} {
		p := &Proposal{ID: "p-" + src + "01", Source: src, Status: Pending, Key: "k", Actions: append([]action.Action(nil), restore...)}
		if err := p.Validate(); !errors.Is(err, ErrNeedsRootView) {
			t.Fatalf("%s: validate %v", src, err)
		}
		if err := s.Save(p); err == nil {
			t.Fatalf("%s: stored a restore from the confined view", src)
		}
		if !p.HoldForRoot("test") || len(p.Actions) != 0 || len(p.Hints) != 1 || len(p.Hints[0].Actions) != 2 || !p.NeedsReview {
			t.Fatalf("%s: hold %+v", src, p)
		}
		if err := s.Save(p); err != nil {
			t.Fatalf("%s: a hint must be storable: %v", src, err)
		}
	}
	// The root command line may propose it.
	p := &Proposal{ID: "p-cli001", Source: "cli", Status: Pending, Key: "k", Actions: restore}
	if p.HoldForRoot("test") || p.Validate() != nil || s.Save(p) != nil {
		t.Fatalf("cli proposal refused: %v", p.Validate())
	}
	// A daemon proposal without snapshot actions is untouched.
	q := &Proposal{ID: "p-dmn002", Source: "daemon", Status: Pending, Actions: restore[1:]}
	if q.HoldForRoot("test") || q.Validate() != nil {
		t.Fatal("restart held")
	}
	// Invalid parameters (find's error text as a path) are refused too.
	bad := &Proposal{ID: "p-bad001", Source: "cli", Status: Pending, Actions: []action.Action{{Kind: action.SELinuxRestorecon,
		Params: map[string]string{"path": "/usr/bin/find: '/var/www': No such file or directory"}}}}
	if bad.Validate() == nil || s.Save(bad) == nil {
		t.Fatal("invalid path stored")
	}
}
