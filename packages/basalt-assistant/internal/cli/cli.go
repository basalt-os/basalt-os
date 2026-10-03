// Package cli implements the `basalt` command.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/apply"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

const usage = `basalt: the Basalt OS system assistant (diagnosis without a language model)

Read-only (no confirmation needed):
  basalt status                       health summary
  basalt why UNIT                     diagnose a service (state, journal, SELinux, dependencies, ports, disk, config)
  basalt fix selinux [--since 1h]     analyze recent SELinux denials and map them to known fixes
  basalt snapshots [list]             snapshots of the root
  basalt snapshots diff A [B]         packages and files that differ (B: 0 or omitted = now)
  basalt disk                         btrfs usage, space held by snapshots, fullness forecast
  basalt pending [--all]              proposals waiting for a decision
  basalt show ID                      one proposal in full
  basalt audit [N] | audit verify     audit log (hash chain, verified across rotated files)

Changes (root; exact commands shown, then confirmation):
  basalt apply ID [--yes --confirm CODE]
  basalt ignore ID [--reason TEXT]
  basalt snapshots rollback N | --before ID
  basalt audit rotate [--force]       seal the audit log and continue in a new file
                                      (when larger than [audit] rotate_size; run daily by a timer)
  basalt why UNIT --apply, basalt fix selinux --apply, basalt disk --apply
                                      store the proposal and go straight to the confirmation

Natural language (optional, needs the local model service basalt-llm and
[translator] enabled = yes in /etc/basalt/assistant.conf):
  basalt ask "por que o nginx caiu?"  translate into one of the commands above and run it if it
                                      only reads; a change is printed, never run (--dry-run: only print)

Options: --json (machine-readable output), --config FILE.
Diagnoses that find a change store it as a pending proposal when run as root.
`

// opts are the parsed flags.
type opts struct {
	json, apply, yes, all bool
	dryRun, force         bool
	since                 time.Duration
	confirm, reason       string
	before, config        string
	args                  []string
}

func parse(argv []string) (opts, error) {
	o := opts{since: time.Hour, config: config.DefaultPath}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		val := func() (string, error) {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
				return v, nil
			}
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return argv[i], nil
		}
		name, _, _ := strings.Cut(a, "=")
		var err error
		var v string
		switch name {
		case "--json":
			o.json = true
		case "--apply":
			o.apply = true
		case "--yes", "-y":
			o.yes = true
		case "--all":
			o.all = true
		case "--dry-run":
			o.dryRun = true
		case "--force":
			o.force = true
		case "--since":
			if v, err = val(); err == nil {
				o.since, err = time.ParseDuration(v)
			}
		case "--confirm":
			o.confirm, err = val()
		case "--reason":
			o.reason, err = val()
		case "--before":
			o.before, err = val()
		case "--config":
			o.config, err = val()
		case "-h", "--help":
			o.args = append([]string{"help"}, o.args...)
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return o, fmt.Errorf("unknown option %s", a)
			}
			o.args = append(o.args, a)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

// app is the wired-up CLI.
type app struct {
	o     opts
	cfg   config.Config
	env   *diag.Env
	store proposal.Store
	audit *audit.Log
	layer *decide.Layer
	out   io.Writer
	root  bool
}

// Main runs the CLI and returns the exit code.
func Main(argv []string, version string) int {
	o, err := parse(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt:", err)
		return 2
	}
	if len(o.args) == 0 || o.args[0] == "help" {
		fmt.Print(usage)
		return 0
	}
	if o.args[0] == "version" {
		fmt.Println("basalt", version)
		return 0
	}
	cfg, err := config.Load(o.config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt:", err)
		return 2
	}
	a := &app{o: o, cfg: cfg, out: os.Stdout, root: os.Geteuid() == 0}
	a.store = proposal.Store{Dir: cfg.StateDir + "/proposals"}
	a.audit = audit.New(cfg.AuditPath, "basalt")
	var logger decide.Logger
	if a.root {
		logger = a.audit
	}
	a.layer = decide.FromConfigFull(cfg.DecideConfig(), logger, cfg.Thresholds, cfg.DefaultThreshold)
	a.env = diag.Real(false, a.layer)
	a.env.HistoryPath = cfg.StateDir + "/disk-history.jsonl"

	ctx := context.Background()
	err = a.dispatch(ctx)
	if err != nil {
		if errors.Is(err, apply.ErrCancelled) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintln(os.Stderr, "basalt:", err)
		return 1
	}
	return 0
}

