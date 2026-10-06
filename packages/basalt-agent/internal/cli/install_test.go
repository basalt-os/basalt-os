package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestInstallToolsPresent(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	found := func(p string) (string, error) { return "/usr/bin/" + p, nil }

	err := installToolsPresent("npm", []string{"nodejs", "npm", "git", "ripgrep"}, missing)
	if err == nil || !strings.Contains(err.Error(), "sudo dnf install nodejs npm git ripgrep") {
		t.Fatalf("missing npm: %v", err)
	}
	if err := installToolsPresent("npm", []string{"nodejs", "npm"}, found); err != nil {
		t.Fatalf("npm present: %v", err)
	}
	err = installToolsPresent("pip", nil, missing)
	if err == nil || !strings.Contains(err.Error(), "sudo dnf install python3") {
		t.Fatalf("missing python3: %v", err)
	}
	if err := installToolsPresent("none", nil, missing); err != nil {
		t.Fatalf("method none: %v", err)
	}
}
