package apply

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gateclient"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gatelink"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// throughGate is `basalt apply ID` where the approval gate decides: the
// proposal is queued (or found queued), root confirms it here with the
// code or a typed yes (recorded by the gate as the person's decision), or
// it waits for a decision in the queue; once approved, the gate starts
// basalt-gate-exec@ID.service, which applies it, and this command follows
// it to the end.
func (a *Applier) throughGate(ctx context.Context, p *proposal.Proposal, o Options, fp string) error {
	g := a.Gate
	rep, err := g.Submit(p)
	if err != nil {
		return fmt.Errorf("%w; nothing was changed", err)
	}
	id := rep.ID
	a.audit("propose", fmt.Sprintf("%s queued in the approval gate as %s (%s)", p.ID, id, rep.Decision),
		map[string]any{"proposal": p.ID, "gate_id": id, "decision": rep.Decision, "by": rep.By})
	switch rep.Decision {
	case gateclient.Refused:
		a.audit("refuse", "the approval gate refused "+p.ID+": "+rep.Reason, map[string]any{"proposal": p.ID, "gate_id": id, "by": rep.By})
		return fmt.Errorf("the approval gate refused it (%s); nothing was changed", rep.Reason)
	case gateclient.Allowed:
		a.printf("Approved by %s.\n", byText(rep.By))
	case gateclient.Asked:
		if err := a.confirmAtGate(id, p, o, fp); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the approval gate answered %q; nothing was changed", rep.Decision)
	}
	return a.follow(ctx, id, p)
}

// confirmAtGate is the person's decision at this root terminal: the code
// (--yes --confirm CODE), a typed yes, or a decision elsewhere in the
// queue when there is no terminal.
func (a *Applier) confirmAtGate(id string, p *proposal.Proposal, o Options, fp string) error {
	g := a.Gate
	switch {
	case o.Yes:
		if _, err := g.C.Confirm(id, o.Confirm, "code"); err != nil {
			a.audit("refuse", "confirmation refused by the approval gate for "+p.ID+": "+err.Error(),
				map[string]any{"proposal": p.ID, "gate_id": id, "given": o.Confirm})
			if strings.Contains(err.Error(), "does not match") {
				return fmt.Errorf("--yes needs --confirm %s (the code shown for exactly these commands); nothing was changed", fp)
			}
			return fmt.Errorf("%v; nothing was changed", err)
		}
	case a.Interactive:
		a.printf("Type yes to apply it, or anything else to cancel (request %s, code %s): ", id, fp)
		line, _ := bufio.NewReader(a.In).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			_, _ = g.C.Cancel(id)
			a.audit("decline", "declined "+p.ID, map[string]any{"proposal": p.ID, "gate_id": id, "fingerprint": fp})
			return ErrCancelled
		}
		if _, err := g.C.Confirm(id, fp, "terminal"); err != nil {
			return fmt.Errorf("%v; nothing was changed", err)
		}
	default:
		a.printf("Waiting for a decision in the approval queue (request %s, code %s): approve it in the desktop shell or with basalt-gate approve %s.\n", id, fp, id)
		rep, err := g.C.Wait(id, 300)
		if err != nil {
			return fmt.Errorf("approval gate: %w", err)
		}
		if rep.Decision != gateclient.Allowed {
			reason := rep.Reason
			if rep.TimedOut {
				reason = "nobody decided within 5 minutes"
			}
			return fmt.Errorf("not approved (%s: %s); nothing was changed", rep.Decision, reason)
		}
	}
	a.audit("confirm", fmt.Sprintf("confirmed %s (%s) at the approval gate as %s", p.ID, fp, id),
		map[string]any{"proposal": p.ID, "fingerprint": fp, "gate_id": id, "by": "person:tty-root"})
	return nil
}

