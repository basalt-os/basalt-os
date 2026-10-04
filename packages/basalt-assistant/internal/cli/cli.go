// Package cli implements the `basalt` command.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/apply"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

const usage = `basalt: the Basalt OS system assistant

It looks at the system, explains what is wrong in plain words and proposes
a fix. It never changes anything until you apply a proposal yourself.

Look (no changes, no confirmation):
  basalt status                       a health summary
  basalt why UNIT                     why a service is not running (log, SELinux, ports, disk, config)
  basalt fix selinux [--since 1h]     recent SELinux denials and the known fix for each
  basalt snapshots [list]             snapshots of the system
  basalt snapshots diff A [B]         packages and files that differ (B: 0 or left out = now)
  basalt disk                         what uses the disk, and when it will be full
  basalt pending [--all]              proposals waiting for your decision
  basalt show ID                      one proposal in full
  basalt audit [N] | audit verify     the audit log (tamper-evident, checked across rotated files)

Change (as root; you see the exact commands first and confirm them):
  basalt apply ID [--yes --confirm CODE]
  basalt ignore ID [--reason TEXT]
  basalt confirm ID                   check, as root, a hint of the background service (a file
                                      restore, a rollback) and turn it into a proposal
  basalt snapshots rollback N | --before ID
  basalt audit rotate [--force]       seal the audit log and continue in a new file
  basalt why UNIT --apply, basalt fix selinux --apply, basalt disk --apply
                                      store the proposal and go straight to the confirmation

Ask in your own words (optional: needs the local model service basalt-llm
and [translator] enabled = yes in /etc/basalt/assistant.conf):
  basalt ask "why did nginx stop?"    read-only requests run; a change is printed, never run

Tell the Basalt OS project what broke or what you wish it did (opt-in; you
see exactly what will be sent and confirm it first):
  basalt feedback ["MESSAGE"] [--kind bug|idea|other] [--email ADDRESS]
                  [--include os,packages,hardware,findings|all|none] [--preview]

Other Basalt tools: basalt NAME ... runs basalt-NAME from /usr/libexec/basalt
or /usr/bin (never from PATH), e.g. basalt ledger summary --since today.

Options: --json (machine-readable), --verbose (all evidence and decisions),
--plain (template text even when [humanize] is on), --config FILE.
Run as root, a diagnosis that finds a fix stores it as a pending proposal.
`

// opts are the parsed flags.
type opts struct {
	json, apply, yes, all bool
	dryRun, force         bool
	verbose, plain        bool
	since                 time.Duration
	confirm, reason       string
	before, config        string
	args                  []string

	// basalt feedback
	kind, email, source string
	include             string
	includeSet, preview bool
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
		case "--verbose", "-v":
			o.verbose = true
		case "--plain":
			o.plain = true
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
		case "--kind":
			o.kind, err = val()
		case "--email":
			o.email, err = val()
		case "--include":
			o.include, err = val()
			o.includeSet = true
		case "--preview":
			o.preview = true
		case "--source":
			o.source, err = val()
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
	tty   bool // stdout is a terminal

	version string
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
	a := &app{o: o, cfg: cfg, out: os.Stdout, root: os.Geteuid() == 0, version: version}
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		a.tty = true
	}
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
		if errors.Is(err, errNotSent) {
			return 1 // the reason was already shown
		}
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
	case "confirm":
		return a.confirm(ctx)
	case "audit":
		return a.auditCmd()
	case "ask":
		return a.ask(ctx)
	case "feedback":
		return a.feedback(ctx)
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
		a.render(ctx, p)
		fmt.Fprintln(a.out, "\nThis is a preview. Run the same command with sudo to store the proposal and apply it.")
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
	a.render(ctx, p)
	return nil
}

func (a *app) status(ctx context.Context) error {
	ps, _ := a.store.List(proposal.Pending)
	s := a.env.GetStatus(ctx, len(ps))
	if a.o.json {
		return a.printJSON(s)
	}
	n, err := audit.Verify(a.cfg.AuditPath)
	if err != nil && (os.IsNotExist(err) || os.IsPermission(err)) {
		n, err = -1, nil
	}
	writeStatus(a.out, s, n, err)
	return nil
}

