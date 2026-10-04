package auditlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainAndTamperDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.jsonl")
	l, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"start", "step", "done", "end"} {
		if err := l.Append(s, "disk-04", "text", map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	if _, err := Create(path); err == nil {
		t.Fatal("an existing log must never be overwritten")
	}
	data, _ := os.ReadFile(path)
	if n, err := Verify(bytes.NewReader(data)); err != nil || n != 4 {
		t.Fatalf("verify: %d %v", n, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	edited := strings.Replace(string(data), `"text":"text"`, `"text":"other"`, 1)
	if _, err := Verify(strings.NewReader(edited)); err == nil || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("edit not detected: %v", err)
	}
	dropped := strings.Join(append(lines[:1], lines[2:]...), "\n")
	if _, err := Verify(strings.NewReader(dropped)); err == nil {
		t.Fatal("a removed record was not detected")
	}
}