// dispatch runs the command in a.o.args.
func (a *app) dispatch(ctx context.Context) error {
	switch a.o.args[0] {
	case "status":
		return a.status(ctx)
	case "why":
		return a.why(ctx)
	case "fix":
		return a.fix(ctx)
	case "snapshots", "snapshot":
		return a.snapshots(ctx)
	case "disk":
		return a.disk(ctx)
	case "pending":
		return a.pending()
	case "show":
		return a.show()
	case "apply":
		return a.applyCmd(ctx)
	case "ignore":
		return a.ignore()
	case "audit":
		return a.auditCmd()
	case "ask":
		return a.ask(ctx)
	}
	return fmt.Errorf("unknown command %q (basalt help)", a.o.args[0])
}

func (a *app) printJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) applier() *apply.Applier {
	st, _ := os.Stdin.Stat()
	interactive := st != nil && st.Mode()&os.ModeCharDevice != 0
	return &apply.Applier{Store: a.store, Audit: a.audit, Exec: runner.Exec{}, In: os.Stdin, Out: a.out,
		Interactive: interactive, Snapshots: true, Settle: 2 * time.Second}
}

// keep stores a proposal (root only), reusing an open one with the same
// key so repeated diagnoses do not pile up. Decisions are logged here.
func (a *app) keep(p *proposal.Proposal) *proposal.Proposal {
	if !a.root || len(p.Actions) == 0 {
		return p
	}
	if old := a.store.FindOpen(p.Key); old != nil {
		p.ID, p.Created, p.Seen = old.ID, old.Created, old.Seen+1
	}
	for i := range p.Decisions {
		p.Decisions[i] = a.layer.Record(p.Decisions[i], "propose "+p.ID+" (cli)")
	}
	if err := a.store.Save(p); err != nil {
		fmt.Fprintln(os.Stderr, "basalt: cannot store the proposal:", err)
		return p
	}
	cmds, _ := p.Commands()
	_, _ = a.audit.Append("proposal", p.ID+" from cli: "+p.Title, map[string]any{"proposal": p.ID, "actions": p.Actions, "commands": cmds})
	return p
}

// present prints a proposal and, with --apply, runs the confirmation.
func (a *app) present(ctx context.Context, p *proposal.Proposal) error {
	if len(p.Actions) > 0 && !a.root {
		p.ID = "preview"
		fmt.Fprint(a.out, report.Render(p))
		fmt.Fprintln(a.out, "\n(run as root to store this proposal and apply it)")
		return nil
	}
	if a.o.apply && len(p.Actions) > 0 {
		if a.o.yes {
			return errors.New("--apply asks interactively; for a non-interactive apply use `basalt apply ID --yes --confirm CODE` with the id and code printed by this command without --apply")
		}
		return a.applier().Apply(ctx, p, apply.Options{})
	}
	if !a.root || len(p.Actions) == 0 {
		// Not stored: render with a neutral id.
		if len(p.Actions) == 0 {
			p.ID = "report"
		}
	}
	fmt.Fprint(a.out, report.Render(p))
	return nil
}

