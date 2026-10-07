package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/keymap"
)

// keyboard is `basalt keyboard`: the system's keyboard (what the login
// screen, the text console and new accounts use), and the keyboard.system
// proposal that changes it.
//
//	basalt keyboard [--json]                              the system's keyboard now
//	basalt keyboard set LAYOUTS [--options OPTIONS]       e.g. br,us(intl) --options grp:alt_shift_toggle
//
// The desktop's Settings, Keyboard stores the proposal through its read
// helper (`--json`); applying it runs localectl in the assistant's
// executor, after the person's confirmation.
func (a *app) keyboard(ctx context.Context) error {
	sub := ""
	if len(a.o.args) > 1 {
		sub = a.o.args[1]
	}
	switch sub {
	case "", "status":
		cur := keymap.ReadCurrent()
		if a.o.json {
			return a.printJSON(cur)
		}
		layouts := keymap.FormatLayouts(cur.Layouts)
		if layouts == "" {
			layouts = "none set (the login screen uses us)"
		}
		fmt.Fprintf(a.out, "  Layouts     %s\n", layouts)
		fmt.Fprintf(a.out, "  Model       %s\n", orNone(cur.Model))
		fmt.Fprintf(a.out, "  Options     %s\n", orNone(strings.Join(cur.Options, ",")))
		fmt.Fprintf(a.out, "  Console     %s\n", orNone(cur.Console))
		return nil
	case "set":
		if len(a.o.args) < 3 {
			return errors.New("usage: basalt keyboard set LAYOUTS [--options OPTIONS], e.g. br,us(intl)")
		}
		cs, err := keymap.ParseLayouts(a.o.args[2])
		if err != nil {
			return err
		}
		layouts := keymap.FormatLayouts(cs)
		cur := keymap.ReadCurrent()
		model := cur.Model
		if model == "" {
			model = "pc105"
		}
		km := keymap.ConsoleKeymap(cs)
		act := action.Action{Kind: action.KeyboardSystem, Params: map[string]string{
			"layouts": layouts, "options": a.o.kv["--options"], "model": model, "keymap": km}}
		if err := act.Validate(); err != nil {
			return err
		}
		names := layouts
		if reg, err := keymap.LoadRegistry(); err == nil {
			ns := make([]string, len(cs))
			for i, c := range cs {
				ns[i] = reg.Describe(c)
			}
			names = strings.Join(ns, ", ")
		}
		p := newProposal("keyboard", layouts, "keyboard:system", "use "+layouts+" for the login screen, the console and new accounts")
		p.Actions = []action.Action{act}
		was := keymap.FormatLayouts(cur.Layouts)
		if was == "" {
			was = "none set (us)"
		}
		p.Evidence = []string{"now: " + was + ", console " + orNone(cur.Console), "console keymap: " + km}
		p.Report = "The login screen, the text console and accounts created from now on type with " + names +
			" (the first is the default; the console uses the keymap " + km + "). People who chose their own layouts in " +
			"Settings, Keyboard keep them. To change it again, choose other layouts the same way."
		return a.storeAndPresent(ctx, p)
	}
	return fmt.Errorf("unknown: basalt keyboard %s (basalt help)", sub)
}
