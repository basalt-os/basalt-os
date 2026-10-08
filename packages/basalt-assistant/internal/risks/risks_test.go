package risks

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAcceptClear(t *testing.T) {
	Path = filepath.Join(t.TempDir(), "security", "accepted-risks.json")
	f, err := Load()
	if err != nil || len(f.Risks) != 0 {
		t.Fatalf("empty: %v %v", f, err)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := Accept("encryption", "edimar", now); err != nil {
		t.Fatal(err)
	}
	f, _ = Load()
	if a := f.Risks["encryption"]; a.By != "edimar" || !a.At.Equal(now) {
		t.Fatalf("got %+v", a)
	}
	if err := Accept("selinux", "edimar", now); err == nil {
		t.Error("SELinux must never be accepted")
	}
	if err := Accept("tpm", "../root", now); err == nil {
		t.Error("bad account name accepted")
	}
	if err := Clear("encryption"); err != nil {
		t.Fatal(err)
	}
	if err := Clear("encryption"); err != nil {
		t.Fatal("clearing twice:", err)
	}
	f, _ = Load()
	if len(f.Risks) != 0 {
		t.Fatalf("left: %v", f.Risks)
	}
}
