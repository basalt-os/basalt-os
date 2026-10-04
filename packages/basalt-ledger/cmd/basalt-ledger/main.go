// Command basalt-ledger is Basalt OS's system audit service
// (docs/ledger.md). The same binary is installed under two names:
//
//	basalt-ledger    the command line (any user)
//	basalt-ledgerd   the daemon (basalt-ledger.service, domain basalt_ledger_t)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/cli"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/collect"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/server"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/sign"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/store"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm"
)

var version = "dev"

// roleBuild only makes the two installed binaries differ in content (they
// carry different SELinux types); no runtime effect.
var roleBuild string

func main() {
	_ = roleBuild
	cli.Version = version
	if filepath.Base(os.Args[0]) == "basalt-ledgerd" {
		if err := daemon(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "basalt-ledgerd:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(cli.Main(os.Args[1:]))
}

type config struct {
	Dir           string
	Socket        string
	RotateSize    int64
	RotateAge     time.Duration
	Journal       bool
	Snapshots     string
	UserProducers []string
	ExportKey     string        // auto, tpm or software
	TPMDevice     string        // the TPM resource manager
	Retention     time.Duration // 0: keep sealed files forever
	RetentionText string
}

// parseRetention reads "365d", "8760h", "forever" (or "never", "0").
func parseRetention(v string) (time.Duration, error) {
	switch v {
	case "forever", "never", "0", "":
		return 0, nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && strings.HasSuffix(v, "d") && n > 0 {
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("retention %q: a number of days (365d), a duration (8760h) or forever", v)
	}
	return d, nil
}

func loadConfig(path string) (config, error) {
	c := config{Dir: "/var/log/basalt-ledger", Socket: "/run/basalt-ledger/ledger.sock", RotateSize: 8 << 20,
		RotateAge: 24 * time.Hour, Journal: true, Snapshots: "/.snapshots", UserProducers: server.DefaultConfig().UserProducers,
		ExportKey: "auto", TPMDevice: tpm.DefaultDevice, Retention: 365 * 24 * time.Hour, RetentionText: "365d"}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
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
			return c, fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "dir":
			c.Dir = v
		case "socket":
			c.Socket = v
		case "rotate_size":
			mb, err := strconv.Atoi(strings.TrimSuffix(v, "M"))
			if err != nil || mb < 1 {
				return c, fmt.Errorf("%s:%d: rotate_size is a number of MiB", path, n)
			}
			c.RotateSize = int64(mb) << 20
		case "rotate_age":
			d, err := time.ParseDuration(v)
			if err != nil {
				return c, fmt.Errorf("%s:%d: %v", path, n, err)
			}
			c.RotateAge = d
		case "journal":
			c.Journal = v == "yes" || v == "true" || v == "1"
		case "snapshots":
			c.Snapshots = v
		case "user_producers":
			c.UserProducers = strings.Fields(v)
		case "export_key":
			if v != "auto" && v != "tpm" && v != "software" {
				return c, fmt.Errorf("%s:%d: export_key is auto, tpm or software", path, n)
			}
			c.ExportKey = v
		case "tpm_device":
			c.TPMDevice = v
		case "retention":
			d, err := parseRetention(v)
			if err != nil {
				return c, fmt.Errorf("%s:%d: %v", path, n, err)
			}
			c.Retention, c.RetentionText = d, v
			if d == 0 {
				c.RetentionText = "forever"
			}
		default:
			return c, fmt.Errorf("%s:%d: unknown key %q", path, n, k)
		}
	}
	return c, sc.Err()
}

func daemon(args []string) error {
	path := "/etc/basalt-ledger/ledger.conf"
	if len(args) == 2 && args[0] == "--config" {
		path = args[1]
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", 0)
	logf := func(format string, a ...any) { logger.Printf(format, a...) }
	// The directory and the public key are readable (users check exports
	// with it); the chain files, state and the private key are root only.
	for d, mode := range map[string]os.FileMode{cfg.Dir: 0o755, filepath.Join(cfg.Dir, "state"): 0o700, filepath.Join(cfg.Dir, "keys"): 0o755} {
		if err := os.MkdirAll(d, mode); err != nil {
			return err
		}
		if err := os.Chmod(d, mode); err != nil {
			return err
		}
	}
	st, err := store.Open(filepath.Join(cfg.Dir, "ledger.jsonl"), os.Geteuid() == 0)
	if err != nil {
		return err
	}
	defer st.Close()
	key, note, err := exportKey(cfg, logf)
	if err != nil {
		return err
	}
	server.KeyNote, server.Retention = note, cfg.RetentionText
	server.PublicKeyFile = filepath.Join(cfg.Dir, "keys", "export.pub")
	sc := server.DefaultConfig()
	sc.UserProducers = cfg.UserProducers
	l := server.New(sc, st, key, logf)

	// A chain problem at start is recorded (critical) and reported; the
	// ledger keeps accepting records so nothing new is lost.
	verifyData := map[string]any{"last_seq": st.Head().Seq, "version": version, "retention": cfg.RetentionText, "key_kind": "none"}
	if key != nil {
		verifyData["key_id"], verifyData["key_kind"], verifyData["key_alg"] = sign.ID(key.Public), key.Kind, key.Alg
	}
	if note != "" {
		verifyData["key_note"] = note
	}
	if sum, err := store.Verify(st.Path); err != nil {
		logf("CHAIN PROBLEM: %v", err)
		b, _ := json.Marshal(map[string]any{"error": err.Error(), "verified_up_to": sum.LastSeq})
		_, _ = l.Internal("verify", record.Record{Producer: record.Producer, Event: "ledger.chain_error", Outcome: "error", Data: b})
	}
	b, _ := json.Marshal(verifyData)
	if _, err := l.Internal("start", record.Record{Producer: record.Producer, Event: record.EventStart, Data: b}); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Socket), 0o755); err != nil {
		return err
	}
	_ = os.Remove(cfg.Socket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	// Who may connect is SELinux's decision; what each peer may do is the
	// ledger's (SO_PEERCRED, SO_PEERSEC).
	if err := os.Chmod(cfg.Socket, 0o666); err != nil {
		return err
	}
	go l.Serve(ln)

	stop := make(chan struct{})
	sink := func(collector string, r record.Record) error {
		_, err := l.Internal(collector, r)
		return err
	}
	if cfg.Journal {
		go collect.Journal(filepath.Join(cfg.Dir, "state", "journal.cursor"), sink, logf, stop)
	}
	if cfg.Snapshots != "" {
		go collect.Snapper(cfg.Snapshots, filepath.Join(cfg.Dir, "state", "snapper.last"), 30*time.Second, sink, logf, stop)
	}
	keyDesc := "none (" + note + ")"
	if key != nil {
		keyDesc = sign.ID(key.Public) + " (" + key.Kind + ")"
	}
	logf("basalt-ledgerd %s: %s, socket %s, chain head %d, export key %s, retention %s", version, st.Path, cfg.Socket, st.Head().Seq, keyDesc, cfg.RetentionText)

	// Retention: sealed files older than the policy are removed, each
	// after a retention record that keeps the chain verifiable.
	expire := func() {
		if cfg.Retention <= 0 {
			return
		}
		done, err := st.Expire(time.Now().Add(-cfg.Retention), cfg.RetentionText)
		for _, rd := range done {
			logf("retention: removed %s (records %d to %d)", rd.File, rd.FirstSeq, rd.LastSeq)
		}
		if err != nil {
			logf("retention: %v", err)
		}
	}
	expire()
	lastExpire := time.Now()

	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	fileStart := time.Now()
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	for {
		select {
		case <-tick.C:
			if time.Since(lastExpire) >= time.Hour {
				expire()
				lastExpire = time.Now()
			}
			if st.Size() >= cfg.RotateSize || (cfg.RotateAge > 0 && time.Since(fileStart) >= cfg.RotateAge) {
				res, err := st.Rotate()
				switch {
				case err != nil:
					logf("rotation: %v", err)
				case res.Rotated:
					logf("sealed %s at record %d", res.Sealed, res.Seal.Seq)
					fileStart = time.Now()
				default:
					fileStart = time.Now()
				}
			}
		case <-sig:
			close(stop)
			ln.Close()
			return nil
		}
	}
}

// exportKey picks the export signing key: the TPM-held key when a TPM is
// usable (export_key = auto or tpm), else the software key (auto or
// software). With export_key = tpm and no usable TPM there is no key and
// exports are refused. The current public key is written to
// keys/export.pub, and every key ever used to keys/export-<id>.pub, so
// exports made before a key change (a cleared TPM, a new machine board)
// can still be checked.
func exportKey(cfg config, logf func(string, ...any)) (*sign.Key, string, error) {
	keys := filepath.Join(cfg.Dir, "keys")
	if err := os.MkdirAll(keys, 0o755); err != nil {
		return nil, "", err
	}
	var key *sign.Key
	var note string
	if cfg.ExportKey != "software" {
		k, err := sign.TPMKey(cfg.TPMDevice)
		switch {
		case err == nil:
			key = k
		case cfg.ExportKey == "tpm":
			note = "export_key = tpm, but the TPM is not usable: " + err.Error()
			logf("no export key: %s", note)
		default:
			note = "no usable TPM (" + err.Error() + "), software key"
			logf("export key: %s", note)
		}
	} else {
		note = "software key chosen in ledger.conf"
	}
	if key == nil && cfg.ExportKey != "tpm" {
		k, err := sign.LoadOrCreate(filepath.Join(keys, "export-ed25519.key"))
		if err != nil {
			return nil, note, err
		}
		key = k
	}
	cur := filepath.Join(keys, "export.pub")
	if key == nil {
		_ = os.Remove(cur)
		return nil, note, nil
	}
	pemData, err := sign.PublicPEM(key.Public)
	if err != nil {
		return nil, note, err
	}
	for _, p := range []string{filepath.Join(keys, "export-"+sign.ID(key.Public)+".pub"), cur} {
		if b, err := os.ReadFile(p); err == nil && string(b) == string(pemData) {
			continue
		}
		tmp := p + ".tmp"
		if err := os.WriteFile(tmp, pemData, 0o644); err != nil {
			return nil, note, err
		}
		if err := os.Rename(tmp, p); err != nil {
			return nil, note, err
		}
	}
	return key, note, nil
}
