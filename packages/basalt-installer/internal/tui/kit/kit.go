// Package kit holds the widgets the installer needs that tui-kit does not
// have yet: a multi-field form, a checklist, a row picker with disabled
// rows, a scrolling pager, a progress panel with a log tail, and a secret
// acknowledgement dialog. They follow tui-kit's dialog conventions (values
// the host model stores; Update returns whether the message was consumed;
// View(theme, width, height); Done/Accepted; wrap, never truncate commands)
// so they can move upstream into tui-kit/ui unchanged. Nothing here depends
// on the installer.
package kit

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"
)

// hints renders a dialog footer (tui-kit's hintLines is internal).
func hints(t theme.Theme, content int, hs []ui.KeyHint) []string {
	var lines []string
	cur, used := "", 0
	for _, h := range hs {
		plain := h.Key + " " + h.Desc
		part := t.Key.Render(h.Key) + t.KeyDesc.Render(" "+h.Desc)
		switch {
		case cur == "":
			cur, used = part, lipgloss.Width(plain)
		case used+4+lipgloss.Width(plain) <= content:
			cur += t.KeyDesc.Render("    ") + part
			used += 4 + lipgloss.Width(plain)
		default:
			lines = append(lines, cur)
			cur, used = part, lipgloss.Width(plain)
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func styled(lines []string, s lipgloss.Style) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = s.Render(l)
	}
	return out
}

// contentWidth is the inner width of a dialog drawn in an area of width w.
func contentWidth(t theme.Theme, w, maxw int) int {
	inner := min(max(w-4, 30), maxw)
	return max(inner-t.Dialog.GetHorizontalFrameSize(), 20)
}

func box(t theme.Theme, lines []string, content, width, height int) string {
	b := t.Dialog.Width(content + t.Dialog.GetHorizontalFrameSize()).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, b)
}

// --- Form --------------------------------------------------------------------------------

// Field is one line of a Form.
type Field struct {
	Label string
	Help  string
	Input textinput.Model
	// Validate returns a message when the value is not acceptable.
	Validate func(value string) string
	// Optional fields may stay empty.
	Optional bool
}

// TextField builds a text field.
func TextField(label, value, placeholder string) Field {
	in := textinput.New()
	in.SetValue(value)
	in.Placeholder = placeholder
	in.CharLimit = 4096
	in.Prompt = ""
	return Field{Label: label, Input: in}
}

// SecretField builds a masked field (passwords). Its value is never drawn.
func SecretField(label string) Field {
	f := TextField(label, "", "")
	f.Input.EchoMode = textinput.EchoPassword
	f.Input.EchoCharacter = '*'
	return f
}

// Form is a multi-field dialog: tab and the arrows move between fields,
// enter on the last field (or ctrl+s anywhere) submits after every field
// validated, esc cancels.
type Form struct {
	Title  string
	Body   string
	Fields []Field
	Focus  int
	// Check validates the whole form after the fields (for example: two
	// password fields that must match). It returns a message or "".
	Check    func(values []string) string
	Error    string
	Done     bool
	Accepted bool
	Payload  any
}

// NewForm builds a form with the first field focused.
func NewForm(title, body string, fields ...Field) Form {
	f := Form{Title: title, Body: body, Fields: fields}
	f.focus(0)
	return f
}

func (f *Form) focus(i int) {
	if len(f.Fields) == 0 {
		return
	}
	i = (i + len(f.Fields)) % len(f.Fields)
	for j := range f.Fields {
		if j == i {
			f.Fields[j].Input.Focus()
		} else {
			f.Fields[j].Input.Blur()
		}
	}
	f.Focus = i
}

// Value returns the trimmed value of field i (secrets are not trimmed).
func (f Form) Value(i int) string {
	v := f.Fields[i].Input.Value()
	if f.Fields[i].Input.EchoMode == textinput.EchoPassword {
		return v
	}
	return strings.TrimSpace(v)
}

// Values returns every field's value.
func (f Form) Values() []string {
	out := make([]string, len(f.Fields))
	for i := range f.Fields {
		out[i] = f.Value(i)
	}
	return out
}

