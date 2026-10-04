package report

import (
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// Width is the column text is wrapped at.
const Width = 78

// Options of a rendering.
type Options struct {
	Verbose bool // every evidence line and the decisions behind the proposal
	// NoCode leaves out the confirmation code (MCP clients never see it:
	// the person reads it at the command line).
	NoCode bool
}

// Parts is a rendered proposal in three pieces, so the prose in the
// middle can come from the optional humanize layer while the rest stays
// the template's.
type Parts struct {
	Head  string // the title and status lines
	Prose string // what is wrong, why, what applying will do (wrapped)
	Tail  string // planned commands, risk, undo, next step, evidence

	Facts   *explain.Facts  // nil for proposals stored before facts existed
	Changes []action.Action // the actions (or the hint's actions) the prose describes
	Text    explain.Text
}

// String is the whole rendering.
func (p Parts) String() string { return p.Head + p.Prose + p.Tail }

// Render prints a proposal with the default options.
func Render(p *proposal.Proposal) string { return RenderWith(p, Options{}).String() }

// RenderWith renders a proposal.
func RenderWith(p *proposal.Proposal, o Options) Parts {
	var out Parts
	fp, _ := p.Fingerprint()
	if len(p.Actions) == 0 || p.Status != proposal.Pending || o.NoCode {
		fp = ""
	}
	changes := p.Actions
	if len(changes) == 0 {
		for _, h := range p.Hints {
			changes = append(changes, h.Actions...)
		}
	}
	out.Changes = changes
	var t explain.Text
	if p.Facts != nil {
		t = explain.Write(p.Facts, changes)
	} else {
		t = explain.Text{Headline: p.Report}
	}
	out.Facts, out.Text = p.Facts, t
	short := p.Facts != nil && p.Facts.OK && len(p.Actions) == 0 && len(p.Hints) == 0

	// Head: id and title, then the status in words.
	var h strings.Builder
	fmt.Fprintf(&h, "[%s] %s\n", p.ID, p.Title)
	if !short {
		h.WriteString(statusLine(p) + "\n")
	}
	h.WriteString("\n")
	out.Head = h.String()
	out.Prose = Wrap(t.Prose(), "  ", Width) + "\n"

	var b strings.Builder
	switch {
	case len(p.Actions) > 0 && p.Status != proposal.Pending:
		b.WriteString("\nThe commands of this proposal\n")
		writeCmds(&b, p.Actions, "  ")
	case len(p.Actions) > 0:
		b.WriteString("\nWhat will run (exactly these commands, as root, in this order)\n")
		writeCmds(&b, p.Actions, "  ")
		writeRisk(&b, p, p.Actions)
	case len(p.Hints) > 0:
		b.WriteString("\nSuggested change (a hint, not yet a proposal you can apply)\n")
		for _, hint := range p.Hints {
			b.WriteString(Wrap("Why: "+hint.Reason+".", "  ", Width) + "\n")
			writeCmds(&b, hint.Actions, "  ")
		}
	case !short:
		b.WriteString("\nNothing will be changed: this is a report for you to read.\n")
	}

	// Next step: prose, then commands in fixed slots.
	next := nextCommands(p, fp)
	if t.Next != "" || len(next) > 0 {
		b.WriteString("\nNext step\n")
		if t.Next != "" {
			b.WriteString(Wrap(t.Next, "  ", Width) + "\n")
		}
		for _, l := range next {
			b.WriteString("  " + l + "\n")
		}
	}

	// Details, sized by severity.
	if !short || o.Verbose {
		writeEvidence(&b, p, o.Verbose)
	}
	out.Tail = b.String()
	return out
}

func statusLine(p *proposal.Proposal) string {
	var parts []string
	switch p.Status {
	case proposal.Pending:
		switch {
		case len(p.Hints) > 0 && len(p.Actions) == 0:
			parts = append(parts, "Waiting for confirmation as root")
		case len(p.Actions) > 0:
			parts = append(parts, "Waiting for your decision")
		default:
			parts = append(parts, "For your information")
		}
	case proposal.Applied:
		parts = append(parts, "Applied")
	case proposal.Failed:
		parts = append(parts, "Applied, but it did not verify")
	case proposal.Ignored:
		parts = append(parts, "Ignored")
	case proposal.Resolved:
		parts = append(parts, "Resolved")
	default:
		parts = append(parts, p.Status)
	}
	if p.NeedsReview && len(p.Actions) > 0 && p.Status == proposal.Pending {
		parts = append(parts, "please review")
	}
	src := map[string]string{"daemon": "found by the background service", "cli": "found by basalt", "mcp": "proposed through MCP"}[p.Source]
	if src == "" {
		src = "from " + p.Source
	}
	parts = append(parts, src+" on "+p.Created.Format("2006-01-02 15:04")+" UTC")
	if p.Seen > 1 {
		parts = append(parts, fmt.Sprintf("seen %d times", p.Seen))
	}
	if p.Severity >= 5 {
		parts = append(parts, "urgent")
	}
	return strings.Join(parts, ", ")
}

func writeCmds(b *strings.Builder, acts []action.Action, indent string) {
	for _, a := range acts {
		cs, err := a.Commands()
		if err != nil {
			b.WriteString(indent + "(invalid action: " + err.Error() + ")\n")
			continue
		}
		for _, c := range cs {
			b.WriteString(indent + "$ " + c.String() + "\n")
		}
	}
}

func writeRisk(b *strings.Builder, p *proposal.Proposal, acts []action.Action) {
	level, why := explain.Risk(acts)
	b.WriteString("\n" + Wrap("Risk: "+explain.RiskName(level)+" ("+why+").", "", Width) + "\n")
	undo, cmd := explain.Undo(acts)
	b.WriteString(Wrap("Undo: "+undo, "", Width) + "\n")
	if cmd && p.Status == proposal.Pending {
		b.WriteString("  $ sudo basalt snapshots rollback --before " + p.ID + "\n")
	}
}

func nextCommands(p *proposal.Proposal, fp string) []string {
	var out []string
	if p.Status != proposal.Pending {
		if p.Status == proposal.Applied && p.Result != nil && p.Result.PreSnapshot > 0 {
			out = append(out, "Undo it:    sudo basalt snapshots rollback --before "+p.ID)
		}
		return out
	}
	switch {
	case len(p.Actions) > 0 && p.ID != "preview":
		out = append(out, "Apply it:   sudo basalt apply "+p.ID)
		if fp != "" {
			out = append(out, "            (without a prompt: sudo basalt apply "+p.ID+" --yes --confirm "+fp+")")
		}
		out = append(out, "Ignore it:  sudo basalt ignore "+p.ID)
	case len(p.Hints) > 0:
		out = append(out, "Confirm it as root: sudo basalt confirm "+p.ID)
		out = append(out, "(the assistant checks the snapshot and turns the hint into a proposal you can apply)")
	}
	if f := p.Facts; f != nil && len(p.Actions) == 0 && len(p.Hints) == 0 {
		var cmds []string
		switch {
		case f.Incomplete && f.Kind == "unit":
			cmds = append(cmds, "basalt why "+f.Subject)
		case f.Kind == "unit" && f.Cause == "dependency_failed" && f.Has("dep"):
			cmds = append(cmds, "basalt why "+f.V("dep"))
		case f.Kind == "unit" && f.Cause == "disk_full":
			cmds = append(cmds, "basalt disk")
		case f.Kind == "unit" && f.Cause == "crash_loop":
			cmds = append(cmds, "journalctl -u "+f.Subject+" -b")
			if f.V("core") == "yes" {
				cmds = append(cmds, "coredumpctl info COREDUMP_UNIT="+f.Subject)
			}
			if f.V("start_limit") == "yes" {
				cmds = append(cmds, "sudo systemctl reset-failed "+f.Subject)
			}
		case f.Kind == "unit" && f.Cause == "oom":
			cmds = append(cmds, "systemctl show "+f.Subject+" -p MemoryMax,MemoryHigh,MemorySwapMax,MemoryPeak",
				"sudo systemctl set-property "+f.Subject+" MemoryMax=SIZE")
		case f.Kind == "unit" && f.Cause == "config_error" && !f.Has("snapshot"):
			if f.Has("checker") {
				cmds = append(cmds, "sudo "+f.V("checker"))
			}
			cmds = append(cmds, "sudo systemctl restart "+f.Subject)
		case f.Kind == "unit" && (f.Cause == "missing_file" || f.Cause == "permission"):
			cmds = append(cmds, "sudo systemctl restart "+f.Subject)
		case f.Kind == "disk" && f.V("confined") == "yes" && !f.OK:
			cmds = append(cmds, "sudo basalt disk")
		}
		if len(cmds) > 0 {
			out = append(out, "Commands you may run yourself:")
			for _, c := range cmds {
				out = append(out, "  $ "+c)
			}
		}
	}
	if p.ID != "" && p.ID != "preview" && p.ID != "report" && len(p.Actions) == 0 && len(p.Hints) == 0 && (p.Facts == nil || !p.Facts.OK) {
		out = append(out, "Close it:   sudo basalt ignore "+p.ID)
	}
	return out
}

func writeEvidence(b *strings.Builder, p *proposal.Proposal, verbose bool) {
	if len(p.Evidence) > 0 {
		max := 8
		if p.Severity >= 4 {
			max = 25
		}
		if verbose {
			max = len(p.Evidence)
		}
		b.WriteString("\nEvidence\n")
		for i, e := range p.Evidence {
			if i >= max {
				fmt.Fprintf(b, "  (%d more lines: basalt show %s --verbose)\n", len(p.Evidence)-max, p.ID)
				break
			}
			if strings.HasPrefix(e, "  ") {
				b.WriteString("      " + strings.TrimPrefix(e, "  ") + "\n") // a continuation of the line above
			} else {
				b.WriteString("  - " + e + "\n")
			}
		}
	}
	if len(p.Decisions) > 0 && (verbose || p.NeedsReview) {
		b.WriteString("\nHow sure the assistant is\n")
		for _, d := range p.Decisions {
			fmt.Fprintf(b, "  - %s: %s, %.2f (threshold %.2f, %s)\n", d.Question.ID, d.Answer.Top, d.Answer.Confidence, d.Threshold, d.Answer.Backend)
		}
	}
}

// Wrap fills text to width columns with an indent, breaking only at
// spaces (paths and commands are never split).
func Wrap(text, indent string, width int) string {
	var lines []string
	line := indent
	for _, w := range strings.Fields(text) {
		if len(line) > len(indent) && len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = indent
		}
		if len(line) > len(indent) {
			line += " "
		}
		line += w
	}
	if len(line) > len(indent) {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// Line is a one-line listing.
func Line(p *proposal.Proposal) string {
	flag := ""
	if p.NeedsReview && len(p.Actions) > 0 && p.Status == proposal.Pending {
		flag = " [review]"
	}
	if len(p.Hints) > 0 {
		flag += " [hint]"
	} else if len(p.Actions) == 0 {
		flag += " [report]"
	}
	return fmt.Sprintf("%-9s %-8s %-8s %s%s", p.ID, p.Status, p.Kind, p.Title, flag)
}
