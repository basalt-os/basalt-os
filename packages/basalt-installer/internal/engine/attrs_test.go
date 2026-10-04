package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClearFileAttrsWithoutFlags(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "var/log"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "var/log/a"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unprivileged tests cannot set the flags; the walk must still work
	// and change nothing.
	restore, n, err := clearFileAttrs(dir)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
}
