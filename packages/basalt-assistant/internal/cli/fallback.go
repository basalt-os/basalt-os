package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
)

// fallback is `basalt fallback`: which login screen starts while GNOME is
// the fallback session (release audit 2026-11, S15), and the
// desktop.fallback proposal that switches it.
//
//	basalt fallback [--json]              GNOME installed? which display manager starts?
//	basalt fallback gnome [--json]        GDM as the login screen (GNOME has a screen lock)
//	basalt fallback basalt [--json]       greetd and the Basalt login screen again
//
// GNOME has no screen lock without GDM, so while the person uses GNOME,
// GDM should be the display manager. The desktop's Settings, About stores
// the proposal through its read helper (`--json`); applying it runs
// systemctl in the assistant's executor, after the person's confirmation
// (the approval gate, basalt-apply@, or `sudo basalt fallback gnome
// --apply` in any terminal, GNOME's included: nothing needs the Basalt
// shell). It takes effect at the next start.
func (a *app) fallback(ctx context.Context) error {
	target := ""
	if len(a.o.args) > 1 {
		target = a.o.args[1]
	}
	installed, dm := action.FallbackInstalled(), action.DisplayManager()
	switch target {
	case "", "status":
		if a.o.json {
			return a.printJSON(map[string]any{"gnome_installed": installed, "display_manager": dm})
		}
		if installed {
			fmt.Fprintln(a.out, i18n.T("GNOME as a fallback: installed"))
		} else {
			fmt.Fprintln(a.out, i18n.T("GNOME as a fallback: not installed (basalt-desktop-gnome-fallback)"))
		}
		fmt.Fprintf(a.out, i18n.T("Login screen at the next start: %s")+"\n", orNoneT(dm))
		return nil
	case "gnome", "basalt":
	default:
		return fmt.Errorf("unknown: basalt fallback %s (gnome or basalt)", target)
	}
	if target == "gnome" && !installed {
		return errors.New("GNOME and its login screen (GDM) are not installed: install basalt-desktop-gnome-fallback first")
	}
	on := "gdm"
	title := "use GDM as the login screen while GNOME is the fallback"
	report := "GNOME's login screen (GDM) starts from the next start, so the GNOME session can lock the screen. " +
		"The Basalt session stays in its list. To go back, choose the Basalt login screen again the same way."
	if target == "basalt" {
		on = "greetd"
		title = "use the Basalt login screen again"
		report = "The Basalt login screen starts from the next start. GNOME stays installed and in its list, without a screen lock."
	}
	if dm == on {
		return fmt.Errorf("%s is already the login screen", on)
	}
	act := action.Action{Kind: action.DesktopFallback, Params: map[string]string{"target": target}}
	if err := act.Validate(); err != nil {
		return err
	}
	p := newProposal("desktop.fallback", target, "desktop:fallback", title)
	p.Actions = []action.Action{act}
	p.Evidence = []string{"now: " + orNone(dm)}
	p.Report = report
	return a.storeAndPresent(ctx, p)
}
