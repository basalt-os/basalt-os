package report

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/titles"
)

// Width is the column text is wrapped at.
const Width = 78

// Options of a rendering.
type Options struct {
	Verbose bool // every evidence line and the decisions behind the proposal
	// NoCode leaves out the confirmation code (MCP clients never see it:
	// the person reads it at the command line).
	NoCode bool
	// Tr is the language of the text: the zero value is English (the
	// daemon's log, MCP clients), i18n.Local() the reader's language (the
	// command line). Commands, ids, codes and evidence stay as they are.
	Tr i18n.Tr
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

// Render prints a proposal with the default options (English).
func Render(p *proposal.Proposal) string { return RenderWith(p, Options{}).String() }

// changes are the actions, or the hint's actions, the prose describes.
func changes(p *proposal.Proposal) []action.Action {
	cs := p.Actions
	if len(cs) == 0 {
		for _, h := range p.Hints {
			cs = append(cs, h.Actions...)
		}
	}
	return cs
}

// Prose is what is wrong, why and what applying will do, in the language
// of tr: written from the facts, or for a proposal without facts (and
// one stored before they existed) its stored report, translated sentence
// by sentence where the sentences are known.
func Prose(tr i18n.Tr, p *proposal.Proposal) string {
	if p.Facts == nil {
		return titles.Report(tr, p.Report)
	}
	return explain.WriteIn(tr, p.Facts, changes(p)).Prose()
}

// RenderWith renders a proposal.
func RenderWith(p *proposal.Proposal, o Options) Parts {
	var out Parts
	fp, _ := p.Fingerprint()
	if len(p.Actions) == 0 || p.Status != proposal.Pending || o.NoCode {
		fp = ""
	}
	tr := o.Tr
	cs := changes(p)
	out.Changes = cs
	var t explain.Text
	if p.Facts != nil {
		t = explain.WriteIn(tr, p.Facts, cs)
	} else {
		t = explain.Text{Headline: titles.Report(tr, p.Report)}
	}
	out.Facts, out.Text = p.Facts, t
	short := p.Facts != nil && p.Facts.OK && len(p.Actions) == 0 && len(p.Hints) == 0

	// Head: id and title, then the status in words.
	var h strings.Builder
	fmt.Fprintf(&h, "[%s] %s\n", p.ID, titles.In(tr, p.Title))
	if !short {
		h.WriteString(statusLine(tr, p) + "\n")
	}
	h.WriteString("\n")
	out.Head = h.String()
	out.Prose = Wrap(t.Prose(), "  ", Width) + "\n"

	var b strings.Builder
	switch {
	case len(p.Actions) > 0 && p.Status != proposal.Pending:
		b.WriteString("\n" + tr.T("The commands of this proposal") + "\n")
		writeCmds(tr, &b, p.Actions, "  ")
	case len(p.Actions) > 0:
		b.WriteString("\n" + tr.T("What will run (exactly these commands, as root, in this order)") + "\n")
		writeCmds(tr, &b, p.Actions, "  ")
		writeRisk(tr, &b, p, p.Actions)
	case len(p.Hints) > 0:
		b.WriteString("\n" + tr.T("Suggested change (a hint, not yet a proposal you can apply)") + "\n")
		for _, hint := range p.Hints {
			b.WriteString(Wrap(fmt.Sprintf(tr.T("Why: %s."), titles.Phrase(tr, hint.Reason)), "  ", Width) + "\n")
			writeCmds(tr, &b, hint.Actions, "  ")
		}
	case !short:
		b.WriteString("\n" + tr.T("Nothing will be changed: this is a report for you to read.") + "\n")
	}

	// Next step: prose, then commands in fixed slots.
	next := nextCommands(tr, p, fp)
	if t.Next != "" || len(next) > 0 {
		b.WriteString("\n" + tr.T("Next step") + "\n")
		if t.Next != "" {
			b.WriteString(Wrap(t.Next, "  ", Width) + "\n")
		}
		for _, l := range next {
			b.WriteString("  " + l + "\n")
		}
	}

	// Details, sized by severity.
	if !short || o.Verbose {
		writeEvidence(tr, &b, p, o.Verbose)
	}
	out.Tail = b.String()
	return out
}

// StatusWord is a proposal's status as one word in the language of tr
// (pending --json keeps the English word in status).
func StatusWord(tr i18n.Tr, status string) string {
	switch status {
	case proposal.Pending:
		return tr.T("pending")
	case proposal.Applied:
		return tr.T("applied")
	case proposal.Failed:
		return tr.T("failed")
	case proposal.Ignored:
		return tr.T("ignored")
	case proposal.Resolved:
		return tr.T("resolved")
	case proposal.Scheduled:
		return tr.T("scheduled")
	}
	return status
}

func statusLine(tr i18n.Tr, p *proposal.Proposal) string {
	var parts []string
	switch p.Status {
	case proposal.Pending:
		switch {
		case len(p.Hints) > 0 && len(p.Actions) == 0:
			parts = append(parts, tr.T("Waiting for confirmation as root"))
		case len(p.Actions) > 0:
			parts = append(parts, tr.T("Waiting for your decision"))
		default:
			parts = append(parts, tr.T("For your information"))
		}
	case proposal.Applied:
		parts = append(parts, tr.T("Applied"))
	case proposal.Failed:
		parts = append(parts, tr.T("Applied, but it did not verify"))
	case proposal.Ignored:
		parts = append(parts, tr.T("Ignored"))
	case proposal.Resolved:
		parts = append(parts, tr.T("Resolved"))
	default:
		parts = append(parts, StatusWord(tr, p.Status))
	}
	if p.NeedsReview && len(p.Actions) > 0 && p.Status == proposal.Pending {
		parts = append(parts, tr.T("please review"))
	}
	var src string
	switch p.Source {
	case "daemon":
		src = tr.T("found by the background service")
	case "cli":
		src = tr.T("found by basalt")
	case "mcp":
		src = tr.T("proposed through MCP")
	default:
		src = fmt.Sprintf(tr.T("from %s"), p.Source)
	}
	parts = append(parts, fmt.Sprintf(tr.T("%s on %s UTC"), src, p.Created.Format("2006-01-02 15:04")))
	if p.Seen > 1 {
		parts = append(parts, fmt.Sprintf(tr.N("seen %d time", "seen %d times", p.Seen), p.Seen))
	}
	if p.Severity >= 5 {
		parts = append(parts, tr.T("urgent"))
	}
	return strings.Join(parts, ", ")
}

func writeCmds(tr i18n.Tr, b *strings.Builder, acts []action.Action, indent string) {
	for _, a := range acts {
		cs, err := a.Commands()
		if err != nil {
			b.WriteString(indent + fmt.Sprintf(tr.T("(invalid action: %s)"), err.Error()) + "\n")
			continue
		}
		for _, c := range cs {
			b.WriteString(indent + "$ " + c.String() + "\n")
		}
	}
}

func writeRisk(tr i18n.Tr, b *strings.Builder, p *proposal.Proposal, acts []action.Action) {
	level, why := explain.RiskIn(tr, acts)
	b.WriteString("\n" + Wrap(fmt.Sprintf(tr.T("Risk: %s (%s)."), explain.RiskNameIn(tr, level), why), "", Width) + "\n")
	undo, cmd := explain.UndoIn(tr, acts)
	b.WriteString(Wrap(fmt.Sprintf(tr.T("Undo: %s"), undo), "", Width) + "\n")
	if cmd && p.Status == proposal.Pending {
		b.WriteString("  $ sudo basalt snapshots rollback --before " + p.ID + "\n")
	}
}

// slot is a labeled command of the next step ("Apply it:   sudo basalt
// apply ID"): the commands start in one column, after the longest label
// of the reader's language.
func slot(tr i18n.Tr, label, cmd string) string {
	col := 12
	for _, l := range []string{tr.T("Apply it:"), tr.T("Ignore it:"), tr.T("Undo it:"), tr.T("Close it:")} {
		if n := utf8.RuneCountInString(l) + 2; n > col {
			col = n
		}
	}
	if cmd == "" {
		return strings.Repeat(" ", col) + label
	}
	return label + strings.Repeat(" ", max(1, col-utf8.RuneCountInString(label))) + cmd
}

func nextCommands(tr i18n.Tr, p *proposal.Proposal, fp string) []string {
	var out []string
	if p.Status != proposal.Pending {
		if p.Status == proposal.Applied && p.Result != nil && p.Result.PreSnapshot > 0 {
			out = append(out, slot(tr, tr.T("Undo it:"), "sudo basalt snapshots rollback --before "+p.ID))
		}
		return out
	}
	switch {
	case len(p.Actions) > 0 && p.ID != "preview":
		out = append(out, slot(tr, tr.T("Apply it:"), "sudo basalt apply "+p.ID))
		if fp != "" {
			out = append(out, slot(tr, fmt.Sprintf(tr.T("(without a prompt: %s)"), "sudo basalt apply "+p.ID+" --yes --confirm "+fp), ""))
		}
		out = append(out, slot(tr, tr.T("Ignore it:"), "sudo basalt ignore "+p.ID))
	case len(p.Hints) > 0:
		out = append(out, fmt.Sprintf(tr.T("Confirm it as root: %s"), "sudo basalt confirm "+p.ID))
		out = append(out, tr.T("(the assistant checks the snapshot and turns the hint into a proposal you can apply)"))
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
			out = append(out, tr.T("Commands you may run yourself:"))
			for _, c := range cmds {
				out = append(out, "  $ "+c)
			}
		}
	}
	if p.ID != "" && p.ID != "preview" && p.ID != "report" && len(p.Actions) == 0 && len(p.Hints) == 0 && (p.Facts == nil || !p.Facts.OK) {
		out = append(out, slot(tr, tr.T("Close it:"), "sudo basalt ignore "+p.ID))
	}
	return out
}

