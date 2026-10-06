// Package apply is the only path that changes the system: it shows the
// exact commands of a proposal, asks for confirmation, takes a snapshot
// before and after, runs the commands, verifies the result and writes an
// audit record. It runs in the administrator's own session (`basalt apply`
// with sudo), never inside the confined daemon or the MCP server.
package apply

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gatelink"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
)

// Applier carries what an apply needs.
type Applier struct {
	Store       proposal.Store
	Audit       *audit.Log
	Exec        runner.Exec
	In          io.Reader
	Out         io.Writer
	Interactive bool // stdin is a terminal
	Snapshots   bool // take pre/post snapshots when snapper is set up
	Settle      time.Duration
	// Gate is the approval gate (nil or Absent: the confirmation here, as
	// always; Observe: the confirmation here, told to the gate; Enforce:
	// the gate decides and basalt-gate-exec@ID.service applies).
	Gate *gatelink.Link
	// Follow is how often and how long the command line follows the
	// executor unit (tests shorten them).
	FollowEvery, FollowFor time.Duration

	decidedBy string    // who decided the apply in progress
	started   time.Time // when it started
}

// ErrCancelled is returned when the person declines.
var ErrCancelled = errors.New("cancelled, nothing was changed")

// Options of one apply.
type Options struct {
	Yes     bool   // non-interactive
	Confirm string // the fingerprint typed by the person (required with Yes)
	// DecidedBy: the approval gate already decided (the executor,
	// basalt apply ID --gate, after claiming): no confirmation here; the
	// audit names who decided.
	DecidedBy string
}

func (a *Applier) printf(format string, args ...any) { fmt.Fprintf(a.Out, format, args...) }

// Apply runs a proposal after confirmation.
func (a *Applier) Apply(ctx context.Context, p *proposal.Proposal, o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("applying a change needs root: run it with sudo")
	}
	if p.Status != proposal.Pending && p.Status != proposal.Failed {
		return fmt.Errorf("proposal %s is %s; only pending or failed proposals can be applied", p.ID, p.Status)
	}
	if len(p.Actions) == 0 && len(p.Hints) > 0 {
		return fmt.Errorf("proposal %s is a hint from the confined view: confirm the snapshot first with `sudo basalt confirm %s`", p.ID, p.ID)
	}
	if len(p.Actions) == 0 {
		return fmt.Errorf("proposal %s is a report: it has no change to apply", p.ID)
	}
	if err := p.Validate(); err != nil {
		a.audit("refuse", "invalid proposal "+p.ID+": "+err.Error(), map[string]any{"proposal": p.ID, "source": p.Source, "actions": p.Actions})
		if errors.Is(err, proposal.ErrNeedsRootView) {
			return fmt.Errorf("proposal %s: %w (sudo basalt confirm %s)", p.ID, err, p.ID)
		}
		return fmt.Errorf("proposal %s: %w", p.ID, err)
	}
	var cmds []runner.Command
	for _, act := range p.Actions {
		cs, err := act.Commands()
		if err != nil {
			a.audit("refuse", "invalid action in "+p.ID+": "+err.Error(), map[string]any{"proposal": p.ID, "action": act})
			return fmt.Errorf("proposal %s: %w", p.ID, err)
		}
		cmds = append(cmds, cs...)
	}
	fp, _ := p.Fingerprint()

	a.printf("%s\n", report.Render(p))
	snapNote := "a snapshot is taken before and after"
	if rollbackAction(p.Actions) {
		snapNote = "no extra snapshot: the rollback works on snapshots itself"
	} else if toggleOnly(p.Actions) {
		snapNote = "no snapshot: turning a channel on or off changes one setting, and the next update takes its own snapshot"
	}
	what := "the command shown above"
	if len(cmds) > 1 {
		what = fmt.Sprintf("the %d commands shown above, in that order", len(cmds))
	}
	a.printf("Applying runs %s, as root (%s).\n", what, snapNote)

	by := "root at a terminal"
	switch {
	case o.DecidedBy != "":
		by = "the approval gate (" + o.DecidedBy + ")"
	case a.Gate != nil && a.Gate.Mode == gatelink.Enforce:
		return a.throughGate(ctx, p, o, fp)
	case o.Yes:
		if o.Confirm != fp {
			a.audit("refuse", "confirmation code mismatch for "+p.ID, map[string]any{"proposal": p.ID, "expected": fp, "given": o.Confirm})
			a.Gate.Observe(p, "refused", "a wrong confirmation code")
			return fmt.Errorf("--yes needs --confirm %s (the code shown for exactly these commands); nothing was changed", fp)
		}
		by = "root with the confirmation code"
	case !a.Interactive:
		return fmt.Errorf("not a terminal: confirm with --yes --confirm %s", fp)
	default:
		a.printf("Type yes to apply it, or anything else to cancel: ")
		line, _ := bufio.NewReader(a.In).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			a.audit("decline", "declined "+p.ID, map[string]any{"proposal": p.ID, "fingerprint": fp})
			a.Gate.Observe(p, "declined", by)
			return ErrCancelled
		}
	}
	if o.DecidedBy == "" {
		a.Gate.Observe(p, "approved", by)
	}
	a.audit("confirm", fmt.Sprintf("confirmed %s (%s) by %s", p.ID, fp, by), map[string]any{"proposal": p.ID, "fingerprint": fp,
		"commands": strs(cmds), "by": by})
	a.decidedBy = by
	return a.run(ctx, p, cmds, fp)
}

