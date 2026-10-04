// Package cli is the basalt-ledger command line: read your audit trail in
// plain English, filter it, verify the chain, and make or check signed
// exports. Everything goes through the daemon's socket, which decides
// what each user may read.
package cli

import (
	"bufio"
	"crypto"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/client"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/server"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/sign"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/store"
)

// Version is set at build time.
var Version = "dev"

const usage = `basalt-ledger: the system audit trail (agents, network, SELinux, escalations, rollbacks, logins)

  basalt-ledger [show] [FILTERS] [--json] [--all]     records, newest last, in plain English
  basalt-ledger summary [FILTERS] [--json] [--all]    what happened, grouped by agent session
  basalt-ledger verify [--path FILE]                  check the hash chain across all files (--path: a copy, offline)
  basalt-ledger export [FILTERS] [-o FILE] [--all]    signed export for an incident report
  basalt-ledger verify-export FILE [--key PUBKEY]     check an export's signature and records
                                                      (PUBKEY: the host's /var/log/basalt-ledger/keys/export.pub)
  basalt-ledger status                                chain head, export key
  basalt-ledger rotate                                seal the current file (root)
  basalt-ledger api                                   raw JSON API: request lines on stdin
  basalt-ledger version

Filters: --agent NAME  --project DIR  --app NAME  --session ID  --producer NAME
         --event NAME (or a prefix ending in ".")  --severity info|notice|warning|critical
         --outcome ok|allowed|denied|error  --since WHEN  --until WHEN  -n N (newest N)
WHEN is "today", "yesterday", a duration ago (30m, 2h, 7d), a date (2026-10-04) or RFC 3339.
You see your own records; --all asks for administrator authentication (polkit) to see everything.
`

// Main runs the command line.
func Main(args []string) int {
	cmd := "show"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "show", "summary", "export":
		err = query(cmd, args)
	case "verify":
		err = verify(args)
	case "verify-export":
		err = verifyExport(args)
	case "status":
		err = status()
	case "rotate":
		err = rotate()
	case "api":
		err = api()
	case "version":
		fmt.Println(Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-ledger:", err)
		return 1
	}
	return 0
}

func socket() string {
	if s := os.Getenv("BASALT_LEDGER_SOCKET"); s != "" {
		return s
	}
	return client.DefaultSocket
}

