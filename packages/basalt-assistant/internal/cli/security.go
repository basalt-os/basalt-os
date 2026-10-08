package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/risks"
)

// security: `basalt security`, the changes Security and Activity offers
// on its cards. Each stores a proposal (nothing runs here); the desktop
// queues it in the approval gate and basalt-gate-exec applies it.
//
//	basalt security risks [--json]          the risks an administrator accepted
//	basalt security audit [--json]          the audit.run proposal (installs the suite first when missing)
//	basalt security accept ITEM [--json]    the risk.accept proposal for ITEM, by the person who asks
//	basalt security review ITEM [--json]    the risk.review proposal: ITEM is a warning again
func (a *app) security(ctx context.Context) error {
	sub := "risks"
	if len(a.o.args) > 1 {
		sub = a.o.args[1]
	}
	switch sub {
	case "risks":
		f, err := risks.Load()
		if err != nil {
			return err
		}
		if a.o.json {
			return a.printJSON(f)
		}
		if len(f.Risks) == 0 {
			fmt.Fprintln(a.out, "No accepted risks.")
			return nil
		}
		for _, k := range f.List() {
			r := f.Risks[k]
			fmt.Fprintf(a.out, "  %-12s accepted by %s on %s\n", k, r.By, r.At.Local().Format("2006-01-02 15:04"))
		}
		return nil
	case "audit":
		install := "no"
		if _, err := os.Stat(action.AuditSuiteBin); err != nil {
			install = "yes"
		}
		run := time.Now().UTC().Format("2006-01-02-150405")
		act := action.Action{Kind: action.AuditRun, Params: map[string]string{"run": run, "install": install}}
		if err := act.Validate(); err != nil {
			return err
		}
		p := newProposal("security", "audit", "security:audit", "run the AI audit suite on this computer")
		p.Actions = []action.Action{act}
		p.Report = "Run the public AI audit suite on this computer: like an attacker would, it tries to get around the " +
			"protections for AI agents (their confinement, their network limits, the activity record, the confirmation " +
			"step). It runs as its own unprivileged account with fake test data and changes none of your files. " +
			"The results appear in Security and Activity."
		if install == "yes" {
			p.Report += " The suite is not installed yet: it is installed first from the Basalt repositories, " +
				"checked against their signing key."
		}
		return a.storeAndPresent(ctx, p)
	case "accept", "review":
		if len(a.o.args) < 3 {
			return fmt.Errorf("usage: basalt security %s ITEM (one of %v)", sub, risks.Items)
		}
		item := a.o.args[2]
		if !risks.ValidItem(item) {
			return fmt.Errorf("%q is not a risk that can be accepted (one of %v)", item, risks.Items)
		}
		var act action.Action
		var title, report string
		if sub == "accept" {
			by := requester()
			act = action.Action{Kind: action.RiskAccept, Params: map[string]string{"item": item, "by": by}}
			title = "accept the risk of " + item
			report = "Record that " + by + " knows about the warning on " + item + " and accepts it. Security and " +
				"Activity then shows it as accepted, with the date and who accepted it, and stops counting it. " +
				"If it ever becomes a problem it shows as a problem again. Review again undoes this."
		} else {
			act = action.Action{Kind: action.RiskReview, Params: map[string]string{"item": item}}
			title = "review the risk of " + item + " again"
			report = "Forget that the risk of " + item + " was accepted: Security and Activity counts it as a warning again."
		}
		if err := act.Validate(); err != nil {
			return err
		}
		p := newProposal("security", item, "security:risk:"+item, title)
		p.Actions = []action.Action{act}
		p.Report = report
		return a.storeAndPresent(ctx, p)
	}
	return fmt.Errorf("unknown: basalt security %s (basalt help)", sub)
}

// requester is the person who asked: through pkexec (the desktop's read
// helper) or sudo, the account behind it, else the current one.
func requester() string {
	for _, k := range []string{"PKEXEC_UID", "SUDO_UID"} {
		if v := os.Getenv(k); v != "" {
			if _, err := strconv.Atoi(v); err == nil {
				if u, err := user.LookupId(v); err == nil && risks.ValidUser(u.Username) {
					return u.Username
				}
			}
		}
	}
	if u, err := user.Current(); err == nil && risks.ValidUser(u.Username) {
		return u.Username
	}
	return "root"
}

// riskHelper is `basalt __risk accept ITEM BY | clear ITEM`: the executor
// of risk.accept and risk.review, as root, after the gate allowed it.
func (a *app) riskHelper() error {
	if !a.root {
		return errors.New("__risk runs as root, from a confirmed proposal")
	}
	args := a.o.args
	switch {
	case len(args) == 4 && args[1] == "accept":
		if err := risks.Accept(args[2], args[3], time.Now()); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "accepted the risk of %s (by %s)\n", args[2], args[3])
		return nil
	case len(args) == 3 && args[1] == "clear":
		if err := risks.Clear(args[2]); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "the risk of %s is no longer accepted\n", args[2])
		return nil
	}
	return errors.New("usage: basalt __risk accept ITEM BY | clear ITEM")
}
