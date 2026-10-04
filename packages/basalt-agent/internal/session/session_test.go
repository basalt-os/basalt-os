package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLevels(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		l, err := PickLevel(nil)
		if err != nil {
			t.Fatal(err)
		}
		a, b, err := ParseLevel(l)
		if err != nil || a >= b {
			t.Fatalf("%s: %d %d %v", l, a, b, err)
		}
		seen[l] = true
	}
	if len(seen) < 190 {
		t.Errorf("levels repeat too often: %d distinct of 200", len(seen))
	}
	for _, bad := range []string{"s0", "s0:c5,c5", "s0:c9,c2", "s0:c1,c1024", "s0-s0:c0.c1023", "s0:c1"} {
		if _, _, err := ParseLevel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !ValidID(NewID()) || ValidID("s-../../x") {
		t.Error("session ids")
	}
}

func TestCheckProject(t *testing.T) {
	home := t.TempDir()
	d := Dirs{Home: home, Data: filepath.Join(home, ".local/share/basalt-agent"), Config: filepath.Join(home, ".config/basalt-agent")}
	ok := filepath.Join(home, "src", "app")
	withSSH := filepath.Join(home, "src", "keys")
	for _, p := range []string{ok, filepath.Join(withSSH, ".ssh"), filepath.Join(home, ".local/share")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := CheckProject(ok, d); err != nil || got != ok {
		t.Errorf("good project refused: %v", err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	if got, err := CheckProject(link, d); err != nil || got != ok {
		t.Errorf("symlink not resolved: %s %v", got, err)
	}
	for what, p := range map[string]string{
		"home itself":      home,
		"parent of home":   filepath.Dir(home),
		"outside home":     "/etc",
		"contains .ssh":    withSSH,
		"basalt-agent dir": filepath.Join(home, ".local"),
		"missing":          filepath.Join(home, "nope"),
	} {
		if _, err := CheckProject(p, d); err == nil {
			t.Errorf("%s (%s) accepted", what, p)
		}
	}
}

func TestLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	un, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); err == nil {
		t.Error("second lock succeeded")
	}
	un()
	un2, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	un2()
}