// progress records where a running apply is (the desktop's Updates page
// shows it as steps: the snapshot, each command, the checks), in
// proposal.ProgressDir, outside the snapshots. Best effort.
func (a *Applier) progress(p *proposal.Proposal, step, steps int, what string) {
	if a.started.IsZero() {
		a.started = time.Now()
	}
	proposal.WriteProgress(p.ID, step, steps, what, a.started)
}

// run takes the snapshots, runs the commands, verifies and records.
func (a *Applier) run(ctx context.Context, p *proposal.Proposal, cmds []runner.Command, fp string) error {

	start := time.Now()
	res := &proposal.Result{Time: start.UTC(), Fingerprint: fp}
	desc := "basalt apply " + p.ID
	snap := a.Snapshots && snapperReady() && !rollbackAction(p.Actions) && !toggleOnly(p.Actions)
	// Steps: the snapshot (when taken), each command, the checks.
	steps, step := len(cmds)+1, 0
	if snap {
		steps++
		step++
		a.progress(p, step, steps, "snapshot")
	}
	if snap {
		if n, err := a.snapper(ctx, "pre", 0, desc, p.ID); err == nil {
			res.PreSnapshot = n
			a.printf("Snapshot %d taken (before the change).\n", n)
		} else {
			a.printf("Warning: no snapshot could be taken before the change: %v\n", err)
		}
	}

	ok := true
	for _, c := range cmds {
		step++
		a.progress(p, step, steps, c.Description)
		a.printf("$ %s\n", c.String())
		r := a.Exec.Run(ctx, c)
		step := proposal.Step{What: c.String(), OK: r.OK(), Code: r.Code, Output: clip(r.Out, 4000)}
		if r.Err != nil {
			step.Output = strings.TrimSpace(step.Output + "\n" + r.Err.Error())
		}
		if r.Out != "" {
			a.printf("%s\n", indent(clip(r.Out, 2000)))
		}
		res.Steps = append(res.Steps, step)
		if !r.OK() {
			ok = false
			a.printf("That command failed (exit code %d), so the remaining ones were not run.\n", r.Code)
			break
		}
	}

	if snap && res.PreSnapshot > 0 {
		if n, err := a.snapper(ctx, "post", res.PreSnapshot, desc, p.ID); err == nil {
			res.PostSnapshot = n
			a.printf("Snapshot %d taken (after the change).\n", n)
		}
	}

	if ok {
		step++
		a.progress(p, step, steps, "checks")
		if a.Settle > 0 {
			time.Sleep(a.Settle)
		}
		a.printf("\nChecking that it worked\n")
		for _, act := range p.Actions {
			for _, chk := range act.Verify(start) {
				good, detail := chk.Run(ctx, a.Exec)
				res.Checks = append(res.Checks, proposal.Step{What: chk.Description, OK: good, Output: detail})
				a.printf("  [%s] %s: %s\n", mark(good), chk.Description, detail)
				if !good {
					ok = false
				}
			}
		}
	}
	res.OK = ok
	a.progress(p, 0, 0, "")
	p.Result = res
	if ok {
		// A source keeps who added it (shown on its card).
		for _, act := range p.Actions {
			if act.Kind == action.SourceAdd {
				_ = sources.Stamp(action.SourcePaths, act.Params["id"], p.ID, a.decidedBy+actorSuffix())
			}
		}
	}
	if ok {
		p.Status = proposal.Applied
	} else {
		p.Status = proposal.Failed
	}
	rec := a.audit("apply", fmt.Sprintf("%s %s: %s", p.ID, p.Status, p.Title), map[string]any{
		"proposal": p.ID, "title": p.Title, "actions": p.Actions, "result": res})
	res.AuditSeq = rec.Seq
	if err := a.Store.Save(p); err != nil {
		a.printf("Warning: could not update the proposal: %v\n", err)
	}
	if ok {
		a.printf("\nDone: %s is applied and every check passed (audit record #%d).\n", p.ID, rec.Seq)
	} else {
		a.printf("\n%s did not work as expected: see the failed step or check above (audit record #%d).\n", p.ID, rec.Seq)
	}
	if res.PreSnapshot > 0 {
		a.printf("To undo it: sudo basalt snapshots rollback --before %s   (back to snapshot %d, at the next boot)\n", p.ID, res.PreSnapshot)
	}
	if !ok {
		return fmt.Errorf("%s did not verify", p.ID)
	}
	return nil
}

