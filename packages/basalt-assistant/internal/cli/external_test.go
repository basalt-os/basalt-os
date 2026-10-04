package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestExternal(t *testing.T) {
	libexec, bin, path := t.TempDir(), t.TempDir(), t.TempDir()
	exe := func(dir, name string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	exe(bin, "basalt-ledger", 0o755)
	exe(libexec, "basalt-ledger", 0o755) // libexec comes first
	exe(bin, "basalt-notes", 0o644)      // not executable
	exe(libexec, "basalt-assistantd", 0o755)
	exe(bin, "basalt-status", 0o755) // shadowed by the built-in
	exe(path, "basalt-evil", 0o755)  // only on PATH
	_ = os.Mkdir(filepath.Join(bin, "basalt-dir"), 0o755)
	t.Setenv("PATH", path+":"+os.Getenv("PATH"))
	dirs := []string{libexec, bin}

	if p, ok := externalIn("ledger", dirs); !ok || p != filepath.Join(libexec, "basalt-ledger") {
		t.Fatalf("ledger: %q %v", p, ok)
	}
	for _, name := range []string{"status", "audit", "help", "version", "notes", "assistantd", "evil", "dir", "missing",
		"../ledger", "Ledger", "-ledger", "ledger/x", "", "a b", strings.Repeat("a", 41)} {
		if p, ok := externalIn(name, dirs); ok {
			t.Fatalf("%q resolved to %s", name, p)
		}
	}
}

// Every subcommand the dispatcher knows must be a built-in, so a
// basalt-<name> program can never shadow it.
func TestBuiltinsCoverDispatch(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(src), "func (a *app) dispatch(")
	if i < 0 {
		t.Fatal("dispatch not found")
	}
	body := string(src[i:])
	if j := strings.Index(body, "\n}\n"); j > 0 {
		body = body[:j]
	}
	for _, m := range regexp.MustCompile(`case ([^:]+):`).FindAllStringSubmatch(body, -1) {
		for _, q := range regexp.MustCompile(`"([a-z-]+)"`).FindAllStringSubmatch(m[1], -1) {
			if !builtins[q[1]] {
				t.Errorf("subcommand %q is not in builtins (external.go)", q[1])
			}
		}
	}
}