func (a *app) status(ctx context.Context) error {
	ps, _ := a.store.List(proposal.Pending)
	s := a.env.GetStatus(ctx, len(ps))
	if a.o.json {
		return a.printJSON(s)
	}
	w := a.out
	fmt.Fprintf(w, "System:      %s\n", s.OS)
	fmt.Fprintf(w, "SELinux:     %s\n", s.SELinux)
	fmt.Fprintf(w, "Failed:      %s\n", orNone(strings.Join(s.FailedUnits, " ")))
	fmt.Fprintf(w, "Denials 24h: %d\n", s.Denials24h)
	fmt.Fprintf(w, "Disk /:      %.1f %% used, %s available\n", s.DiskPct, diag.HumanBytes(int64(s.Disk.Free)))
	fmt.Fprintf(w, "Snapshots:   %d, newest %s\n", s.Snapshots, orNone(s.LastSnapshot))
	fmt.Fprintf(w, "Rollback:    %s\n", s.RollbackState)
	fmt.Fprintf(w, "Assistant:   basalt-assistantd %s, %d pending proposal(s)\n", s.Daemon, s.Pending)
	if n, err := audit.Verify(a.cfg.AuditPath); err == nil {
		fmt.Fprintf(w, "Audit log:   %d records, chain verifies\n", n)
	} else if !os.IsNotExist(err) && !os.IsPermission(err) {
		fmt.Fprintf(w, "Audit log:   CHAIN BROKEN: %v\n", err)
	}
	if len(s.Problems) == 0 {
		fmt.Fprintln(w, "\nNo problems found.")
		return nil
	}
	fmt.Fprintln(w, "\nProblems:")
	for _, p := range s.Problems {
		fmt.Fprintf(w, "  - %s\n", p)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (a *app) why(ctx context.Context) error {
	if len(a.o.args) < 2 {
		return errors.New("usage: basalt why UNIT")
	}
	rep, err := diag.WhyUnit(ctx, a.env, a.o.args[1])
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(rep)
	}
	p := a.keep(report.FromUnit("cli", rep))
	return a.present(ctx, p)
}

func (a *app) fix(ctx context.Context) error {
	if len(a.o.args) < 2 || a.o.args[1] != "selinux" {
		return errors.New("usage: basalt fix selinux [--since 1h] [--apply]")
	}
	items := a.env.FixSELinux(ctx, time.Now().Add(-a.o.since))
	if a.o.json {
		return a.printJSON(items)
	}
	if len(items) == 0 {
		fmt.Fprintf(a.out, "No SELinux denials in the last %s.\n", a.o.since)
		return nil
	}
	fmt.Fprintf(a.out, "%d group(s) of SELinux denials in the last %s.\n\n", len(items), a.o.since)
	for _, it := range items {
		p := a.keep(report.FromSELinux("cli", it))
		if err := a.present(ctx, p); err != nil {
			return err
		}
		fmt.Fprintln(a.out)
	}
	return nil
}

func (a *app) disk(ctx context.Context) error {
	rep, err := a.env.Disk(ctx, a.cfg.Disk, 50)
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(rep)
	}
	w := a.out
	fmt.Fprintf(w, "/ %s used of %s (%.1f %%), %s available\n", diag.HumanBytes(int64(rep.FS.Total-rep.FS.Free)),
		diag.HumanBytes(int64(rep.FS.Total)), rep.UsedPct, diag.HumanBytes(int64(rep.FS.Free)))
	if b := rep.Btrfs; len(b) > 0 {
		fmt.Fprintf(w, "btrfs: device %s, allocated %s, used %s, free (estimated) %s, data ratio %.2f\n",
			diag.HumanBytes(b["device_size"]), diag.HumanBytes(b["device_allocated"]), diag.HumanBytes(b["used"]),
			diag.HumanBytes(b["free_estimated"]), float64(b["data_ratio"])/100)
	}
	fmt.Fprintf(w, "journal %s, package cache %s\n", diag.HumanBytes(rep.Journal), diag.HumanBytes(rep.PkgCache))
	fmt.Fprintf(w, "snapshots: %d measured, %s held exclusively\n", len(rep.Snapshots), diag.HumanBytes(rep.SnapshotTotal))
	for i, s := range rep.Snapshots {
		if i >= 8 {
			break
		}
		fmt.Fprintf(w, "  %4d %-6s %s  exclusive %-10s %s\n", s.Snapshot.Number, s.Snapshot.Type, s.Snapshot.Date, diag.HumanBytes(s.Exclusive), s.Snapshot.Description)
	}
	fmt.Fprintf(w, "forecast: %s (%d samples over %.1f h)\n\n", rep.Forecast.Note, rep.Forecast.Samples, rep.Forecast.SpanHours)
	p := report.FromDisk("cli", rep)
	if len(p.Actions) == 0 {
		fmt.Fprintln(w, rep.Explanation)
		return nil
	}
	return a.present(ctx, a.keep(p))
}

