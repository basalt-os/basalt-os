// Command basalt-assistantd is the assistant's event engine: it watches the
// journal, disk usage and snapshots, and stores diagnoses with proposed
// changes for a person to apply. It never changes the system itself.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

var version = "dev"

func main() {
	path := config.DefaultPath
	args := os.Args[1:]
	if len(args) > 1 && args[0] == "--config" {
		path, args = args[1], args[2:]
	}
	if len(args) > 0 && args[0] == "--probe-write" {
		os.Exit(probeWrite(args[1:]))
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-assistantd:", err)
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "proposals"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "basalt-assistantd:", err)
		os.Exit(1)
	}
	al := audit.New(cfg.AuditPath, "basalt-assistantd")
	layer := decide.FromConfig(cfg.Backend, cfg.ModelEndpoint, cfg.Model, al, cfg.Thresholds, cfg.DefaultThreshold)
	env := diag.Real(true, layer)
	env.HistoryPath = filepath.Join(cfg.StateDir, "disk-history.jsonl")
	eng := engine.New(cfg, env, proposal.Store{Dir: filepath.Join(cfg.StateDir, "proposals")}, al, layer, os.Stdout)
	if _, err := al.Append("start", "basalt-assistantd "+version+" started", map[string]any{"backend": layer.Backend.Name(),
		"thresholds": cfg.Thresholds, "default_threshold": layer.Default}); err != nil {
		fmt.Fprintln(os.Stderr, "basalt-assistantd: audit log:", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_ = eng.Run(ctx)
	_, _ = al.Append("stop", "basalt-assistantd stopped", eng.Stats())
}

// probeWrite tries to create each path and reports the result. It exists
// for the confinement test: run inside the daemon's SELinux domain, every
// path outside the state directory must be refused.
func probeWrite(paths []string) int {
	bad := 0
	for _, p := range paths {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Printf("refused  %s: %v\n", p, err)
			continue
		}
		f.Close()
		_ = os.Remove(p)
		fmt.Printf("WRITABLE %s\n", p)
		bad++
	}
	return bad
}