func writeEvidence(tr i18n.Tr, b *strings.Builder, p *proposal.Proposal, verbose bool) {
	if len(p.Evidence) > 0 {
		max := 8
		if p.Severity >= 4 {
			max = 25
		}
		if verbose {
			max = len(p.Evidence)
		}
		b.WriteString("\n" + tr.T("Evidence") + "\n")
		for i, e := range p.Evidence {
			if i >= max {
				n := len(p.Evidence) - max
				b.WriteString("  " + fmt.Sprintf(tr.N("(%d more line: %s)", "(%d more lines: %s)", n), n, "basalt show "+p.ID+" --verbose") + "\n")
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
		b.WriteString("\n" + tr.T("How sure the assistant is") + "\n")
		for _, d := range p.Decisions {
			b.WriteString("  - " + fmt.Sprintf(tr.T("%s: %s, %.2f (threshold %.2f, %s)"), d.Question.ID, d.Answer.Top, d.Answer.Confidence, d.Threshold, d.Answer.Backend) + "\n")
		}
	}
}

// Wrap fills text to width columns with an indent, breaking only at
// spaces (paths and commands are never split). Columns are characters,
// not bytes (accented letters).
func Wrap(text, indent string, width int) string {
	var lines []string
	line := indent
	cols := utf8.RuneCountInString
	for _, w := range strings.Fields(text) {
		if len(line) > len(indent) && cols(line)+1+cols(w) > width {
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

// Line is a one-line listing (English).
func Line(p *proposal.Proposal) string { return LineIn(i18n.Tr{}, p) }

// LineIn is Line in the language of tr: the status and the title
// translated, the flags too.
func LineIn(tr i18n.Tr, p *proposal.Proposal) string {
	flag := ""
	if p.NeedsReview && len(p.Actions) > 0 && p.Status == proposal.Pending {
		flag = " " + tr.T("[review]")
	}
	if len(p.Hints) > 0 {
		flag += " " + tr.T("[hint]")
	} else if len(p.Actions) == 0 {
		flag += " " + tr.T("[report]")
	}
	return pad(p.ID, 9) + " " + pad(StatusWord(tr, p.Status), 8) + " " + pad(p.Kind, 8) + " " + titles.In(tr, p.Title) + flag
}

// pad fills s with spaces to n characters, like %-*s for text with
// accented letters.
func pad(s string, n int) string {
	if c := utf8.RuneCountInString(s); c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return s
}