func do(req server.Request) (server.Reply, error) {
	c, err := client.Dial(socket())
	if err != nil {
		return server.Reply{}, err
	}
	defer c.Close()
	rep, err := c.Do(req)
	if err != nil {
		return rep, err
	}
	if !rep.OK {
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}

// ParseWhen reads a time for --since/--until.
func ParseWhen(s string, now time.Time) (time.Time, error) {
	switch s {
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	case "yesterday":
		y, m, d := now.AddDate(0, 0, -1).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	case "now":
		return now, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return now.Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, now.Location()); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, now.Location()); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

type queryOpts struct {
	filter server.Filter
	json   bool
	all    bool
	out    string
}

func parseQuery(cmd string, args []string) (queryOpts, error) {
	var o queryOpts
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := &o.filter
	fs.StringVar(&f.Agent, "agent", "", "")
	fs.StringVar(&f.Project, "project", "", "")
	fs.StringVar(&f.App, "app", "", "")
	fs.StringVar(&f.Session, "session", "", "")
	fs.StringVar(&f.Producer, "producer", "", "")
	fs.StringVar(&f.Event, "event", "", "")
	fs.StringVar(&f.Severity, "severity", "", "")
	fs.StringVar(&f.Outcome, "outcome", "", "")
	fs.Int64Var(&f.AfterSeq, "after-seq", 0, "")
	since := fs.String("since", "", "")
	until := fs.String("until", "", "")
	fs.IntVar(&f.Limit, "n", 0, "")
	uid := fs.Int("uid", -1, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.all, "all", false, "")
	fs.StringVar(&o.out, "o", "", "")
	if err := fs.Parse(args); err != nil {
		return o, fmt.Errorf("%v (basalt-ledger help)", err)
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q (basalt-ledger help)", fs.Arg(0))
	}
	now := time.Now()
	if *since != "" {
		t, err := ParseWhen(*since, now)
		if err != nil {
			return o, fmt.Errorf("--since %q: not a time", *since)
		}
		f.Since = t.UTC().Format(time.RFC3339Nano)
	}
	if *until != "" {
		t, err := ParseWhen(*until, now)
		if err != nil {
			return o, fmt.Errorf("--until %q: not a time", *until)
		}
		f.Until = t.UTC().Format(time.RFC3339Nano)
	}
	if f.Project != "" {
		if abs, err := filepath.Abs(f.Project); err == nil {
			f.Project = abs
		}
	}
	if *uid >= 0 {
		f.UID = uid
	}
	return o, nil
}

// elevate re-runs the command through pkexec (polkit action
// org.basalt-os.ledger.read-all) without --all.
func elevate(args []string) error {
	var rest []string
	for _, a := range args {
		if a != "--all" && a != "-all" {
			rest = append(rest, a)
		}
	}
	self, err := os.Executable()
	if err != nil {
		self = "/usr/bin/basalt-ledger"
	}
	cmd := exec.Command("pkexec", append([]string{self}, rest...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}

func query(cmd string, args []string) error {
	o, err := parseQuery(cmd, args)
	if err != nil {
		return err
	}
	if o.all && os.Geteuid() != 0 {
		return elevate(append([]string{cmd}, args...))
	}
	switch cmd {
	case "summary":
		rep, err := do(server.Request{Op: "summary", Filter: o.filter})
		if err != nil {
			return err
		}
		if o.json {
			return printJSON(rep.Summary)
		}
		for _, l := range rep.Summary.Lines {
			fmt.Println(l)
		}
		var parts []string
		for _, s := range record.Severities {
			if n := rep.Summary.Counts[s]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, s))
			}
		}
		if len(parts) > 0 {
			fmt.Printf("Records: %s.\n", strings.Join(parts, ", "))
		}
		return nil
	case "export":
		rep, err := do(server.Request{Op: "export", Filter: o.filter})
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(rep.Export, "", "  ")
		if o.out == "" || o.out == "-" {
			_, err = os.Stdout.Write(append(b, '\n'))
			return err
		}
		if err := os.WriteFile(o.out, append(b, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "exported %d records to %s, signed with key %s (%s); check it with: basalt-ledger verify-export %s\n",
			len(rep.Export.Records), o.out, rep.Export.KeyID, rep.Export.KeyKind, o.out)
		return nil
	}
	rep, err := do(server.Request{Op: "query", Filter: o.filter})
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(rep.Records)
	}
	if len(rep.Records) == 0 {
		fmt.Println("No records match.")
		return nil
	}
	for _, v := range rep.Records {
		t := v.When().Local().Format("2006-01-02 15:04:05")
		fmt.Printf("%s  %-8s  #%-6d %s\n", t, v.Severity, v.Seq, v.Text)
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func verify(args []string) error {
	var v *store.Summary
	if len(args) == 2 && args[0] == "--path" {
		// Offline: a copied ledger (incident response), or the files
		// themselves as root. No daemon involved.
		s, err := store.Verify(args[1])
		if err != nil {
			return fmt.Errorf("chain broken: %w", err)
		}
		if s.Records == 0 {
			return fmt.Errorf("no ledger at %s", args[1])
		}
		if s.Truncated {
			return fmt.Errorf("chain starts at record %d: the oldest ledger files were removed without a retention record", s.FirstSeq)
		}
		v = &s
	} else {
		rep, err := do(server.Request{Op: "verify"})
		if len(args) == 1 && args[0] == "--json" && rep.Verify != nil {
			_ = printJSON(rep.Verify)
		}
		if err != nil {
			return err
		}
		v = rep.Verify
	}
	fmt.Printf("ledger chain intact: records %d to %d (%d records, %d files, %d seals)\n", v.FirstSeq, v.LastSeq, v.Records, len(v.Files), v.Seals)
	if v.ExpiredUpTo > 0 {
		fmt.Printf("note: records up to %d were removed by the retention policy; retention records in the chain vouch for them\n", v.ExpiredUpTo)
	}
	if v.Truncated {
		fmt.Println("note: the oldest files were removed; the chain is verified from its first remaining file")
	}
	return nil
}

func verifyExport(args []string) error {
	fs := flag.NewFlagSet("verify-export", flag.ContinueOnError)
	keyFile := fs.String("key", "", "")
	var files []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(files) != 1 {
		return errors.New("usage: basalt-ledger verify-export FILE [--key PUBKEY]")
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	var e sign.Export
	if err := json.Unmarshal(b, &e); err != nil {
		return fmt.Errorf("%s: not an export: %w", files[0], err)
	}
	var trusted crypto.PublicKey
	if *keyFile != "" {
		if trusted, err = sign.ReadPublic(*keyFile); err != nil {
			return err
		}
	}
	origin, err := sign.Verify(e, trusted)
	if err != nil {
		return fmt.Errorf("export NOT valid: %w", err)
	}
	fmt.Printf("export valid: %d records (%d to %d) from %s, generated %s; %s\n", len(e.Records), e.Chain.FirstSeq, e.Chain.LastSeq,
		e.Host, e.Generated, origin)
	if !e.Chain.Verified {
		fmt.Printf("warning: the ledger chain did not verify when this was exported: %s\n", e.Chain.Error)
	}
	switch e.KeyKind {
	case sign.KindTPM:
		fmt.Println("key: held by the host's TPM (ECDSA P-256); it cannot be copied off that machine")
	case sign.KindSoftware, sign.KindDevelopment:
		fmt.Println("note: signed with a software key kept on the host's disk (no TPM); whoever is root there can sign with it")
	}
	return nil
}

func status() error {
	rep, err := do(server.Request{Op: "status"})
	if err != nil {
		return err
	}
	s := rep.Status
	who := "your own records"
	if s.SeesAll {
		who = "all records (root)"
	}
	kind := s.KeyKind
	if s.KeyNote != "" {
		kind += ", " + s.KeyNote
	}
	fmt.Printf("chain head: record %d, %s\nfile: %s (%d bytes)\nexport key: %s (%s), public key %s\nretention: %s\nyou can read: %s\n",
		s.HeadSeq, s.HeadHash, s.Path, s.Size, s.KeyID, kind, s.PublicKey, s.Retention, who)
	return nil
}

func rotate() error {
	rep, err := do(server.Request{Op: "rotate"})
	if err != nil {
		return err
	}
	if !rep.Rotate.Rotated {
		fmt.Println("not rotated:", rep.Rotate.Reason)
		return nil
	}
	fmt.Printf("sealed %s at record %d; continues at record %d\n", rep.Rotate.Sealed, rep.Rotate.Seal.Seq, rep.Rotate.Continue.Seq)
	return nil
}

// api passes raw request lines from stdin and prints the raw replies:
// the same JSON API the desktop shell's timeline uses.
func api() error {
	c, err := client.Dial(socket())
	if err != nil {
		return err
	}
	defer c.Close()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		out, err := c.Raw([]byte(line))
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
	}
	return sc.Err()
}
