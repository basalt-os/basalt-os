package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// guard records every program the command line starts and answers
// nothing.
type guard struct{ ran []string }

func (g *guard) Read(ctx context.Context, argv ...string) runner.Result {
	g.ran = append(g.ran, strings.Join(argv, " "))
	return runner.Result{Code: 1}
}

// The desktop's read helper (libexec/assistant-read in basalt-shell) runs
// these as root in the desktop's domain: none of them may start rpm or
// dnf. Package queries run only in the assistant's executors
// (basalt-updates-check, basalt-drivers-refresh, basalt-apply@).
func TestReadHelperCommandsRunNoRPMOrDNF(t *testing.T) {
	d := t.TempDir()
	cache := `{"gpus":[],"recommendation":{"action":"none"},"state":{},"secure_boot":{},"license":{"name":"x"},"docs":"d"}`
	if err := os.WriteFile(filepath.Join(d, "drivers.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(d, "updates.json"), []byte(`{"checked":"2026-10-06T00:00:00Z","updates":[{"name":"tzdata","nevra":"tzdata-2026c-1.fc44.noarch","group":"system"}]}`), 0o600)
	// Exactly the forms the read helper allows (with the --cached it adds).
	for _, argv := range [][]string{
		{"drivers", "--json", "--cached"},
		{"drivers", "install", "nvidia", "--json", "--cached"},
		{"drivers", "rollback", "--json", "--cached"},
		{"updates", "--json"},
		{"updates", "status", "--json"},
		{"updates", "install", "--json"},
		{"updates", "install", "--security", "--json"},
		{"updates", "rollback", "--json"},
		{"channels", "--json"},
		{"pending", "--json"},
		{"status", "--json"},
		{"disk", "--json"},
		{"snapshots", "--json"},
		{"why", "nginx.service", "--json"},
		{"fix", "selinux", "--json"},
		{"audit", "20"},
		{"keyboard", "--json"},
		{"keyboard", "set", "br,us(intl)", "--json"},
		{"keyboard", "set", "br", "--options", "grp:alt_shift_toggle", "--json"},
	} {
		g := &guard{}
		o, err := parse(argv)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Defaults()
		cfg.StateDir, cfg.AuditPath = d, filepath.Join(d, "audit.jsonl")
		layer := decide.FromConfigFull(cfg.DecideConfig(), nil, cfg.Thresholds, cfg.DefaultThreshold)
		env := diag.Real(false, layer)
		env.R, env.Policy, env.Isolate = g, selinux.NewPolicy(g), nil
		env.Glob = func(string) []string { return nil }
		env.SnapshotDir = filepath.Join(d, "snap")
		env.HistoryPath = filepath.Join(d, "history.jsonl")
		a := &app{o: o, cfg: cfg, env: env, layer: layer, store: proposal.Store{Dir: filepath.Join(d, "proposals")},
			audit: audit.New(cfg.AuditPath, "basalt"), out: &bytes.Buffer{}}
		_ = a.dispatch(context.Background())
		for _, r := range g.ran {
			// Nor localectl: the system's keyboard changes only when the
			// person applies the keyboard.system proposal (the executor).
			if p := strings.Fields(r)[0]; p == "rpm" || p == "dnf" || p == "dnf5" || p == "rpmkeys" || p == "localectl" {
				t.Errorf("basalt %s ran %q", strings.Join(argv, " "), r)
			}
		}
	}
}