// Ignore marks a proposal ignored.
func (a *Applier) Ignore(p *proposal.Proposal, why string) error {
	if p.Status != proposal.Pending {
		return fmt.Errorf("proposal %s is %s", p.ID, p.Status)
	}
	p.Status = proposal.Ignored
	if p.Extra == nil {
		p.Extra = map[string]string{}
	}
	p.Extra["ignored_because"] = why
	a.audit("ignore", "ignored "+p.ID+": "+p.Title, map[string]any{"proposal": p.ID, "reason": why})
	return a.Store.Save(p)
}

func (a *Applier) audit(typ, text string, data any) audit.Record {
	if a.Audit == nil {
		return audit.Record{}
	}
	r, err := a.Audit.Append(typ, text, data)
	if err != nil {
		a.printf("warning: audit log: %v\n", err)
	}
	return r
}

func snapperReady() bool {
	if _, err := os.Stat("/etc/snapper/configs/root"); err != nil {
		return false
	}
	// A snapshot booted read-only from the menu cannot take snapshots.
	b, _ := os.ReadFile("/proc/cmdline")
	return !strings.Contains(string(b), "basalt.snapshot=")
}

func rollbackAction(as []action.Action) bool {
	for _, a := range as {
		if a.Kind == action.SnapshotRollback || a.Kind == action.SnapshotDelete || a.Kind == action.UpdateRollback {
			return true
		}
	}
	return false
}

// toggleOnly: a proposal that only turns channels on or off needs no
// snapshot (one setting; every update takes its own).
func toggleOnly(as []action.Action) bool {
	for _, a := range as {
		if a.Kind != action.RepoEnable && a.Kind != action.RepoDisable {
			return false
		}
		if a.Params["definition"] != "" {
			return false
		}
	}
	return len(as) > 0
}

// actorSuffix names the person behind sudo or pkexec, when known.
func actorSuffix() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return ", user " + u
	}
	if id := os.Getenv("PKEXEC_UID"); id != "" {
		if u, err := user.LookupId(id); err == nil {
			return ", user " + u.Username
		}
		return ", uid " + id
	}
	return ""
}

func (a *Applier) snapper(ctx context.Context, typ string, pre int, desc, id string) (int, error) {
	argv := []string{"snapper", "-c", "root", "create", "-t", typ, "-p", "-c", "number", "-d", desc,
		"-u", "basalt=apply,proposal=" + id}
	if typ == "post" {
		argv = append(argv, "--pre-number", strconv.Itoa(pre))
	}
	r := a.Exec.Read(ctx, argv...)
	if !r.OK() {
		return 0, fmt.Errorf("snapper: %s", strings.TrimSpace(r.Out))
	}
	return strconv.Atoi(strings.TrimSpace(r.Out))
}

func strs(cs []runner.Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.String()
	}
	return out
}

// clip keeps the start and the end of a long output: the end is where a
// failing command says why.
func clip(s string, n int) string {
	if len(s) > n {
		head := n / 4
		return s[:head] + "\n(truncated)\n" + s[len(s)-(n-head):]
	}
	return s
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

func mark(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}
