// Command basalt-resolver is Basalt OS's per-session egress resolver
// (docs/network.md): a local DNS resolver that answers only allowlisted
// names for each registered session and fills the session's nftables sets,
// so confined sessions are default-deny with name-based exceptions.
//
//	basalt-resolver serve [--config FILE]   the service (root, basalt_resolver_t)
//	basalt-resolver sessions [--json]        registered sessions (yours; all as root)
//	basalt-resolver version
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/nflog"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/nft"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/server"
)

var version = "dev"

// roleBuild only makes the installed daemon and command line binaries
// differ in content (they carry different SELinux types).
var roleBuild string

const defaultConfig = "/etc/basalt-resolver/resolver.conf"

func main() {
	_ = roleBuild
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "sessions":
		err = sessions(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-resolver:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `basalt-resolver: per-session default-deny egress with DNS-aware allowlists

  basalt-resolver serve [--config FILE]   run the service
  basalt-resolver sessions [--json]       registered sessions
  basalt-resolver version
`)
}

// loadConfig reads "key = value" lines over the defaults.
func loadConfig(path string) (server.Config, string, error) {
	cfg := server.Defaults()
	ledgerSock := ledger.DefaultSocket
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, ledgerSock, nil
		}
		return cfg, ledgerSock, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return cfg, ledgerSock, fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		bad := func() error { return fmt.Errorf("%s:%d: bad value for %s", path, n, k) }
		switch k {
		case "upstream":
			if _, _, err := net.SplitHostPort(v); err != nil {
				return cfg, ledgerSock, bad()
			}
			cfg.Upstream = v
		case "ports":
			lo, hi, ok := strings.Cut(v, "-")
			a, e1 := strconv.Atoi(lo)
			b, e2 := strconv.Atoi(hi)
			if !ok || e1 != nil || e2 != nil || a < 1024 || b < a || b > 65535 {
				return cfg, ledgerSock, bad()
			}
			cfg.PortFirst, cfg.PortLast = a, b
		case "log_group":
			g, err := strconv.Atoi(v)
			if err != nil || g < 1 || g > 65535 {
				return cfg, ledgerSock, bad()
			}
			cfg.LogGroup = g
		case "min_ttl", "max_ttl", "grace":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return cfg, ledgerSock, bad()
			}
			switch k {
			case "min_ttl":
				cfg.MinTTL = d
			case "max_ttl":
				cfg.MaxTTL = d
			default:
				cfg.Grace = d
			}
		case "ledger":
			ledgerSock = v
		default:
			return cfg, ledgerSock, fmt.Errorf("%s:%d: unknown key %q", path, n, k)
		}
	}
	return cfg, ledgerSock, sc.Err()
}

func serve(args []string) error {
	path := defaultConfig
	if len(args) == 2 && args[0] == "--config" {
		path = args[1]
	} else if len(args) != 0 {
		return errors.New("usage: basalt-resolver serve [--config FILE]")
	}
	cfg, ledgerSock, err := loadConfig(path)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", 0)
	logf := func(format string, a ...any) { logger.Printf(format, a...) }
	sink := ledger.New(ledgerSock, "basalt-resolver", func(s string) { logger.Print(s) })
	fw := nft.Manager{R: nft.Exec{}}
	srv := server.New(cfg, fw, sink, logf)
	if err := srv.Start(); err != nil {
		return err
	}

	// Dropped packets of every session arrive on one nflog group.
	nl, err := nflog.Open(uint16(cfg.LogGroup))
	if err != nil {
		return err
	}
	go func() {
		for {
			pkts, err := nl.Read()
			if err != nil {
				if errors.Is(err, nflog.ErrOverrun) {
					logf("%v", err)
					continue
				}
				logf("nflog: %v", err)
				return
			}
			for _, p := range pkts {
				srv.Dropped(p)
			}
		}
	}()

	if err := os.MkdirAll(filepath.Dir(cfg.ControlSocket), 0o755); err != nil {
		return err
	}
	_ = os.Remove(cfg.ControlSocket)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.ControlSocket, Net: "unix"})
	if err != nil {
		return err
	}
	// Any user may register their own sessions (SO_PEERCRED decides what
	// they may do); SELinux decides which domains may connect at all.
	if err := os.Chmod(cfg.ControlSocket, 0o666); err != nil {
		return err
	}
	go srv.ServeControl(l)
	logf("basalt-resolver %s: control %s, upstream %s, session ports %d-%d, nflog group %d",
		version, cfg.ControlSocket, cfg.Upstream, cfg.PortFirst, cfg.PortLast, cfg.LogGroup)

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case <-tick.C:
			srv.GC()
		case <-sig:
			logf("stopping; session rules stay in place (fail closed) until the next start")
			l.Close()
			srv.Close()
			_ = nl.Close()
			sink.Flush(3 * time.Second)
			return nil
		}
	}
}

func sessions(args []string) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	c, err := net.DialTimeout("unix", server.Defaults().ControlSocket, 3*time.Second)
	if err != nil {
		return fmt.Errorf("the resolver is not running: %w", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	b, _ := json.Marshal(server.Request{Op: "list"})
	if _, err := c.Write(append(b, '\n')); err != nil {
		return err
	}
	var rep server.Reply
	if err := json.NewDecoder(c).Decode(&rep); err != nil {
		return err
	}
	if !rep.OK {
		return errors.New(rep.Error)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep.Sessions)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tUID\tPROFILE\tDNS PORT\tANSWERED\tREFUSED\tDROPPED\tCGROUP")
	for _, s := range rep.Sessions {
		fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%d\t%d\t%d\t%s\n", s.ID, s.UID, s.Profile, s.Port, s.Counts["dns_answered"],
			s.Counts["dns_refused"], s.Counts["egress_drop"]+s.Counts["dns_direct"], s.Cgroup)
	}
	return w.Flush()
}