func (a *app) snapshots(ctx context.Context) error {
	sub := "list"
	if len(a.o.args) > 1 {
		sub = a.o.args[1]
	}
	switch sub {
	case "list":
		snaps := a.env.Snapshots(ctx)
		if a.o.json {
			return a.printJSON(snaps)
		}
		fmt.Fprintf(a.out, "%5s %-6s %5s %-19s %s\n", "#", "type", "pre", "date (UTC)", "description")
		for _, s := range snaps {
			pre := ""
			if s.Pre > 0 {
				pre = strconv.Itoa(s.Pre)
			}
			extra := ""
			if s.Userdata["basalt"] == "apply" {
				extra = "  [" + s.Userdata["proposal"] + "]"
			}
			fmt.Fprintf(a.out, "%5d %-6s %5s %-19s %s%s\n", s.Number, s.Type, pre, s.Date, s.Description, extra)
		}
		if o := diag.OrphanPre(snaps, time.Now(), 10*time.Minute); len(o) > 0 {
			fmt.Fprintf(a.out, "\nUnfinished transactions (pre without post): ")
			for _, s := range o {
				fmt.Fprintf(a.out, "%d ", s.Number)
			}
			fmt.Fprintln(a.out)
		}
		return nil
	case "diff":
		if len(a.o.args) < 3 {
			return errors.New("usage: basalt snapshots diff A [B]")
		}
		from, err := strconv.Atoi(a.o.args[2])
		if err != nil {
			return err
		}
		to := 0
		if len(a.o.args) > 3 {
			if to, err = strconv.Atoi(a.o.args[3]); err != nil {
				return err
			}
		}
		pd := a.env.DiffPackages(ctx, from, to)
		fd := a.env.DiffFiles(ctx, from, to)
		if a.o.json {
			return a.printJSON(map[string]any{"packages": pd, "files": fd})
		}
		name := func(n int) string {
			if n == 0 {
				return "now"
			}
			return "snapshot " + strconv.Itoa(n)
		}
		fmt.Fprintf(a.out, "From %s to %s\n", name(from), name(to))
		if pd.Error != "" {
			fmt.Fprintf(a.out, "packages: %s\n", pd.Error)
		} else {
			fmt.Fprintf(a.out, "packages: %d -> %d; %d added, %d removed, %d changed\n", pd.FromSize, pd.ToSize, len(pd.Added), len(pd.Removed), len(pd.Changed))
			for _, l := range pd.Added {
				fmt.Fprintf(a.out, "  + %s\n", l)
			}
			for _, l := range pd.Removed {
				fmt.Fprintf(a.out, "  - %s\n", l)
			}
			for _, l := range pd.Changed {
				fmt.Fprintf(a.out, "  ~ %s\n", l)
			}
		}
		if fd.Skipped != "" {
			fmt.Fprintf(a.out, "files: %s\n", fd.Skipped)
		} else {
			fmt.Fprintf(a.out, "files: %d changed\n", fd.Total)
			for top, n := range fd.ByTop {
				fmt.Fprintf(a.out, "  %-24s %d\n", top, n)
			}
			for _, l := range fd.Etc {
				fmt.Fprintf(a.out, "  %s\n", l)
			}
		}
		return nil
	case "rollback":
		n := 0
		why := "Requested from the command line."
		if a.o.before != "" {
			pre, _ := a.env.FindApplySnapshots(ctx, a.o.before)
			if pre == 0 {
				return fmt.Errorf("no pre snapshot recorded for %s", a.o.before)
			}
			n = pre
			why = fmt.Sprintf("Undo %s: return the root to snapshot %d, taken just before it was applied.", a.o.before, pre)
		} else if len(a.o.args) > 2 {
			var err error
			if n, err = strconv.Atoi(a.o.args[2]); err != nil {
				return err
			}
		} else {
			return errors.New("usage: basalt snapshots rollback N | --before PROPOSAL")
		}
		plan, err := a.env.PlanRollback(ctx, n)
		if err != nil {
			return err
		}
		if a.o.json {
			return a.printJSON(plan)
		}
		p := a.keep(report.FromRollback("cli", plan, why))
		if !a.root {
			return a.present(ctx, p)
		}
		if a.o.yes && a.o.confirm == "" {
			// Non-interactive: store it and print the id and the code to
			// confirm with (`basalt apply ID --yes --confirm CODE`).
			fmt.Fprint(a.out, report.Render(p))
			return nil
		}
		if a.o.yes {
			return a.applier().Apply(ctx, p, apply.Options{Yes: true, Confirm: a.o.confirm})
		}
		return a.applier().Apply(ctx, p, apply.Options{})
	}
	return fmt.Errorf("unknown: basalt snapshots %s", sub)
}

func (a *app) pending() error {
	st := proposal.Pending
	if a.o.all {
		st = ""
	}
	ps, err := a.store.List(st)
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(ps)
	}
	if len(ps) == 0 {
		fmt.Fprintln(a.out, "No proposals.")
		return nil
	}
	for _, p := range ps {
		fmt.Fprintln(a.out, report.Line(p))
	}
	fmt.Fprintln(a.out, "\nbasalt show ID for the details; sudo basalt apply ID or sudo basalt ignore ID.")
	return nil
}

