// Command basalt-mcp serves the assistant's diagnosers as MCP tools over
// stdio. Write tools only store proposals; nothing is executed.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/mcp"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

var version = "dev"

func main() {
	path := config.DefaultPath
	if len(os.Args) > 2 && os.Args[1] == "--config" {
		path = os.Args[2]
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-mcp:", err)
		os.Exit(2)
	}
	al := audit.New(cfg.AuditPath, "basalt-mcp")
	layer := decide.FromConfigFull(cfg.DecideConfig(), al, cfg.Thresholds, cfg.DefaultThreshold)
	env := diag.Real(true, layer)
	env.HistoryPath = cfg.StateDir + "/disk-history.jsonl"
	s := &mcp.Server{Env: env, Store: proposal.Store{Dir: cfg.StateDir + "/proposals"}, Audit: al,
		Decide: layer, Disk: cfg.Disk, Version: version, DriversCache: cfg.StateDir + "/drivers.json"}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := s.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "basalt-mcp:", err)
		os.Exit(1)
	}
}