func (f *Form) submit() {
	for i, fl := range f.Fields {
		v := f.Value(i)
		if v == "" && !fl.Optional {
			f.Error = fl.Label + ": required"
			f.focus(i)
			return
		}
		if fl.Validate != nil && v != "" {
			if msg := fl.Validate(v); msg != "" {
				f.Error = fl.Label + ": " + msg
				f.focus(i)
				return
			}
		}
	}
	if f.Check != nil {
		if msg := f.Check(f.Values()); msg != "" {
			f.Error = msg
			return
		}
	}
	f.Error, f.Done, f.Accepted = "", true, true
}

// Update handles a message; it returns a command and whether it consumed it.
func (f *Form) Update(msg tea.Msg) (tea.Cmd, bool) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "tab", "down":
			f.focus(f.Focus + 1)
			return nil, true
		case "shift+tab", "up":
			f.focus(f.Focus - 1)
			return nil, true
		case "enter":
			if f.Focus < len(f.Fields)-1 {
				f.focus(f.Focus + 1)
				return nil, true
			}
			f.submit()
			return nil, true
		case "ctrl+s":
			f.submit()
			return nil, true
		case "esc", "ctrl+c":
			f.Done, f.Accepted = true, false
			return nil, true
		}
	}
	if len(f.Fields) == 0 {
		return nil, false
	}
	var cmd tea.Cmd
	f.Fields[f.Focus].Input, cmd = f.Fields[f.Focus].Input.Update(msg)
	return cmd, true
}

// View renders the form centered in the area.
func (f *Form) View(t theme.Theme, width, height int) string {
	content := contentWidth(t, width, 86)
	lines := styled(ui.Wrap(f.Title, content), t.Title)
	if f.Body != "" {
		lines = append(lines, "")
		lines = append(lines, styled(ui.WrapBody(f.Body, content), t.Base)...)
	}
	labelW := 0
	for _, fl := range f.Fields {
		labelW = max(labelW, lipgloss.Width(fl.Label))
	}
	labelW = min(labelW, content/3)
	for i := range f.Fields {
		fl := &f.Fields[i]
		fl.Input.Width = max(content-labelW-3, 8)
		marker := "  "
		label := t.Muted.Render(ui.Pad(fl.Label, labelW))
		if i == f.Focus {
			marker = t.Accent.Render("> ")
			label = t.Accent.Render(ui.Pad(fl.Label, labelW))
		}
		lines = append(lines, "", marker+label+" "+fl.Input.View())
		if fl.Help != "" && i == f.Focus {
			for _, h := range ui.Wrap(fl.Help, max(content-labelW-3, 10)) {
				lines = append(lines, strings.Repeat(" ", labelW+3)+t.Muted.Render(h))
			}
		}
	}
	if f.Error != "" {
		lines = append(lines, "")
		lines = append(lines, styled(ui.Wrap(f.Error, content), t.Danger)...)
	}
	lines = append(lines, "")
	lines = append(lines, hints(t, content, []ui.KeyHint{{Key: "tab/↑↓", Desc: "field"}, {Key: "enter", Desc: "next / done"},
		{Key: "ctrl+s", Desc: "done"}, {Key: "esc", Desc: "back"}})...)
	return box(t, lines, content, width, height)
}

// --- Checklist ------------------------------------------------------------------------------

// CheckItem is one line of a Checklist.
type CheckItem struct {
	Label  string
	Detail string
	On     bool
	// Locked items cannot be toggled (shown with the reason in Detail).
	Locked bool
}

// Checklist toggles items with space; enter accepts, esc cancels.
type Checklist struct {
	Title    string
	Body     string
	Items    []CheckItem
	Cursor   int
	Done     bool
	Accepted bool
	Payload  any
}

// Update handles a key; it returns true when consumed.
func (c *Checklist) Update(msg tea.Msg) bool {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false
	}
	switch key.String() {
	case "up", "k":
		c.Cursor = max(c.Cursor-1, 0)
	case "down", "j":
		c.Cursor = min(c.Cursor+1, len(c.Items)-1)
	case " ", "x":
		if it := &c.Items[c.Cursor]; !it.Locked {
			it.On = !it.On
		}
	case "enter":
		c.Done, c.Accepted = true, true
	case "esc", "ctrl+c":
		c.Done, c.Accepted = true, false
	}
	return true
}