func (a *app) load() (*proposal.Proposal, error) {
	if len(a.o.args) < 2 {
		return nil, fmt.Errorf("usage: basalt %s ID", a.o.args[0])
	}
	return a.store.Load(a.o.args[1])
}

func (a *app) show() error {
	p, err := a.load()
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(p)
	}
	fmt.Fprint(a.out, report.Render(p))
	if r := p.Result; r != nil {
		fmt.Fprintf(a.out, "\nResult (%s): ok=%v, snapshots %d/%d, audit #%d\n", r.Time.Format(time.RFC3339), r.OK, r.PreSnapshot, r.PostSnapshot, r.AuditSeq)
		for _, s := range append(r.Steps, r.Checks...) {
			fmt.Fprintf(a.out, "  [%v] %s %s\n", s.OK, s.What, strings.ReplaceAll(s.Output, "\n", " | "))
		}
	}
	return nil
}

func (a *app) applyCmd(ctx context.Context) error {
	p, err := a.load()
	if err != nil {
		return err
	}
	return a.applier().Apply(ctx, p, apply.Options{Yes: a.o.yes, Confirm: a.o.confirm})
}

func (a *app) ignore() error {
	if !a.root {
		return errors.New("ignoring a proposal needs root (it is recorded in the audit log)")
	}
	p, err := a.load()
	if err != nil {
		return err
	}
	why := a.o.reason
	if why == "" {
		why = "ignored by the administrator"
	}
	if err := a.applier().Ignore(p, why); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s ignored.\n", p.ID)
	return nil
}

func (a *app) auditCmd() error {
	if len(a.o.args) > 1 && a.o.args[1] == "verify" {
		sum, err := audit.VerifyChain(a.cfg.AuditPath)
		if a.o.json {
			out := map[string]any{"summary": sum, "ok": err == nil}
			if err != nil {
				out["error"] = err.Error()
			}
			if perr := a.printJSON(out); perr != nil {
				return perr
			}
			if err != nil {
				return fmt.Errorf("audit chain broken after record %d: %w", sum.LastSeq, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("audit chain broken after record %d: %w", sum.LastSeq, err)
		}
		fmt.Fprintf(a.out, "audit chain verifies: records %d to %d in %d file(s), %d seal(s), %s\n",
			sum.FirstSeq, sum.LastSeq, len(sum.Files), sum.Seals, a.cfg.AuditPath)
		if sum.Truncated {
			fmt.Fprintf(a.out, "note: the oldest file left continues from a removed one; records before %d cannot be checked\n", sum.FirstSeq)
		}
		if sum.Unsealed {
			fmt.Fprintln(a.out, "note: the current file ends with a seal: a rotation did not finish (basalt audit rotate)")
		}
		return nil
	}
	if len(a.o.args) > 1 && a.o.args[1] == "rotate" {
		return a.auditRotate()
	}
	n := 20
	if len(a.o.args) > 1 {
		if v, err := strconv.Atoi(a.o.args[1]); err == nil {
			n = v
		}
	}
	rs, err := audit.Tail(a.cfg.AuditPath, n)
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(rs)
	}
	for _, r := range rs {
		who := r.Actor.Program
		if r.Actor.SudoUser != "" {
			who += " (" + r.Actor.SudoUser + ")"
		}
		fmt.Fprintf(a.out, "#%-4d %s %-22s %-9s %s\n", r.Seq, r.Time.Local().Format("2006-01-02 15:04:05"), who, r.Type, r.Text)
	}
	return nil
}

// auditRotate seals the audit log and starts the next file (root only:
// the files are append-only and immutable, see audit.Rotate).
func (a *app) auditRotate() error {
	if !a.root {
		return errors.New("rotating the audit log needs root")
	}
	minSize := a.cfg.AuditRotateSize
	if a.o.force {
		minSize = 0
	}
	res, err := a.audit.Rotate(audit.RotateOptions{MinSize: minSize, Attrs: true})
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(res)
	}
	if !res.Rotated {
		fmt.Fprintf(a.out, "audit log not rotated: %s\n", res.Reason)
		return nil
	}
	fmt.Fprintf(a.out, "audit log sealed at record %d and kept as %s; continues at record %d in %s\n",
		res.Seal.Seq, res.Sealed, res.Continue.Seq, a.cfg.AuditPath)
	return nil
}
