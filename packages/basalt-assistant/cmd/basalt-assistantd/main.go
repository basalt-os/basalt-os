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
	if len(args) > 0 && args[0] == "--probe-vsm" {
		os.Exit(probeVSM(cfg))
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "proposals"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "basalt-assistantd:", err)
		os.Exit(1)
	}
	al := audit.New(cfg.AuditPath, "basalt-assistantd")
	layer := decide.FromConfigFull(cfg.DecideConfig(), al, cfg.Thresholds, cfg.DefaultThreshold)
	env := diag.Real(true, layer)
	env.HistoryPath = filepath.Join(cfg.StateDir, "disk-history.jsonl")
	eng := engine.New(cfg, env, proposal.Store{Dir: filepath.Join(cfg.StateDir, "proposals")}, al, layer, os.Stdout)
	start := map[string]any{"backend": layer.Backend.Name(), "thresholds": cfg.Thresholds, "default_threshold": layer.Default}
	if vb, ok := layer.Backend.(*decide.VSMBackend); ok {
		// Load the knowledge and the planner now, so the start record says
		// which ones answer (or why the rules will).
		if e, err := vb.Engine(); err != nil {
			start["vsm_error"] = err.Error()
		} else {
			start["vsm"] = e.Version()
		}
	}
	if _, err := al.Append("start", "basalt-assistantd "+version+" started", start); err != nil {
		fmt.Fprintln(os.Stderr, "basalt-assistantd: audit log:", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	_ = eng.Run(ctx)
	_, _ = al.Append("stop", "basalt-assistantd stopped", eng.Stats())
}

// probeVSM loads the VSM backend as configured and answers one question
// with it. It exists for the confinement test: run inside the daemon's
// SELinux domain, it shows the knowledge and the planner are readable
// there (no denial) while --probe-write shows they are not writable.
func probeVSM(cfg config.Config) int {
	c := cfg.DecideConfig()
	vb := decide.NewVSMBackend(c.VSMKnowledgeRoot, c.VSMKnowledge, c.VSMPlanner)
	e, err := vb.Engine()
	if err != nil {
		fmt.Println("vsm:", err)
		return 1
	}
	q := decide.UnitCause("probe.service", map[string]bool{"unit_failed": true, "journal_address_in_use": true})
	a, err := vb.Answer(context.Background(), q)
	if err != nil {
		fmt.Println("vsm:", err)
		return 1
	}
	fmt.Printf("vsm: %s; %s -> %s %.3f (case %s, %d us)\n", e.Version(), q.ID, a.Top, a.Confidence, a.VSM.Case, a.VSM.Micros)
	return 0
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
