package proposal

import (
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