// View renders the checklist centered in the area.
func (c *Checklist) View(t theme.Theme, width, height int) string {
	content := contentWidth(t, width, 86)
	lines := styled(ui.Wrap(c.Title, content), t.Title)
	if c.Body != "" {
		lines = append(lines, "")
		lines = append(lines, styled(ui.WrapBody(c.Body, content), t.Base)...)
	}
	lines = append(lines, "")
	for i, it := range c.Items {
		box := "[ ]"
		if it.On {
			box = "[x]"
		}
		row := box + " " + it.Label
		style := t.Base
		if it.Locked {
			style = t.Muted
		}
		if i == c.Cursor {
			style = t.Accent
			row = "> " + row
		} else {
			row = "  " + row
		}
		lines = append(lines, style.Render(ui.Truncate(row, content)))
		if it.Detail != "" {
			for _, d := range ui.Wrap(it.Detail, content-6) {
				lines = append(lines, "      "+t.Muted.Render(d))
			}
		}
	}
	lines = append(lines, "")
	lines = append(lines, hints(t, content, []ui.KeyHint{{Key: "↑/↓", Desc: "move"}, {Key: "space", Desc: "toggle"},
		{Key: "enter", Desc: "done"}, {Key: "esc", Desc: "back"}})...)
	return box(t, lines, content, width, height)
}

// --- RowPicker ------------------------------------------------------------------------------

// Row is one choice of a RowPicker. A row with Disabled set is shown dimmed
// with the reason and cannot be chosen.
type Row struct {
	Cells    []string
	Disabled string
}

// RowPicker picks one row of a small table (disks, interfaces).
type RowPicker struct {
	Title    string
	Body     string
	Columns  []ui.Column
	Rows     []Row
	Cursor   int
	Message  string
	Done     bool
	Accepted bool
	Payload  any
}

// Update handles a key; it returns true when consumed.
func (p *RowPicker) Update(msg tea.Msg) bool {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false
	}
	p.Message = ""
	switch key.String() {
	case "up", "k":
		p.Cursor = max(p.Cursor-1, 0)
	case "down", "j":
		p.Cursor = min(p.Cursor+1, len(p.Rows)-1)
	case "enter":
		if len(p.Rows) == 0 {
			return true
		}
		if r := p.Rows[p.Cursor]; r.Disabled != "" {
			p.Message = "cannot use this one: " + r.Disabled
			return true
		}
		p.Done, p.Accepted = true, true
	case "esc", "ctrl+c":
		p.Done, p.Accepted = true, false
	}
	return true
}

// View renders the picker centered in the area.
func (p *RowPicker) View(t theme.Theme, width, height int) string {
	content := contentWidth(t, width, 110)
	lines := styled(ui.Wrap(p.Title, content), t.Title)
	if p.Body != "" {
		lines = append(lines, "")
		lines = append(lines, styled(ui.WrapBody(p.Body, content), t.Base)...)
	}
	lines = append(lines, "")
	rows := make([][]string, len(p.Rows))
	styles := make([]*lipgloss.Style, len(p.Rows))
	for i, r := range p.Rows {
		rows[i] = r.Cells
		if r.Disabled != "" {
			s := t.Row.Foreground(lipgloss.Color(t.Palette.MutedForeground))
			if t.NoColor {
				s = t.Row.Faint(true)
			}
			styles[i] = &s
		}
	}
	tb := ui.Table{Columns: p.Columns, Rows: rows, Styles: styles, Selected: p.Cursor, Height: len(rows)}
	lines = append(lines, strings.Split(tb.Render(t, content), "\n")...)
	if len(p.Rows) > 0 {
		if d := p.Rows[p.Cursor].Disabled; d != "" {
			lines = append(lines, "")
			lines = append(lines, styled(ui.Wrap("not available: "+d, content), t.Warn)...)
		}
	} else {
		lines = append(lines, t.Muted.Render("(nothing found)"))
	}
	if p.Message != "" {
		lines = append(lines, styled(ui.Wrap(p.Message, content), t.Danger)...)
	}
	lines = append(lines, "")
	lines = append(lines, hints(t, content, []ui.KeyHint{{Key: "↑/↓", Desc: "move"}, {Key: "enter", Desc: "select"}, {Key: "esc", Desc: "back"}})...)
	return box(t, lines, content, width, height)
}

