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
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
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
}

// ErrCancelled is returned when the person declines.
var ErrCancelled = errors.New("cancelled, nothing was changed")

// Options of one apply.
type Options struct {
	Yes     bool   // non-interactive
	Confirm string // the fingerprint typed by the person (required with Yes)
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
	}
	what := "the command shown above"
	if len(cmds) > 1 {
		what = fmt.Sprintf("the %d commands shown above, in that order", len(cmds))
	}
	a.printf("Applying runs %s, as root (%s).\n", what, snapNote)

	switch {
	case o.Yes:
		if o.Confirm != fp {
			a.audit("refuse", "confirmation code mismatch for "+p.ID, map[string]any{"proposal": p.ID, "expected": fp, "given": o.Confirm})
			return fmt.Errorf("--yes needs --confirm %s (the code shown for exactly these commands); nothing was changed", fp)
		}
	case !a.Interactive:
		return fmt.Errorf("not a terminal: confirm with --yes --confirm %s", fp)
	default:
		a.printf("Type yes to apply it, or anything else to cancel: ")
		line, _ := bufio.NewReader(a.In).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			a.audit("decline", "declined "+p.ID, map[string]any{"proposal": p.ID, "fingerprint": fp})
			return ErrCancelled
		}
	}
	a.audit("confirm", fmt.Sprintf("confirmed %s (%s)", p.ID, fp), map[string]any{"proposal": p.ID, "fingerprint": fp, "commands": strs(cmds)})

	start := time.Now()
	res := &proposal.Result{Time: start.UTC(), Fingerprint: fp}
	desc := "basalt apply " + p.ID
	snap := a.Snapshots && snapperReady() && !rollbackAction(p.Actions)
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
	p.Result = res
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
		if a.Kind == action.SnapshotRollback || a.Kind == action.SnapshotDelete {
			return true
		}
	}
	return false
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

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n(truncated)"
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
