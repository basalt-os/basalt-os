// basalt-gated is the Basalt approval gate daemon (docs/gate.md): the one
// place where a request for a side effect becomes a decision. It runs as
// root in its own SELinux domain (basalt_gate_t), listens on
// /run/basalt-gate/gate.sock and records every request and decision in
// the audit ledger.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/polkit"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/server"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/store"
)

var version = "dev"

func main() {
	conf := flag.String("config", config.DefaultFile, "configuration file")
	check := flag.Bool("check", false, "load the configuration, registry, hard limits and presets, then exit")
	flag.Parse()
	logger := log.New(os.Stderr, "", 0)
	logf := func(f string, a ...any) { logger.Printf(f, a...) }
	if err := run(*conf, *check, logf); err != nil {
		fmt.Fprintln(os.Stderr, "basalt-gated:", err)
		os.Exit(1)
	}
}

func run(conf string, check bool, logf func(string, ...any)) error {
	server.Version = version
	cfg, err := config.Load(conf)
	if err != nil {
		return err
	}
	reg, err := registry.Load(cfg.Registry)
	if err != nil {
		return fmt.Errorf("action registry: %w", err)
	}
	limits, err := policy.LoadHardLimits(cfg.HardLimits)
	if err != nil {
		return fmt.Errorf("hard limits: %w", err)
	}
	presets, err := policy.LoadPresets(cfg.Presets)
	if err != nil {
		return fmt.Errorf("presets: %w", err)
	}
	if _, ok := presets["careful"]; !ok {
		return fmt.Errorf("presets: careful.toml is missing from %s", cfg.Presets)
	}
	now := time.Now()
	for name, rules := range presets {
		for i := range rules {
			if err := policy.Validate(&rules[i], reg, policy.ScopeSystem, now); err != nil {
				return fmt.Errorf("preset %s, rule %s: %w", name, rules[i].ID, err)
			}
		}
	}
	if check {
		fmt.Printf("ok: %d actions from %d files, %d locked and %d always-ask limits, presets", len(reg.Actions),
			len(reg.Files), len(limits.Locked), len(limits.AlwaysAsk))
		for name := range presets {
			fmt.Printf(" %s", name)
		}
		fmt.Println()
		return nil
	}
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	led := ledger.New(cfg.LedgerSocket, "basalt-gate", func(m string) { logf("%s", m) })
	srv, err := server.New(server.Options{
		Config: cfg, Reg: reg, Limits: limits, Presets: presets, Store: st, Ledger: led,
		Polkit: polkit.Pkcheck{}, Logf: logf,
		AgentName: func(session string) string {
			recs, err := ledger.Query(cfg.LedgerSocket, map[string]any{"event": "session.start", "session": session, "limit": 1})
			if err != nil || len(recs) == 0 {
				return ""
			}
			return recs[0].Subject.Profile
		},
		History: func(uid int, since time.Time) ([]ledger.Stored, error) {
			f := map[string]any{"producer": "basalt-gate", "event": "gate.", "since": since.UTC().Format(time.RFC3339Nano),
				"limit": 100000}
			if uid != 0 {
				f["uid"] = uid
			}
			return ledger.Query(cfg.LedgerSocket, f)
		},
	})
	if err != nil {
		return err
	}
	status := srv.Status()
	led.Send(ledger.Record{Event: "gate.start", Data: map[string]any{"rules": status.Rules, "state": map[bool]string{
		true: "stopped", false: "running"}[status.Stopped], "preset": status.Preset, "version": version,
		"actions": status.Actions, "seal_ok": status.SealOK}})

	ln, err := listen(cfg.Socket)
	if err != nil {
		return err
	}
	go srv.Serve(ln)
	logf("basalt-gated %s: socket %s, %d actions, %d rules (preset %s), automation %s", version, cfg.Socket,
		status.Actions, status.Rules, status.Preset, map[bool]string{true: "STOPPED", false: "on"}[status.Stopped])

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ln.Close()
	led.Flush(3 * time.Second)
	return nil
}

// listen uses the socket systemd passes (LISTEN_FDS) or creates it. Who
// may connect is SELinux's decision; what each peer may do is the gate's
// (SO_PEERCRED, SO_PEERSEC).
func listen(path string) (*net.UnixListener, error) {
	if os.Getenv("LISTEN_PID") == fmt.Sprint(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "listen")
		l, err := net.FileListener(f)
		if err != nil {
			return nil, err
		}
		ul, ok := l.(*net.UnixListener)
		if !ok {
			return nil, fmt.Errorf("the passed socket is not a Unix socket")
		}
		return ul, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		return nil, err
	}
	return ln, nil
}