// --- Pager -------------------------------------------------------------------------------------

// Pager shows a long text (a command preview, a log) that scrolls. Lines
// starting with "$ " are drawn in the command style and wrapped with an
// indented continuation, like tui-kit's confirm dialog: nothing is cut.
type Pager struct {
	Title  string
	Text   string
	Offset int
	Hints  []ui.KeyHint
	// Keys the host handles itself (Update leaves them unconsumed).
	PassKeys []string
	wrapped  []string
	width    int
	maxOff   int
}

// Update scrolls; it returns false for keys in PassKeys.
func (p *Pager) Update(msg tea.Msg) bool {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return false
	}
	k := key.String()
	for _, pk := range p.PassKeys {
		if k == pk {
			return false
		}
	}
	switch k {
	case "down", "j":
		p.Offset++
	case "up", "k":
		p.Offset--
	case "pgdown", " ", "f":
		p.Offset += 15
	case "pgup", "b":
		p.Offset -= 15
	case "home", "g":
		p.Offset = 0
	case "end", "G":
		p.Offset = p.maxOff
	default:
		return false
	}
	p.Offset = min(max(p.Offset, 0), p.maxOff)
	return true
}

// View renders the pager filling the area.
func (p *Pager) View(t theme.Theme, width, height int) string {
	content := max(width-4, 20)
	if p.width != content || p.wrapped == nil {
		p.width = content
		p.wrapped = nil
		for _, line := range strings.Split(p.Text, "\n") {
			trimmed := strings.TrimLeft(line, " ")
			indent := line[:len(line)-len(trimmed)]
			switch {
			case strings.HasPrefix(trimmed, "$ "):
				for _, w := range ui.WrapBody(trimmed, max(content-len(indent), 10)) {
					p.wrapped = append(p.wrapped, indent+t.Command.Render(w))
				}
			case strings.HasPrefix(trimmed, "== "):
				p.wrapped = append(p.wrapped, t.Title.Render(line))
			case strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "| "):
				for _, w := range ui.WrapBody(line, content) {
					p.wrapped = append(p.wrapped, t.Muted.Render(w))
				}
			default:
				for _, w := range ui.WrapBody(line, content) {
					p.wrapped = append(p.wrapped, t.Base.Render(w))
				}
			}
		}
	}
	head := []string{t.Title.Render(ui.Truncate(p.Title, content))}
	foot := hints(t, content, p.Hints)
	room := max(height-len(head)-len(foot)-2, 3)
	p.maxOff = max(len(p.wrapped)-room, 0)
	p.Offset = min(max(p.Offset, 0), p.maxOff)
	end := min(p.Offset+room, len(p.wrapped))
	body := append([]string{}, p.wrapped[p.Offset:end]...)
	for len(body) < room {
		body = append(body, "")
	}
	pos := t.Muted.Render(fmt.Sprintf("lines %d-%d of %d   ↑/↓ pgup/pgdn scroll", p.Offset+1, end, len(p.wrapped)))
	out := append(append(append(head, body...), pos), foot...)
	return lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(out, "\n"))
}

// --- Progress ---------------------------------------------------------------------------------

// Progress is a progress panel: a bar, the running step, its command and
// the last lines of output.
type Progress struct {
	Title    string
	Fraction float64
	Step     string
	Command  string
	Tail     []string
	// TailSize is how many output lines are kept (default 8).
	TailSize int
	Status   string
}

// Add appends one output line to the tail.
func (p *Progress) Add(line string) {
	n := p.TailSize
	if n == 0 {
		n = 8
	}
	p.Tail = append(p.Tail, line)
	if len(p.Tail) > n {
		p.Tail = p.Tail[len(p.Tail)-n:]
	}
}