// follow waits for the executor's result and prints what it did.
func (a *Applier) follow(ctx context.Context, id string, p *proposal.Proposal) error {
	every, limit := a.FollowEvery, a.FollowFor
	if every <= 0 {
		every = time.Second
	}
	if limit <= 0 {
		limit = 30 * time.Minute
	}
	unit := "basalt-gate-exec@" + id + ".service"
	a.printf("basalt-gate-exec applies it now (journalctl -u %s shows its output).\n", unit)
	deadline := time.Now().Add(limit)
	var view *gateclient.View
	for {
		rep, err := a.Gate.C.Status(id)
		if err == nil && rep.Request != nil && rep.Request.Result != "" {
			view = rep.Request
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no result from %s after %s; see journalctl -u %s and basalt show %s", unit, limit, unit, p.ID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
	q, err := a.Store.Load(p.ID)
	if err == nil && q.Result != nil {
		for _, st := range q.Result.Steps {
			a.printf("$ %s  [%s]\n", st.What, mark(st.OK))
		}
		for _, c := range q.Result.Checks {
			a.printf("  [%s] %s: %s\n", mark(c.OK), c.What, c.Output)
		}
		if q.Result.PreSnapshot > 0 {
			a.printf("To undo it: sudo basalt snapshots rollback --before %s   (back to snapshot %d, at the next boot)\n", p.ID, q.Result.PreSnapshot)
		}
	}
	if strings.HasPrefix(view.Result, "exit code 0") || view.Result == "done" {
		a.printf("\nDone: %s is applied and every check passed.\n", p.ID)
		return nil
	}
	return fmt.Errorf("%s did not verify (%s)", p.ID, view.Result)
}

func byText(by string) string {
	switch {
	case strings.HasPrefix(by, "rule:"):
		return "the rule " + strings.TrimPrefix(by, "rule:")
	case strings.HasPrefix(by, "person:"):
		return "a person (" + strings.TrimPrefix(by, "person:") + ")"
	}
	return by
}

// Execute is basalt apply ID --gate, in basalt-gate-exec@ID.service: it
// claims the approved request with what it is about to run (the
// proposal's calls and preview, rebuilt from the stored proposal), applies
// it without asking again, and reports the result to the gate.
func (a *Applier) Execute(ctx context.Context, id string) error {
	if a.Gate == nil || a.Gate.C == nil {
		return errors.New("--gate needs the approval gate (basalt-gated is not running)")
	}
	g := a.Gate.C
	st, err := g.Status(id)
	if err != nil || st.Request == nil {
		return fmt.Errorf("request %s: %v", id, err)
	}
	ref := st.Request.Ref
	if ref == "" {
		return fmt.Errorf("request %s is not a system assistant proposal", id)
	}
	p, err := a.Store.Load(ref)
	if err != nil {
		return err
	}
	gp, err := gatelink.Request(p)
	if err != nil {
		return err
	}
	if _, err := g.ClaimCalls(id, gp.Calls, gp.Preview, gatelink.Executor); err != nil {
		a.audit("refuse", "claim of "+id+" for "+p.ID+" refused: "+err.Error(), map[string]any{"proposal": p.ID, "gate_id": id})
		return fmt.Errorf("claim refused: %w", err)
	}
	by := st.Request.By
	runErr := a.Apply(ctx, p, Options{DecidedBy: id + ", " + by})
	ok, exit, detail := runErr == nil, 0, "applied and verified"
	if runErr != nil {
		exit, detail = 1, runErr.Error()
	}
	var snaps []string
	if p.Result != nil {
		for _, n := range []int{p.Result.PreSnapshot, p.Result.PostSnapshot} {
			if n > 0 {
				snaps = append(snaps, strconv.Itoa(n))
			}
		}
	}
	if _, err := g.Report(id, ok, exit, detail, snaps); err != nil {
		a.printf("Warning: the result could not be reported to the approval gate: %v\n", err)
	}
	if runErr == nil {
		// The restart into an offline update, once the gate knows.
		a.RunAfter(ctx)
	}
	return runErr
}
