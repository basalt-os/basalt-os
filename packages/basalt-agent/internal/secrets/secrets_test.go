package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no secret-tool
	dir := t.TempDir()
	f := filepath.Join(dir, "claude.env")
	data := "# keys\nANTHROPIC_API_KEY=\"sk-test-1\"\nexport OTHER_KEY=x\n"
	if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	vals, missing, err := Load(f, "claude", []string{"ANTHROPIC_API_KEY", "NOT_THERE"})
	if err != nil {
		t.Fatal(err)
	}
	if vals["ANTHROPIC_API_KEY"] != "sk-test-1" || len(vals) != 1 {
		t.Errorf("vals %v (only listed names may be read)", vals)
	}
	if len(missing) != 1 || missing[0] != "NOT_THERE" {
		t.Errorf("missing %v", missing)
	}
	if got := ParseEnvFile(EnvFile(vals)); got["ANTHROPIC_API_KEY"] != "sk-test-1" {
		t.Errorf("round trip %v", got)
	}

	// A readable file is refused.
	if err := os.Chmod(f, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(f, "claude", []string{"ANTHROPIC_API_KEY"}); err == nil {
		t.Error("group/other readable secret file accepted")
	}
	// A missing file is not an error.
	if _, missing, err := Load(filepath.Join(dir, "none.env"), "x", []string{"A"}); err != nil || len(missing) != 1 {
		t.Errorf("missing file: %v %v", missing, err)
	}
}