// writeStatus prints the health summary; auditN < 0: the audit log was
// not readable here.
func writeStatus(w io.Writer, s diag.Status, auditN int64, auditErr error) {
	if len(s.Problems) == 0 {
		fmt.Fprintln(w, "Everything looks fine.")
	} else {
		fmt.Fprintf(w, "%s your attention:\n", plural(len(s.Problems), "thing needs", "things need"))
		for _, p := range s.Problems {
			fmt.Fprintln(w, "  - "+p)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  System      %s\n", s.OS)
	selinux := s.SELinux
	if s.SELinux == "Enforcing" {
		selinux += " (protecting the system)"
	}
	fmt.Fprintf(w, "  SELinux     %s\n", orNone(selinux))
	if len(s.FailedUnits) == 0 {
		fmt.Fprintln(w, "  Services    none failed")
	} else {
		fmt.Fprintf(w, "  Services    failed: %s\n", strings.Join(s.FailedUnits, " "))
	}
	if s.Denials24h == 0 {
		fmt.Fprintln(w, "  Denials     none in the last 24 hours")
	} else {
		fmt.Fprintf(w, "  Denials     %d in the last 24 hours\n", s.Denials24h)
	}
	fmt.Fprintf(w, "  Disk /      %.1f %% used, %s free\n", s.DiskPct, diag.HumanBytes(int64(s.Disk.Free)))
	if s.Snapshots == 0 {
		fmt.Fprintln(w, "  Snapshots   none")
	} else {
		fmt.Fprintf(w, "  Snapshots   %d, newest %s\n", s.Snapshots, s.LastSnapshot)
	}
	rb := s.RollbackState
	if rb == "none" {
		rb = "none waiting"
	}
	fmt.Fprintf(w, "  Rollback    %s\n", rb)
	fmt.Fprintf(w, "  Assistant   background service %s, %s\n", orNone(s.Daemon), plural(s.Pending, "proposal waiting", "proposals waiting"))
	switch {
	case auditErr != nil:
		fmt.Fprintf(w, "  Audit log   CHAIN BROKEN: %v (basalt audit verify)\n", auditErr)
	case auditN >= 0:
		fmt.Fprintf(w, "  Audit log   %d records, chain intact\n", auditN)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
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
		if err := a.printJSON(rep); err != nil {
			return err
		}
		return incomplete(rep.Errors)
	}
	p := a.keep(report.FromUnit("cli", rep))
	if err := a.present(ctx, p); err != nil {
		return err
	}
	return incomplete(rep.Errors)
}

// incomplete turns probe errors into the command's error: a diagnosis that
// could not query the SELinux policy is not a clean answer.
func incomplete(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("the diagnosis is incomplete: %s", strings.Join(errs, "; "))
}

func (a *app) fix(ctx context.Context) error {
	if len(a.o.args) < 2 || a.o.args[1] != "selinux" {
		return errors.New("usage: basalt fix selinux [--since 1h] [--apply]")
	}
	items := a.env.FixSELinux(ctx, time.Now().Add(-a.o.since))
	var errs []string
	for _, it := range items {
		errs = append(errs, it.Fix.Errors...)
	}
	if a.o.json {
		if err := a.printJSON(items); err != nil {
			return err
		}
		return incomplete(errs)
	}
	if len(items) == 0 {
		fmt.Fprintf(a.out, "SELinux blocked nothing in the last %s. Nothing to do.\n", human(a.o.since))
		return nil
	}
	fmt.Fprintf(a.out, "SELinux blocked %s in the last %s. Each one is explained below.\n\n",
		plural(len(items), "kind of access", "kinds of access"), human(a.o.since))
	for _, it := range items {
		p := a.keep(report.FromSELinux("cli", it))
		if err := a.present(ctx, p); err != nil {
			return err
		}
		fmt.Fprintln(a.out)
	}
	return incomplete(errs)
}

// confirm checks a hint of the confined view as root (the snapshot exists,
// a restored copy passes the service's config checker, what a rollback
// changes) and stores it as a new proposal from the command line; the hint
// is closed as resolved. Proposals of older versions whose confined source
// carried a restore or a rollback as actions are confirmed the same way.
func (a *app) confirm(ctx context.Context) error {
	if len(a.o.args) < 2 {
		return errors.New("usage: basalt confirm ID")
	}
	if !a.root {
		return errors.New("confirming a hint needs the root view: run it with sudo")
	}
	p, err := a.store.Load(a.o.args[1])
	if err != nil {
		return err
	}
	if p.Status != proposal.Pending {
		return fmt.Errorf("proposal %s is %s", p.ID, p.Status)
	}
	hints := p.Hints
	if len(hints) == 0 && p.FromConfinedView() && action.AnyNeedsRootView(p.Actions) {
		hints = []action.Hint{{Actions: p.Actions, Reason: "proposed by " + p.Source + " before hints existed"}}
	}
	if len(hints) == 0 {
		return fmt.Errorf("proposal %s has no hint to confirm", p.ID)
	}
	unit := ""
	if p.Kind == "unit" {
		unit = p.Subject
	}
	var acts []action.Action
	var evidence []string
	review := false
	for _, h := range hints {
		ev, rv, err := a.env.ConfirmHint(ctx, unit, h.Actions)
		if err != nil {
			_, _ = a.audit.Append("refuse", "hint "+p.ID+" not confirmed: "+err.Error(), map[string]any{"proposal": p.ID, "hint": h})
			return fmt.Errorf("hint of %s not confirmed: %w", p.ID, err)
		}
		acts = append(acts, h.Actions...)
		evidence = append(evidence, ev...)
		review = review || rv
	}
	now := time.Now().UTC()
	q := &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1, Source: "cli",
		Kind: p.Kind, Subject: p.Subject, Key: p.Key, Title: p.Title, Actions: acts, Decisions: p.Decisions,
		Severity: p.Severity, Status: proposal.Pending, NeedsReview: p.NeedsReview || review,
		Report: p.Report + " Confirmed as root: " + hints[0].Reason + ".", Facts: confirmedFacts(p),
		Evidence: append(append([]string{}, p.Evidence...), evidence...),
		Extra:    map[string]string{"confirms": p.ID, "origin": p.Source}}
	if err := a.store.Save(q); err != nil {
		return err
	}
	p.Status = proposal.Resolved
	if p.Extra == nil {
		p.Extra = map[string]string{}
	}
	p.Extra["confirmed_as"] = q.ID
	if err := a.store.Save(p); err != nil {
		return err
	}
	cmds, _ := q.Commands()
	_, _ = a.audit.Append("proposal", q.ID+" from cli: confirms the hint of "+p.ID+": "+q.Title,
		map[string]any{"proposal": q.ID, "confirms": p.ID, "actions": q.Actions, "commands": cmds, "evidence": evidence})
	fmt.Fprintf(a.out, "Checked as root. The hint %s is closed and stored as proposal %s, which you can apply.\n\n", p.ID, q.ID)
	return a.present(ctx, q)
}

func (a *app) disk(ctx context.Context) error {
	rep, err := a.env.Disk(ctx, a.cfg.Disk, 50)
	if err != nil {
		return err
	}
	if a.o.json {
		return a.printJSON(rep)
	}
	writeDisk(a.out, rep, a.o.verbose)
	p := report.FromDisk("cli", rep)
	if len(p.Actions) == 0 {
		p.ID = "report"
		a.render(ctx, p)
		return nil
	}
	return a.present(ctx, a.keep(p))
}

// writeDisk prints the measurements of a disk report.
func writeDisk(w io.Writer, rep *diag.DiskReport, verbose bool) {
	fmt.Fprintf(w, "Disk usage of /\n  %s used of %s (%.1f %%), %s free\n", diag.HumanBytes(int64(rep.FS.Total-rep.FS.Free)),
		diag.HumanBytes(int64(rep.FS.Total)), rep.UsedPct, diag.HumanBytes(int64(rep.FS.Free)))
	if b := rep.Btrfs; len(b) > 0 && verbose {
		fmt.Fprintf(w, "  btrfs: device %s, allocated %s, used %s, free (estimated) %s, data ratio %.2f\n",
			diag.HumanBytes(b["device_size"]), diag.HumanBytes(b["device_allocated"]), diag.HumanBytes(b["used"]),
			diag.HumanBytes(b["free_estimated"]), float64(b["data_ratio"])/100)
	}
	fmt.Fprintf(w, "  system journal %s, cached packages %s\n", diag.HumanBytes(rep.Journal), diag.HumanBytes(rep.PkgCache))
	if len(rep.Snapshots) > 0 {
		fmt.Fprintf(w, "  snapshots hold %s that nothing else uses; the largest:\n", diag.HumanBytes(rep.SnapshotTotal))
		for i, s := range rep.Snapshots {
			if i >= 5 && !verbose {
				break
			}
			fmt.Fprintf(w, "    %4d  %-10s %s  %s\n", s.Snapshot.Number, diag.HumanBytes(s.Exclusive), s.Snapshot.Date, s.Snapshot.Description)
		}
	}
	fmt.Fprintf(w, "  forecast: %s\n\n", forecastText(rep.Forecast))
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
		writeSnapshots(a.out, snaps, diag.OrphanPre(snaps, time.Now(), 10*time.Minute))
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
		fmt.Fprintf(a.out, "What changed from %s to %s\n", name(from), name(to))
		if pd.Error != "" {
			fmt.Fprintf(a.out, "packages: %s\n", pd.Error)
		} else {
			fmt.Fprintf(a.out, "packages: %d added (+), %d removed (-), %d changed version (~); %d before, %d after\n",
				len(pd.Added), len(pd.Removed), len(pd.Changed), pd.FromSize, pd.ToSize)
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
			fmt.Fprintf(a.out, "files: %d changed, by directory:\n", fd.Total)
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
		why, before := "Requested from the command line.", ""
		if a.o.before != "" {
			pre, _ := a.env.FindApplySnapshots(ctx, a.o.before)
			if pre == 0 {
				return fmt.Errorf("no pre snapshot recorded for %s", a.o.before)
			}
			n = pre
			why = fmt.Sprintf("Undo %s: return the root to snapshot %d, taken just before it was applied.", a.o.before, pre)
			before = a.o.before
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
		p := a.keep(report.FromRollback("cli", plan, why, before))
		if !a.root {
			return a.present(ctx, p)
		}
		if a.o.yes && a.o.confirm == "" {
			// Non-interactive: store it and print the id and the code to
			// confirm with (`basalt apply ID --yes --confirm CODE`).
			a.render(ctx, p)
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
		if a.o.all {
			fmt.Fprintln(a.out, "There are no proposals.")
		} else {
			fmt.Fprintln(a.out, "Nothing is waiting for your decision.")
		}
		return nil
	}
	for _, p := range ps {
		fmt.Fprintln(a.out, report.Line(p))
	}
	fmt.Fprintln(a.out, "\n[review]: the assistant is not sure, read it first. [hint]: confirm it as root first. [report]: nothing to apply.")
	fmt.Fprintln(a.out, "Details: basalt show ID.   Then: sudo basalt apply ID, or sudo basalt ignore ID.")
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
	a.render(context.Background(), p)
	if r := p.Result; r != nil {
		verdict := "it worked and every check passed"
		if !r.OK {
			verdict = "it did NOT verify"
		}
		fmt.Fprintf(a.out, "\nApplied on %s UTC: %s (snapshots %d before, %d after; audit record #%d)\n",
			r.Time.Format("2006-01-02 15:04"), verdict, r.PreSnapshot, r.PostSnapshot, r.AuditSeq)
		for _, s := range append(r.Steps, r.Checks...) {
			fmt.Fprintf(a.out, "  [%s] %s %s\n", okMark(s.OK), s.What, strings.ReplaceAll(strings.TrimSpace(s.Output), "\n", " | "))
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
	fmt.Fprintf(a.out, "%s is closed without changing anything (recorded in the audit log).\n", p.ID)
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
		fmt.Fprintf(a.out, "The audit log is intact: records %d to %d, %s, %s, nothing edited, removed or reordered (%s).\n",
			sum.FirstSeq, sum.LastSeq, plural(len(sum.Files), "file", "files"), plural(sum.Seals, "seal", "seals"), a.cfg.AuditPath)
		if sum.Truncated {
			fmt.Fprintf(a.out, "Note: the oldest file left continues from one that was removed, so records before %d cannot be checked.\n", sum.FirstSeq)
		}
		if sum.Unsealed {
			fmt.Fprintln(a.out, "Note: the current file ends with a seal, so a rotation did not finish. Finish it: sudo basalt audit rotate")
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
		fmt.Fprintf(a.out, "The audit log was not rotated: %s.\n", res.Reason)
		return nil
	}
	fmt.Fprintf(a.out, "The audit log is sealed at record %d and kept as %s; it continues at record %d in %s.\n",
		res.Seal.Seq, res.Sealed, res.Continue.Seq, a.cfg.AuditPath)
	return nil
}

func okMark(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAILED"
}

// human prints a duration the way people say it (1h, 30m, 24h).
func human(d time.Duration) string {
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return plural(int(d/time.Hour), "hour", "hours")
	case d%time.Minute == 0 && d >= time.Minute:
		return plural(int(d/time.Minute), "minute", "minutes")
	}
	return d.String()
}

func forecastText(f diag.Forecast) string {
	switch {
	case f.Samples < 2:
		return "not enough history yet to tell"
	case f.DaysToFull > 0:
		when := "within a day"
		if d := math.Round(f.DaysToFull); d >= 2 {
			when = fmt.Sprintf("in about %.0f days", d)
		} else if d == 1 {
			when = "in about a day"
		}
		return fmt.Sprintf("at the current rate, full %s (%d samples over %.1f hours)", when, f.Samples, f.SpanHours)
	}
	return fmt.Sprintf("not growing (%d samples over %.1f hours)", f.Samples, f.SpanHours)
}

// confirmedFacts are a confirmed hint's facts: no longer a hint.
func confirmedFacts(p *proposal.Proposal) *explain.Facts {
	if p.Facts == nil {
		return nil
	}
	f := *p.Facts
	f.Hint = false
	return &f
}

// writeSnapshots lists the root snapshots; o are the unfinished ones.
func writeSnapshots(w io.Writer, snaps []diag.Snapshot, o []diag.Snapshot) {
	if len(snaps) == 0 {
		fmt.Fprintln(w, "There are no snapshots of the system yet.")
		return
	}
	fmt.Fprintf(w, "%5s  %-19s  %-6s  %s\n", "#", "date (UTC)", "kind", "what it was taken for")
	for _, s := range snaps {
		kind := map[string]string{"single": "single", "pre": "before", "post": "after"}[s.Type]
		if kind == "" {
			kind = s.Type
		}
		desc := s.Description
		if s.Type == "post" && s.Pre > 0 {
			desc += " (pairs with " + strconv.Itoa(s.Pre) + ")"
		}
		if s.Userdata["basalt"] == "apply" {
			desc += "  [proposal " + s.Userdata["proposal"] + "]"
		}
		fmt.Fprintf(w, "%5d  %-19s  %-6s  %s\n", s.Number, s.Date, kind, desc)
	}
	if len(o) > 0 {
		var ns []string
		for _, s := range o {
			ns = append(ns, strconv.Itoa(s.Number))
		}
		fmt.Fprintf(w, "\nA package transaction did not finish: snapshot %s has no matching \"after\" snapshot.\n", strings.Join(ns, ", "))
	}
	fmt.Fprintln(w, "\nCompare one with now: basalt snapshots diff N.   Go back to one: sudo basalt snapshots rollback N")
}