// Bar renders a bar of the given width. ASCII when ascii is true (serial
// consoles without UTF-8).
func Bar(f float64, width int, ascii bool) string {
	f = min(max(f, 0), 1)
	fill := int(f*float64(width) + 0.5)
	full, empty := "█", "░"
	if ascii {
		full, empty = "#", "-"
	}
	return strings.Repeat(full, fill) + strings.Repeat(empty, width-fill)
}

// View renders the panel filling the area.
func (p *Progress) View(t theme.Theme, width, height int, ascii bool) string {
	content := max(width-4, 20)
	pct := fmt.Sprintf(" %3d%%", int(p.Fraction*100))
	lines := []string{t.Title.Render(ui.Truncate(p.Title, content)), "",
		t.Accent.Render(Bar(p.Fraction, content-len(pct), ascii)) + t.Base.Render(pct), ""}
	if p.Step != "" {
		lines = append(lines, styled(ui.Wrap(p.Step, content), t.Base)...)
	}
	if p.Command != "" {
		lines = append(lines, styled(ui.WrapBody("$ "+p.Command, content), t.Command)...)
	}
	lines = append(lines, "")
	room := max(height-len(lines)-3, 1)
	tail := p.Tail
	if len(tail) > room {
		tail = tail[len(tail)-room:]
	}
	for _, l := range tail {
		lines = append(lines, t.Muted.Render(ui.Truncate(l, content)))
	}
	for len(lines) < height-2 {
		lines = append(lines, "")
	}
	if p.Status != "" {
		lines = append(lines, t.Muted.Render(ui.Truncate(p.Status, content)))
	}
	return lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(lines, "\n"))
}

// --- SecretAck --------------------------------------------------------------------------------

// SecretAck shows a secret once and asks the person to prove they stored it
// by typing part of it. The secret is drawn in the command style, spaced
// for reading aloud.
type SecretAck struct {
	Title  string
	Body   string
	Secret string
	Prompt string
	Input  textinput.Model
	// Check returns an error message for a wrong answer.
	Check    func(answer string) string
	Error    string
	Done     bool
	Accepted bool
}

// NewSecretAck builds the dialog with the input focused.
func NewSecretAck(title, body, secret, prompt string, check func(string) string) SecretAck {
	in := textinput.New()
	in.Prompt = "> "
	in.CharLimit = 128
	in.Focus()
	return SecretAck{Title: title, Body: body, Secret: secret, Prompt: prompt, Input: in, Check: check}
}

// Update handles a message. There is no cancel: the secret must be
// acknowledged (ctrl+c is left to the host).
func (s *SecretAck) Update(msg tea.Msg) (tea.Cmd, bool) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "enter" {
		if msg := s.Check(strings.TrimSpace(s.Input.Value())); msg != "" {
			s.Error = msg
			s.Input.SetValue("")
			return nil, true
		}
		s.Done, s.Accepted, s.Error = true, true, ""
		return nil, true
	}
	var cmd tea.Cmd
	s.Input, cmd = s.Input.Update(msg)
	return cmd, true
}

// View renders the dialog centered in the area.
func (s *SecretAck) View(t theme.Theme, width, height int) string {
	content := contentWidth(t, width, 86)
	lines := styled(ui.Wrap(s.Title, content), t.Danger)
	if s.Body != "" {
		lines = append(lines, "")
		lines = append(lines, styled(ui.WrapBody(s.Body, content), t.Base)...)
	}
	lines = append(lines, "")
	// Groups on lines of at most four, so the key reads in chunks.
	groups := strings.Split(s.Secret, "-")
	for i := 0; i < len(groups); i += 4 {
		end := min(i+4, len(groups))
		part := strings.Join(groups[i:end], "-")
		if end < len(groups) {
			part += "-"
		}
		lines = append(lines, "  "+t.Command.Render(part))
	}
	lines = append(lines, "")
	lines = append(lines, styled(ui.Wrap(s.Prompt, content), t.Base)...)
	lines = append(lines, s.Input.View())
	if s.Error != "" {
		lines = append(lines, styled(ui.Wrap(s.Error, content), t.Danger)...)
	}
	lines = append(lines, "")
	lines = append(lines, hints(t, content, []ui.KeyHint{{Key: "enter", Desc: "confirm"}})...)
	return box(t, lines, content, width, height)
}
