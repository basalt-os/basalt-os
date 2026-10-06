package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/drivers"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
)

// drivers: `basalt drivers` (Additional drivers, docs/nvidia.md).
//
//	basalt drivers [--json]                         the GPUs, the driver that fits, its state
//	basalt drivers license nvidia                   the NVIDIA Driver License Agreement
//	basalt drivers install nvidia [display|compute] the driver.install proposal (root stores it;
//	                                                --apply goes straight to the confirmation)
//	basalt drivers rollback                         back to the snapshot taken before the install
func (a *app) drivers(ctx context.Context) error {
	sys := drivers.Real(a.env.R)
	sub := ""
	if len(a.o.args) > 1 {
		sub = a.o.args[1]
	}
	switch sub {
	case "":
		r := drivers.Build(ctx, sys, a.o.json)
		if a.o.json {
			return a.printJSON(r)
		}
		a.writeDrivers(r)
		return nil
	case "license":
		if len(a.o.args) < 3 || a.o.args[2] != "nvidia" {
			return errors.New("usage: basalt drivers license nvidia")
		}
		fmt.Fprint(a.out, drivers.NvidiaLicense)
		return nil
	case "install":
		if len(a.o.args) < 3 || a.o.args[2] != "nvidia" {
			return errors.New("usage: basalt drivers install nvidia [display|compute]")
		}
		variant := ""
		if len(a.o.args) > 3 {
			variant = a.o.args[3]
			if variant != "display" && variant != "compute" {
				return errors.New("usage: basalt drivers install nvidia [display|compute]")
			}
		}
		r := drivers.Build(ctx, sys, false)
		p, err := drivers.InstallProposal(r, "cli", variant)
		if err != nil {
			return err
		}
		p = a.keep(p)
		if a.o.json {
			return a.printJSON(storedRef(p, a.root))
		}
		return a.present(ctx, p)
	case "rollback":
		r := drivers.Build(ctx, sys, false)
		n, before := 0, r.State.Proposal
		if before != "" {
			n, _ = a.env.FindApplySnapshots(ctx, before)
		}
		if n == 0 {
			n, _ = strconv.Atoi(r.State.Snapshot)
		}
		if n == 0 {
			return errors.New(i18n.T("No snapshot from before the NVIDIA driver install is known on this system."))
		}
		plan, err := a.env.PlanRollback(ctx, n)
		if err != nil {
			return err
		}
		why := fmt.Sprintf("Undo the NVIDIA driver install: return the root to snapshot %d, taken just before it.", n)
		if r.State.Mode == "fallback" && r.State.Reason != "" {
			why = fmt.Sprintf("The NVIDIA driver did not work (%s). Return the root to snapshot %d, taken just before it was installed.", r.State.Reason, n)
		}
		p := a.keep(report.FromRollback("cli", plan, why, before))
		if a.o.json {
			return a.printJSON(storedRef(p, a.root))
		}
		return a.present(ctx, p)
	}
	return fmt.Errorf("unknown: basalt drivers %s (basalt help)", sub)
}

// storedRef is the machine answer of a stored proposal. The confirmation
// code is not in it: the person (or the desktop's confirmation sheet)
// reads it from `basalt show ID`.
func storedRef(p *proposal.Proposal, root bool) map[string]any {
	cmds, _ := p.Commands()
	return map[string]any{"id": p.ID, "title": p.Title, "stored": root, "commands": cmds, "needs_review": p.NeedsReview}
}

func (a *app) writeDrivers(r drivers.Report) {
	say := func(s string) { fmt.Fprintln(a.out, s) }
	say(i18n.T("Graphics hardware"))
	if len(r.GPUs) == 0 {
		say("  " + i18n.T("No display controller was found."))
	}
	for _, g := range r.GPUs {
		line := fmt.Sprintf("  %s (%s:%s, PCI %s)", g.Name, g.VendorID, g.DeviceID, g.Slot)
		drv := g.Driver
		if drv == "" {
			drv = "-"
		}
		line += ", " + fmt.Sprintf(i18n.T("driver now: %s"), drv)
		if g.BootVGA {
			line += ", " + i18n.T("shows the boot screen")
		}
		say(line)
	}
	rec, st := r.Recommendation, r.State
	say("")
	switch rec.Action {
	case "install":
		say(fmt.Sprintf(i18n.T("Recommended: the NVIDIA driver %s (proprietary) for %s."), rec.Version, rec.GPU))
		say(i18n.T("What changes if you install it:"))
		for _, c := range rec.Changes {
			say(report.Wrap("- "+c, "  ", report.Width))
		}
		say(fmt.Sprintf(i18n.T("Packages: %s"), strings.Join(rec.Packages, " ")))
	case "installed":
		say(fmt.Sprintf(i18n.T("The NVIDIA driver %s is installed (%s)."), st.Version, st.Installed))
	case "fallback":
		say(fmt.Sprintf(i18n.T("The NVIDIA driver %s is installed but switched off since %s: %s. nouveau is used."), st.Version, st.Since, st.Reason))
	case "unavailable":
		say(i18n.T("Driver installation is coming soon: the basalt-nonfree repository is not published yet."))
	case "none":
		say(i18n.T("No NVIDIA GPU: the drivers already installed are the right ones."))
	}
	for _, n := range rec.Notes {
		say(report.Wrap(n, "", report.Width))
	}
	for _, b := range rec.Blockers {
		say(report.Wrap(i18n.T("Before installing:")+" "+b, "", report.Width))
	}
	if rec.Action == "installed" || rec.Action == "fallback" {
		var state string
		switch st.Mode {
		case "trial":
			state = i18n.T("waiting for the first start to check it")
		case "active":
			state = i18n.T("in use")
		case "fallback":
			state = i18n.T("off, nouveau is used")
		default:
			state = i18n.T("not set up")
		}
		say(fmt.Sprintf(i18n.T("State: %s."), state))
		if st.ThisBoot != "" {
			say(fmt.Sprintf(i18n.T("This start uses: %s %s"), st.ThisBoot, st.ThisBootReason))
		}
		if st.HeldKernel != "" {
			say(fmt.Sprintf(i18n.T("Kernel %s waits for its NVIDIA module; the computer keeps starting an older kernel."), st.HeldKernel))
		}
	}
	say(fmt.Sprintf(i18n.T("Secure Boot: %s."), drivers.SBWords(r.SecureBoot)))
	for _, n := range r.Notices {
		say(report.Wrap(n, "", report.Width))
	}
	say("")
	switch rec.Action {
	case "install":
		if len(rec.Blockers) == 0 {
			say(i18n.T("Read the license: basalt drivers license nvidia"))
			say(i18n.T("Install it: sudo basalt drivers install nvidia --apply"))
		}
	case "fallback":
		say(i18n.T("Return to the snapshot taken before the install: sudo basalt drivers rollback --apply"))
		say(i18n.T("Try the NVIDIA driver again at the next start: sudo basalt-nvidia retry"))
	}
	say(fmt.Sprintf(i18n.T("More: %s"), r.Docs))
}
