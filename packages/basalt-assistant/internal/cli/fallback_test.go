package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// basalt fallback: the report, and the desktop.fallback proposal in the
// form the desktop's Settings, About reads (`--json`: id, commands; the
// shell then shows it by id). It never runs systemctl itself.
func TestFallbackCommand(t *testing.T) {
	d := t.TempDir()
	old := action.FallbackPaths
	action.FallbackPaths.GNOMESession = filepath.Join(d, "gnome.desktop")
	action.FallbackPaths.GDMUnits = []string{filepath.Join(d, "gdm.service")}
	action.FallbackPaths.DisplayManager = filepath.Join(d, "display-manager.service")
	t.Cleanup(func() { action.FallbackPaths = old })
	_ = os.Symlink("/usr/lib/systemd/system/greetd.service", action.FallbackPaths.DisplayManager)
	run := func(argv ...string) (string, error) {
		o, err := parse(argv)
		if err != nil {
			return "", err
		}
		cfg := config.Defaults()
		cfg.StateDir, cfg.AuditPath = d, filepath.Join(d, "audit.jsonl")
		out := &bytes.Buffer{}
		a := &app{o: o, cfg: cfg, store: proposal.Store{Dir: filepath.Join(d, "proposals")}, audit: audit.New(cfg.AuditPath, "basalt"), out: out}
		err = a.dispatch(context.Background())
		return out.String(), err
	}
	out, err := run("fallback", "--json")
	if err != nil || !strings.Contains(out, `"gnome_installed": false`) || !strings.Contains(out, `"display_manager": "greetd"`) {
		t.Fatalf("report: %s %v", out, err)
	}
	if _, err := run("fallback", "gnome", "--json"); err == nil || !strings.Contains(err.Error(), "basalt-desktop-gnome-fallback") {
		t.Fatalf("gnome without the fallback package: %v", err)
	}
	if _, err := run("fallback", "basalt", "--json"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("basalt while greetd starts: %v", err)
	}
	_ = os.WriteFile(action.FallbackPaths.GNOMESession, nil, 0o644)
	_ = os.WriteFile(action.FallbackPaths.GDMUnits[0], nil, 0o644)
	out, err = run("fallback", "gnome", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		ID       string   `json:"id"`
		Stored   bool     `json:"stored"`
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out), &ref); err != nil || !strings.HasPrefix(ref.ID, "p-") {
		t.Fatalf("%s %v", out, err)
	}
	if strings.Join(ref.Commands, "; ") != "systemctl enable --force gdm.service; systemctl disable greetd.service" {
		t.Errorf("commands: %v", ref.Commands)
	}
	if ref.Stored {
		t.Error("stored without root")
	}
	if _, err := run("fallback", "kde", "--json"); err == nil {
		t.Error("kde accepted")
	}
}
