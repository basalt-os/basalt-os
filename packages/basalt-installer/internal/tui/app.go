// Package tui is the installer's text frontend: a wizard that builds a
// plan, the exact preview of every step, a typed confirmation, progress,
// and the recovery key with its acknowledgement. Built on tui-kit, so it
// looks and behaves like every tui-tools program; it works on a serial
// console (80x24 when the terminal reports no size).
//
// It runs the installation in process, or follows one that the engine
// service runs (an unattended installation from the boot menu, or
// `basalt-installer tui --attach`).
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/client"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/earlyterm"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/tui/kit"
)

// BasaltPalette is the Basalt identity for terminals: ink background, warm
// text, terra roxa accent (the dark tokens of the desktop shell).
func BasaltPalette() theme.Palette {
	return theme.Palette{Name: "basalt", Dark: true,
		Background: "#111418", AltBackground: "#181c21", Foreground: "#ece7e1", MutedForeground: "#a0a6ae",
		Accent: "#d0663f", Selection: "#343b44", Green: "#5fae7b", Red: "#e0605a", Yellow: "#d9a23a",
		Blue: "#7fa7c9", Magenta: "#c58fb0", Cyan: "#8fbfb6", Orange: "#d9a23a"}
}

// Options configure the TUI.
type Options struct {
	Session *session.Session
	Version string
	// ASCII draws borders and bars with ASCII only (serial terminals
	// without UTF-8).
	ASCII bool
	// Attach follows the installation of the engine service on Socket
	// instead of running a wizard. It is chosen by itself when the boot
	// line asks for an unattended installation.
	Attach bool
	Socket string
}

// backend is what the screens after the confirmation need: the session in
// process, or the engine service through its socket.
type backend interface {
	AckRecoveryKey(proof string) error
	Cancel() error
	Finish(ctx context.Context, action string) error
	KeyMedia(ctx context.Context) ([]probe.KeyMedium, error)
	SaveRecoveryKey(ctx context.Context, device string) (string, error)
}

type screen int

const (
	scWelcome screen = iota
	scDisk
	scLayout
	scLayoutSizes
	scSubvols
	scEncryption
	scTang
	scSystem
	scProfile
	scAccounts
	scNetwork
	scStatic
	scRepos
	scRepoURL
	scReview
	scConfirm
	scProgress
	scDone
	scFailed
	scWaiting
)

type model struct {
	opt   Options
	t     theme.Theme
	w, h  int
	scr   screen
	back  []screen
	facts probe.Facts
	p     plan.Plan
	err   string
	note  string // shown on the welcome screen (a plan was loaded, or why not)

	be     backend
	remote *client.Client
	// autoPlan and autoDisk describe the unattended installation followed.
	autoPlan, autoDisk string
	finishAuto         string // the end action the engine service runs by itself
	initCmd            tea.Cmd

	diskPicker kit.RowPicker
	picker     ui.Picker
	form       kit.Form
	checklist  kit.Checklist
	pager      kit.Pager
	confirm    ui.Input
	progress   kit.Progress
	ack        *kit.SecretAck
	media      *kit.RowPicker
	mediaList  []probe.KeyMedium
	quit       *ui.Confirm
	preview    session.Preview

	events     <-chan engine.Event
	stopSub    func()
	started    time.Time
	logPath    string
	result     string
	acked      bool
	pendingKey string // shown when the installation is done (plan writes it to media)
	keyMedia   string // the plan's encryption.recovery_key_media
	saved      []string
}

// Run starts the TUI and returns when the person leaves it.
func Run(opt Options) error {
	t := theme.FromPalette(BasaltPalette())
	if opt.ASCII {
		t.Dialog = t.Dialog.Border(lipgloss.NormalBorder())
	}
	if opt.Socket == "" {
		opt.Socket = client.DefaultSocket
	}
	m := &model{opt: opt, t: t, be: opt.Session}
	if opt.Attach || opt.Session.UnattendedRequested() {
		m.remote = &client.Client{Socket: opt.Socket}
		m.be = m.remote
		c := opt.Session.Cmdline()
		m.autoPlan, m.autoDisk = c["plan"], strings.TrimPrefix(c["confirm"], "/dev/")
		m.scr, m.started = scWaiting, time.Now()
		m.initCmd = tea.Batch(m.pollAuto(0), ui.RunningTick())
	} else {
		s := suggest(opt.Session)
		if s.err != nil {
			return s.err
		}
		m.p, m.facts, m.note = s.p, s.f, s.note
	}
	prog := tea.NewProgram(m, tea.WithAltScreen())
	_, err := prog.Run()
	if m.stopSub != nil {
		m.stopSub()
	}
	return err
}

// suggest loads the starting plan (the boot line's or the media's plan, or
// the defaults) and notes where it came from for the welcome screen.
func suggest(ss *session.Session) suggestedMsg {
	p, f, err := ss.Suggest(context.Background())
	if err != nil {
		return suggestedMsg{err: fmt.Errorf("probing the machine: %w", err)}
	}
	out := suggestedMsg{p: p, f: f}
	switch src, perr := ss.PlanSource(); {
	case src != "" && perr != nil:
		out.note = fmt.Sprintf(i18n.T("The plan %s could not be read (%v). The wizard starts from the defaults."), src, perr)
	case src != "":
		out.note = fmt.Sprintf(i18n.T("Plan loaded from %s. Every screen shows its values; nothing is written until you type the disk name."), src)
	}
	return out
}

type eventMsg engine.Event
type eventsClosed struct{}
type autoMsg struct {
	st  session.Status
	err error
}
type suggestedMsg struct {
	p    plan.Plan
	f    probe.Facts
	note string
	err  error
}
type mediaMsg struct {
	list []probe.KeyMedium
	err  error
}
type savedMsg struct {
	where string
	err   error
}
type keyMsg struct{ key string }
type netDoneMsg struct{}

func (m *model) waitEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return eventsClosed{}
		}
		return eventMsg(e)
	}
}

// pollAuto asks the engine service how the unattended installation goes.
func (m *model) pollAuto(delay time.Duration) tea.Cmd {
	c := m.remote
	return func() tea.Msg {
		time.Sleep(delay)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := c.Wait(ctx); err != nil {
			return autoMsg{err: err}
		}
		st, err := c.Status(ctx)
		return autoMsg{st: st, err: err}
	}
}

func (m *model) Init() tea.Cmd { return m.initCmd }

func (m *model) size() (int, int) {
	w, h := m.w, m.h
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return w, h
}

func (m *model) go_(s screen) {
	m.back = append(m.back, m.scr)
	m.enter(s)
}

func (m *model) goBack() {
	if len(m.back) == 0 {
		return
	}
	s := m.back[len(m.back)-1]
	m.back = m.back[:len(m.back)-1]
	m.enter(s)
}

// Choices of the pickers (translated once; compared by value).
func encLabels() (tpm, recovery, tang, tpmTang, off string) {
	return i18n.T("TPM: opens by itself while Secure Boot is unchanged"),
		i18n.T("Recovery key: typed at every boot"),
		i18n.T("Tang: opens when a Tang server answers"),
		i18n.T("TPM and Tang: both are required"),
		i18n.T("Do not encrypt (not recommended)")
}

func netLabels() (dhcp, static string) {
	return i18n.T("Automatic (DHCP) on every wired interface"), i18n.T("Static address on one interface")
}

func layoutLabels() (auto, manual string) {
	return i18n.T("Automatic: the Basalt layout on the whole disk"), i18n.T("Manual: the sizes and the optional subvolumes")
}

// enter builds the widget of a screen from the current plan.
func (m *model) enter(s screen) {
	m.scr, m.err = s, ""
	p := &m.p
	switch s {
	case scDisk:
		m.diskPicker = kit.RowPicker{Title: "Where should Basalt OS be installed?",
			Body:    "The whole disk is used and erased. Only one local disk is supported here; RAID, multipath and iSCSI installs use the kickstart installer.",
			Columns: []ui.Column{{Title: "Disk", Width: 14}, {Title: "Size", Width: 10}, {Title: "Model", Width: 18, Flex: true}, {Title: "Contents", Width: 22, Flex: true}}}
		for i, d := range m.facts.Disks {
			reason := d.Complex
			if reason == "" {
				reason = d.InUse
			}
			if reason == "" && d.ReadOnly {
				reason = "read-only"
			}
			if reason == "" && d.SizeGiB() < plan.MinDiskGiB {
				reason = fmt.Sprintf("smaller than %d GiB", plan.MinDiskGiB)
			}
			m.diskPicker.Rows = append(m.diskPicker.Rows, kit.Row{Cells: []string{d.Path, probe.HumanSize(d.SizeBytes), d.Model, d.Contents}, Disabled: reason})
			if d.Path == p.Target.Disk {
				m.diskPicker.Cursor = i
			}
		}
	case scLayout:
		auto, manual := layoutLabels()
		cur := auto
		if p.Layout.Mode == "manual" {
			cur = manual
		}
		m.picker = ui.NewPicker(i18n.T("Partitioning (automatic is recommended)"), []string{auto, manual}, cur)
	case scLayoutSizes:
		root := ""
		if p.Layout.RootGiB > 0 {
			root = fmt.Sprint(p.Layout.RootGiB)
		}
		esp, boot, sys := kit.TextField("EFI partition (MiB)", fmt.Sprint(p.Layout.ESPMiB), ""), kit.TextField("/boot (MiB)", fmt.Sprint(p.Layout.BootMiB), ""), kit.TextField("System (GiB)", root, "empty: rest of the disk")
		esp.Validate, boot.Validate, sys.Validate = number, number, number
		sys.Optional = true
		m.form = kit.NewForm("Partition sizes", "The system partition holds the btrfs file system (encrypted unless you turn that off). Leave its size empty to use the rest of the disk.", esp, boot, sys)
	case scSubvols:
		m.checklist = kit.Checklist{Title: "btrfs subvolumes", Body: "Data in its own subvolume survives a system rollback. Required ones cannot be turned off."}
		for _, sv := range plan.Subvolumes() {
			m.checklist.Items = append(m.checklist.Items, kit.CheckItem{Label: fmt.Sprintf("%-20s %s", sv.Name, sv.Mountpoint), Detail: sv.Purpose,
				On: contains(p.Layout.Subvolumes, sv.Name), Locked: sv.Required})
		}
	case scEncryption:
		tpm, recovery, tang, tpmTang, off := encLabels()
		cur := tpm
		switch {
		case !p.Encrypted():
			cur = off
		case p.Encryption.Unlock == "recovery-only":
			cur = recovery
		case p.Encryption.Unlock == "tang":
			cur = tang
		case p.Encryption.Unlock == "tpm2+tang":
			cur = tpmTang
		}
		title := i18n.T("Disk encryption (LUKS2); the TPM is recommended")
		if !m.facts.TPM2 {
			title = i18n.T("Disk encryption (LUKS2); no TPM 2.0 on this machine")
		}
		m.picker = ui.NewPicker(title, []string{tpm, recovery, tang, tpmTang, off}, cur)
	case scTang:
		u, th := kit.TextField("Tang URL", p.Encryption.Tang.URL, "http://tang.example:7500"), kit.TextField("Thumbprint", p.Encryption.Tang.Thumbprint, "tang-show-keys on the server")
		th.Optional = true
		m.form = kit.NewForm("Tang server", "The disk unlocks when this server answers. Without a thumbprint the first answer is trusted.", u, th)
	case scSystem:
		hn, tz := kit.TextField("Host name", p.Hostname, "basalt"), kit.TextField("Time zone", p.Timezone, "Etc/UTC")
		tz.Help = "As in /usr/share/zoneinfo, for example America/Sao_Paulo"
		m.form = kit.NewForm("System", "", hn, tz)
	case scProfile:
		opts := []string{"auto", "minimal", "standard"}
		m.picker = ui.NewPicker(fmt.Sprintf("Package profile (auto picks %s on this machine)", autoProfile(m.facts)), opts, p.Profile)
	case scAccounts:
		m.form = m.accountsForm()
	case scNetwork:
		dhcp, static := netLabels()
		cur := dhcp
		if p.Network.Mode == "static" {
			cur = static
		}
		m.picker = ui.NewPicker("Network", []string{dhcp, static}, cur)
	case scStatic:
		// The detected interface is filled in (the first one when there are
		// several); the hint lists them all.
		name := p.Network.Interface
		if name == "" && len(m.facts.Interfaces) > 0 {
			name = m.facts.Interfaces[0]
		}
		iface := kit.TextField(i18n.T("Interface"), name, strings.Join(m.facts.Interfaces, ", "))
		if len(m.facts.Interfaces) > 1 {
			iface.Help = fmt.Sprintf(i18n.T("Detected: %s"), strings.Join(m.facts.Interfaces, ", "))
		}
		addr, gw, dns := kit.TextField("Address/prefix", p.Network.Address, "192.0.2.10/24"), kit.TextField("Gateway", p.Network.Gateway, ""), kit.TextField("DNS servers", strings.Join(p.Network.DNS, " "), "space separated")
		gw.Optional, dns.Optional = true, true
		m.form = kit.NewForm("Static network", "", iface, addr, gw, dns)
	case scRepos:
		m.checklist = kit.Checklist{Title: "Repositories", Body: "basalt (the distribution) and Fedora are always on. Packages and metadata are signature checked.",
			Items: []kit.CheckItem{
				{Label: "basalt", Detail: "Basalt OS packages: required, carries security and identity updates", On: true, Locked: true},
				{Label: "basalt-tools", Detail: "OpenBasalt tools (Samba Conductor and others): metadata only, nothing installed unless chosen", On: p.Repos.Tools == nil || *p.Repos.Tools},
				{Label: "tui-tools (third party)", Detail: "terminal tools with a command preview, straight from upstream; key fingerprint pinned", On: p.Repos.ThirdParty.TUITools == nil || *p.Repos.ThirdParty.TUITools},
			}}
	case scRepoURL:
		u := kit.TextField("Install from", p.Repos.Basalt.URL, "media")
		u.Help = "\"media\" is the repository on this installer image; or an http(s) URL"
		iu := kit.TextField(i18n.T("Installed system uses"), p.Repos.Basalt.InstalledURL, i18n.T("empty: the default"))
		iu.Optional = true
		iu.Help = fmt.Sprintf(i18n.T("Empty: the URL above when it is http(s), otherwise %s."), plan.DefaultRepoURL)
		m.form = kit.NewForm("Basalt repository", "", u, iu)
	case scReview:
		pv, err := m.opt.Session.MakePreview(context.Background(), m.p)
		m.pager = kit.Pager{Hints: []ui.KeyHint{{Key: "↑/↓", Desc: "scroll"}, {Key: "enter", Desc: "install"}, {Key: "esc", Desc: "back"}, {Key: "q", Desc: "quit"}},
			PassKeys: []string{"enter", "esc", "q", "ctrl+c"}}
		if err != nil {
			m.pager.Title = "The plan has problems"
			m.pager.Text = issuesText(pv.Issues) + "\n" + err.Error()
			m.preview = session.Preview{}
			return
		}
		m.preview = pv
		// The title stays short enough for 80 columns; the plan summary is
		// the first line of the text, where it wraps.
		m.pager.Title = fmt.Sprintf(i18n.N("Review: %d step, exactly what will run", "Review: %d steps, exactly what will run", len(pv.Steps)), len(pv.Steps))
		m.pager.Text = "# " + m.p.Summary() + "\n\n" + issuesText(pv.Issues) + pv.Text
	case scConfirm:
		word := m.preview.ConfirmWord()
		m.confirm = ui.NewInput(fmt.Sprintf("Erase %s and install Basalt OS?", m.preview.Resolved.Disk.Path), "", "")
		m.confirm.Help = fmt.Sprintf("Everything on %s (%s) is destroyed. Type %s to confirm.", m.preview.Resolved.Disk.Path, m.preview.Resolved.Disk.Contents, word)
	}
}

// accountsForm keeps what a plan file set and the form cannot show: a
// password hash stays unless a new password is typed.
func (m *model) accountsForm() kit.Form {
	p := &m.p
	rootKey := kit.TextField("Root SSH key", strings.Join(p.Accounts.Root.SSHKeys, " "), "ssh-ed25519 AAAA... (paste)")
	rootKey.Optional = true
	rootKey.Help = "Root can only log in with a key. Leave empty to keep root locked."
	if p.Accounts.Root.PasswordHash != "" || p.Accounts.Root.Password != "" {
		rootKey.Help = i18n.T("Root also has a password from the plan file (console logins only).")
	}
	user := kit.TextField("Admin user", "", "optional, member of wheel")
	user.Optional = true
	pw1, pw2 := kit.SecretField("Password"), kit.SecretField("Repeat password")
	pw1.Optional, pw2.Optional = true, true
	ukey := kit.TextField("User SSH key", "", "optional")
	ukey.Optional = true
	keptHash := false
	if u := p.Accounts.User; u != nil {
		user.Input.SetValue(u.Name)
		ukey.Input.SetValue(strings.Join(u.SSHKeys, " "))
		if u.PasswordHash != "" && u.Password == "" {
			keptHash = true
			pw1.Help = i18n.T("The plan file sets this password (as a hash): leave both fields empty to keep it, or type a new one.")
		}
	}
	rootHasPassword := p.Accounts.Root.PasswordHash != "" || p.Accounts.Root.Password != ""
	f := kit.NewForm("Accounts", "Give root an SSH key, or create an administrator, or both. SSH accepts keys only.", rootKey, user, pw1, pw2, ukey)
	f.Check = func(v []string) string {
		if v[2] != v[3] {
			return "the passwords do not match"
		}
		if v[1] == "" && (v[2] != "" || v[4] != "") {
			return "give the user a name"
		}
		if v[0] == "" && v[1] == "" && !rootHasPassword {
			return "nobody could log in: add a root key or an administrator"
		}
		if v[1] != "" && v[2] == "" && v[4] == "" && !(keptHash && m.sameUser(v[1])) {
			return i18n.T("the administrator needs a password or an SSH key")
		}
		return ""
	}
	return f
}

// sameUser reports whether name is the plan's user (whose hash is kept).
func (m *model) sameUser(name string) bool {
	return m.p.Accounts.User != nil && m.p.Accounts.User.Name == name
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case autoMsg:
		return m, m.onAuto(msg)
	case suggestedMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		m.p, m.facts, m.note = msg.p, msg.f, strings.TrimSpace(m.note+" "+msg.note)
		m.back = nil
		m.enter(scWelcome)
		return m, nil
	case ui.RunningTickMsg:
		if m.scr == scProgress || m.scr == scWaiting {
			return m, ui.RunningTick()
		}
		return m, nil
	case eventMsg:
		cmd := m.onEvent(engine.Event(msg))
		return m, tea.Batch(m.waitEvent(), cmd)
	case eventsClosed:
		if m.remote != nil && m.scr == scProgress {
			m.err = i18n.T("The connection to the installer engine was lost; reconnecting.")
			return m, m.pollAuto(2 * time.Second)
		}
		return m, nil
	case keyMsg:
		m.showKey(msg.key)
		return m, nil
	case mediaMsg:
		m.onMedia(msg)
		return m, nil
	case savedMsg:
		if m.ack != nil {
			if msg.err != nil {
				m.ack.Error, m.ack.Note = fmt.Sprintf(i18n.T("The key was not saved: %v"), msg.err), ""
			} else {
				m.saved = append(m.saved, msg.where)
				m.ack.Error, m.ack.Note = "", fmt.Sprintf(i18n.T("A copy is on %s. Keep that stick somewhere safe, then type the first group to confirm."), msg.where)
			}
		}
		return m, nil
	case netDoneMsg:
		if f, err := m.opt.Session.Facts(context.Background(), true); err == nil {
			m.facts = f
		}
		return m, nil
	case tea.KeyMsg:
		if m.quit != nil {
			m.quit.Update(msg)
			if m.quit.Done {
				ok := m.quit.Confirmed
				m.quit = nil
				if ok {
					if m.scr == scProgress {
						_ = m.be.Cancel()
						return m, nil
					}
					return m, tea.Quit
				}
			}
			return m, nil
		}
		if m.media != nil {
			m.media.Update(msg)
			if m.media.Done {
				ok, i := m.media.Accepted, m.media.Cursor
				m.media = nil
				if ok && i < len(m.mediaList) {
					dev := m.mediaList[i].Path
					if m.ack != nil {
						m.ack.Note = fmt.Sprintf(i18n.T("Writing the key to %s."), dev)
					}
					be := m.be
					return m, func() tea.Msg {
						where, err := be.SaveRecoveryKey(context.Background(), dev)
						return savedMsg{where, err}
					}
				}
			}
			return m, nil
		}
		if m.ack != nil {
			switch msg.String() {
			case "ctrl+c":
				return m, nil
			case "ctrl+o":
				be := m.be
				m.ack.Error, m.ack.Note = "", i18n.T("Looking for USB sticks.")
				return m, func() tea.Msg {
					list, err := be.KeyMedia(context.Background())
					return mediaMsg{list, err}
				}
			}
			cmd, _ := m.ack.Update(msg)
			if m.ack.Done {
				m.ack, m.acked = nil, true
			}
			return m, cmd
		}
		if msg.String() == "ctrl+c" {
			m.askQuit()
			return m, nil
		}
		return m.onKey(msg)
	}
	// Cursor blink and other messages go to the focused text input.
	switch m.scr {
	case scLayoutSizes, scTang, scSystem, scAccounts, scStatic, scRepoURL:
		cmd, _ := m.form.Update(msg)
		return m, cmd
	case scConfirm:
		cmd, _ := m.confirm.Update(msg)
		return m, cmd
	}
	return m, nil
}

// onAuto follows the unattended installation: wait while the engine
// service prepares it, then show its progress, or start the wizard with a
// note when it was refused.
func (m *model) onAuto(a autoMsg) tea.Cmd {
	if a.err != nil {
		m.err = fmt.Sprintf(i18n.T("The installer engine does not answer (%v); trying again."), a.err)
		return m.pollAuto(3 * time.Second)
	}
	st := a.st
	if st.Disk != "" {
		m.autoDisk = st.Disk
	}
	m.finishAuto = st.Finish
	running := st.State == session.Installing || st.State == session.Succeeded || st.State == session.Failed
	switch {
	case st.Unattended.State == session.AutoRefused:
		return m.toWizard(fmt.Sprintf(i18n.T("The unattended installation did not start: %s"), st.Unattended.Error))
	case running:
	case !st.Unattended.Requested && !m.opt.Attach:
		return m.toWizard("")
	default:
		m.scr = scWaiting
		return m.pollAuto(time.Second)
	}
	// Running, failed or done: follow the engine's events (history first).
	ch, stop, err := m.remote.Subscribe(context.Background())
	if err != nil {
		m.err = err.Error()
		return m.pollAuto(2 * time.Second)
	}
	if m.stopSub != nil {
		m.stopSub()
	}
	m.events, m.stopSub, m.err = ch, stop, ""
	m.scr, m.started = scProgress, time.Now().Add(-time.Duration(st.Seconds)*time.Second)
	m.progress = kit.Progress{Title: fmt.Sprintf(i18n.T("Unattended installation of Basalt OS on %s"), m.autoDisk), TailSize: 12}
	m.saved = st.KeySaved
	cmds := []tea.Cmd{m.waitEvent(), ui.RunningTick()}
	if st.RecoveryKey {
		c := m.remote
		cmds = append(cmds, func() tea.Msg {
			k, _ := c.RecoveryKey(context.Background())
			return keyMsg{k}
		})
	}
	return tea.Batch(cmds...)
}

// toWizard leaves the attached mode for the wizard (the plan prefilled).
func (m *model) toWizard(note string) tea.Cmd {
	m.remote, m.be = nil, m.opt.Session
	if m.stopSub != nil {
		m.stopSub()
		m.stopSub = nil
	}
	m.scr, m.note = scWaiting, note
	ss := m.opt.Session
	return func() tea.Msg { return suggest(ss) }
}

func (m *model) onMedia(msg mediaMsg) {
	if m.ack == nil {
		return
	}
	if msg.err != nil {
		m.ack.Error, m.ack.Note = msg.err.Error(), ""
		return
	}
	if len(msg.list) == 0 {
		m.ack.Note = ""
		m.ack.Error = i18n.T("No USB stick with a FAT, exFAT or ext4 file system was found. Plug one in and press ctrl+o again.")
		return
	}
	m.ack.Note = ""
	m.mediaList = msg.list
	p := kit.RowPicker{Title: i18n.T("Save a copy of the recovery key"),
		Body:    i18n.T("The key is written as a text file to the stick you choose. Keep the stick away from this machine."),
		Columns: []ui.Column{{Title: i18n.T("Device"), Width: 12}, {Title: i18n.T("Size"), Width: 10}, {Title: i18n.T("Label"), Width: 14, Flex: true}, {Title: i18n.T("Model"), Width: 14, Flex: true}}}
	for _, k := range msg.list {
		p.Rows = append(p.Rows, kit.Row{Cells: []string{k.Path, probe.HumanSize(k.Size), k.Label + " (" + k.FSType + ")", k.Model}})
	}
	m.media = &p
}

func (m *model) askQuit() {
	c := ui.Confirm{Title: "Leave the installer?", Body: "Nothing has been written to any disk.", Danger: false}
	switch m.scr {
	case scProgress:
		c = ui.Confirm{Title: "Cancel the installation?", Danger: true,
			Body: "The running step is stopped and what can be undone is undone (mounts, the open encrypted volume, the temporary key). The disk is already partitioned and will not boot."}
	case scDone:
		c.Body = "The system is installed."
	case scWaiting:
		c.Body = i18n.T("The unattended installation goes on in the installer engine; this screen only follows it.")
	}
	m.quit = &c
}

func (m *model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := &m.p
	switch m.scr {
	case scWelcome:
		switch k.String() {
		case "enter":
			m.go_(scDisk)
		case "n":
			if path, err := exec.LookPath("nmtui"); err == nil {
				return m, tea.ExecProcess(exec.Command(path), func(error) tea.Msg { return netDoneMsg{} })
			}
		case "q":
			m.askQuit()
		}
	case scDisk:
		m.diskPicker.Update(k)
		if m.diskPicker.Done {
			ok := m.diskPicker.Accepted
			m.diskPicker.Done = false
			if !ok {
				m.goBack()
				break
			}
			// Choosing the disk on a screen that says it is erased sets the
			// plan's wipe flag; the typed disk name after the review is the
			// confirmation that authorizes it.
			p.Target.Disk, p.Target.Wipe = m.facts.Disks[m.diskPicker.Cursor].Path, true
			m.go_(scLayout)
		}
	case scLayout:
		m.picker.Update(k)
		if m.picker.Done {
			if !m.picker.Accepted {
				m.goBack()
				break
			}
			if _, manual := layoutLabels(); m.picker.Selected() == manual {
				p.Layout.Mode = "manual"
				m.go_(scLayoutSizes)
			} else {
				p.Layout = plan.Layout{Mode: "automatic"}
				p.ApplyDefaults()
				m.go_(scEncryption)
			}
		}
	case scLayoutSizes:
		cmd, _ := m.form.Update(k)
		if m.form.Done {
			if !m.form.Accepted {
				m.goBack()
				break
			}
			v := m.form.Values()
			fmt.Sscan(v[0], &p.Layout.ESPMiB)
			fmt.Sscan(v[1], &p.Layout.BootMiB)
			p.Layout.RootGiB = 0
			if v[2] != "" {
				fmt.Sscan(v[2], &p.Layout.RootGiB)
			}
			m.go_(scSubvols)
		}
		return m, cmd
	case scSubvols:
		m.checklist.Update(k)
		if m.checklist.Done {
			if !m.checklist.Accepted {
				m.goBack()
				break
			}
			p.Layout.Subvolumes = nil
			for i, sv := range plan.Subvolumes() {
				if m.checklist.Items[i].On {
					p.Layout.Subvolumes = append(p.Layout.Subvolumes, sv.Name)
				}
			}
			m.go_(scEncryption)
		}
	case scEncryption:
		m.picker.Update(k)
		if m.picker.Done {
			if !m.picker.Accepted {
				m.goBack()
				break
			}
			tpm, recovery, tang, tpmTang, off := encLabels()
			_ = tpm
			p.Encryption.Enabled = plan.Bool(true)
			p.Encryption.Tang = plan.Tang{}
			switch m.picker.Selected() {
			case off:
				p.Encryption.Enabled = plan.Bool(false)
				m.go_(scSystem)
			case recovery:
				p.Encryption.Unlock = "recovery-only"
				m.go_(scSystem)
			case tang, tpmTang:
				p.Encryption.Unlock = map[string]string{tang: "tang", tpmTang: "tpm2+tang"}[m.picker.Selected()]
				m.go_(scTang)
			default:
				p.Encryption.Unlock = "tpm2"
				m.go_(scSystem)
			}
		}
	case scTang:
		cmd, _ := m.form.Update(k)
		if m.form.Done {
			if !m.form.Accepted {
				m.goBack()
				break
			}
			p.Encryption.Tang = plan.Tang{URL: m.form.Value(0), Thumbprint: m.form.Value(1)}
			m.go_(scSystem)
		}
		return m, cmd
	case scSystem:
		cmd, _ := m.form.Update(k)
		if m.form.Done {
			if !m.form.Accepted {
				m.goBack()
				break
			}
			p.Hostname, p.Timezone = m.form.Value(0), m.form.Value(1)
			m.go_(scProfile)
		}
		return m, cmd
	case scProfile:
		m.picker.Update(k)
		if m.picker.Done {
			if !m.picker.Accepted {
				m.goBack()
				break
			}
			p.Profile = m.picker.Selected()
			m.go_(scAccounts)
		}
	case scAccounts:
		cmd, _ := m.form.Update(k)
		if m.form.Done {
			if !m.form.Accepted {
				m.goBack()
				break
			}
			v := m.form.Values()
			p.Accounts.Root.SSHKeys = splitKeys(v[0])
			old := p.Accounts.User
			p.Accounts.User = nil
			if v[1] != "" {
				u := &plan.User{Name: v[1], Password: v[2], SSHKeys: splitKeys(v[4]), Admin: plan.Bool(true)}
				if old != nil && old.Name == v[1] {
					// What the form does not show stays as the plan set it.
					u.FullName, u.Admin = old.FullName, old.Admin
					if u.Admin == nil {
						u.Admin = plan.Bool(true)
					}
					if v[2] == "" {
						u.PasswordHash = old.PasswordHash
					}
				}
				p.Accounts.User = u
			}
			m.go_(scNetwork)
		}
		return m, cmd
	case scNetwork:
		m.picker.Update(k)
		if m.picker.Done {
			if !m.picker.Accepted {
				m.goBack()
				break
			}
			if _, static := netLabels(); m.picker.Selected() == static {
				p.Network.Mode = "static"
				m.go_(scStatic)
			} else {
				p.Network = plan.Network{Mode: "dhcp"}
				m.go_(scRepos)
			}
		}
	case scStatic:
		cmd, _ := m.form.Update(k)
		if m.form.Done {
			if !m.form.Accepted {
				m.goBack()
				break
			}
			v := m.form.Values()
			p.Network = plan.Network{Mode: "static", Interface: v[0], Address: v[1], Gateway: v[2], DNS: strings.Fields(v[3])}
			m.go_(scRepos)
		}
		return m, cmd
	case scRepos:
		m.checklist.Update(k)
		if m.checklist.Done {
			if !m.checklist.Accepted {
				m.goBack()
				break
			}
			p.Repos.Tools = plan.Bool(m.checklist.Items[1].On)
			p.Repos.ThirdParty.TUITools = plan.Bool(m.checklist.Items[2].On)
			m.go_(scRepoURL)
		}
	case scRepoURL:
		cmd, _ := m.form.Update(k)
		if m.form.Done {
			if !m.form.Accepted {
				m.goBack()
				break
			}
			p.Repos.Basalt.URL, p.Repos.Basalt.InstalledURL = m.form.Value(0), m.form.Value(1)
			m.go_(scReview)
		}
		return m, cmd
	case scReview:
		if m.pager.Update(k) {
			break
		}
		switch k.String() {
		case "enter":
			if m.preview.Token == "" {
				m.err = "the plan has errors: go back and fix them"
				break
			}
			// The person agreed to the wipe by reviewing it; the typed
			// confirmation that follows is what authorizes it.
			m.go_(scConfirm)
		case "esc":
			m.goBack()
		case "q":
			m.askQuit()
		}
	case scConfirm:
		cmd, _ := m.confirm.Update(k)
		if m.confirm.Done {
			if !m.confirm.Accepted {
				m.goBack()
				break
			}
			if m.confirm.Value() != m.preview.ConfirmWord() {
				m.confirm.Done, m.confirm.Accepted = false, false
				m.confirm.Model.SetValue("")
				m.err = fmt.Sprintf("type %s exactly", m.preview.ConfirmWord())
				break
			}
			return m, m.startInstall()
		}
		return m, cmd
	case scProgress, scWaiting:
		// Keys do nothing while the installation runs (ctrl+c asks to cancel).
	case scDone:
		if m.remote != nil {
			// The engine service runs the end action itself.
			if k.String() == "q" {
				return m, tea.Quit
			}
			break
		}
		switch k.String() {
		case "enter":
			if err := m.be.Finish(context.Background(), ""); err != nil {
				m.err = err.Error()
				break
			}
			return m, tea.Quit
		case "q":
			return m, tea.Quit
		}
	case scFailed:
		switch k.String() {
		case "enter":
			if m.remote != nil {
				return m, m.toWizard(i18n.T("The unattended installation failed; its plan is loaded."))
			}
			m.back = nil
			m.enter(scReview)
		case "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *model) startInstall() tea.Cmd {
	ss := m.opt.Session
	m.events, m.stopSub = ss.Subscribe()
	if err := ss.Install(context.Background(), m.preview.Token, m.confirm.Value()); err != nil {
		m.stopSub()
		m.stopSub = nil
		m.err = err.Error()
		m.enter(scReview)
		return nil
	}
	m.scr, m.started = scProgress, time.Now()
	m.keyMedia, m.pendingKey, m.saved = m.p.Encryption.RecoveryKeyMedia, "", nil
	m.progress = kit.Progress{Title: "Installing Basalt OS on " + m.preview.Resolved.Disk.Path, TailSize: 12}
	return tea.Batch(m.waitEvent(), ui.RunningTick())
}

// showKey opens the recovery key dialog.
func (m *model) showKey(key string) {
	if key == "" || m.acked || m.ack != nil {
		return
	}
	first, _, _ := strings.Cut(key, "-")
	be := m.be
	a := kit.NewSecretAck(i18n.T("Recovery key: write it down now"),
		i18n.T("This key opens the disk when the TPM refuses (Secure Boot changed, the disk moved to another machine). It is shown only this once and is not stored on any disk unless you save a copy to a USB stick (ctrl+o). Keep it off this machine."),
		key, fmt.Sprintf(i18n.T("Type the first group (%d characters) to confirm you stored it:"), len(first)),
		func(answer string) string {
			if err := be.AckRecoveryKey(answer); err != nil {
				return err.Error()
			}
			return ""
		})
	a.Hints = []ui.KeyHint{{Key: "ctrl+o", Desc: i18n.T("save to a USB stick")}}
	if len(m.saved) > 0 {
		a.Note = fmt.Sprintf(i18n.T("A copy is on %s. Keep that stick somewhere safe, then type the first group to confirm."), m.saved[len(m.saved)-1])
	}
	m.ack = &a
}

func (m *model) onEvent(e engine.Event) tea.Cmd {
	switch e.Type {
	case engine.EvStarted:
		m.logPath = e.LogPath
	case engine.EvStepStart:
		m.progress.Step = fmt.Sprintf("Step %d of %d: %s", e.Index, e.Total, e.Title)
		m.progress.Command = e.Command
	case engine.EvOutput:
		m.progress.Add(e.Text)
	case engine.EvProgress:
		m.progress.Fraction = e.Fraction
	case engine.EvWarning:
		m.progress.Add("warning: " + e.Text)
	case engine.EvRollback:
		if e.Command != "" {
			m.progress.Add("rollback: " + e.Command)
		} else {
			m.progress.Add(e.Text)
		}
	case engine.EvSecret:
		if e.Kind == engine.SecretRecKey && e.Secret != "" {
			if m.keyMedia != "" {
				// The plan writes the key to removable media when the
				// installation is done; ask only if that fails.
				m.pendingKey = e.Secret
				break
			}
			m.showKey(e.Secret)
		}
	case session.EvKeySaved:
		m.saved = append(m.saved, e.Text)
		m.progress.Add(fmt.Sprintf(i18n.T("recovery key written to %s"), e.Text))
	case session.EvKeyAcked:
		m.ack, m.media, m.acked, m.pendingKey = nil, nil, true, ""
	case engine.EvDone:
		m.logPath = e.LogPath
		if e.OK {
			m.scr = scDone
			m.progress.Fraction = 1
			if m.pendingKey != "" && !m.acked {
				m.showKey(m.pendingKey)
			}
			m.pendingKey = ""
		} else {
			// A recovery key of a rolled-back volume opens nothing.
			m.scr, m.result, m.ack, m.media, m.pendingKey = scFailed, e.Error, nil, nil, ""
		}
	}
	return nil
}

func (m *model) View() string {
	w, h := m.size()
	t := m.t
	subtitle := map[screen]string{scWelcome: "welcome", scDisk: "disk", scLayout: "layout", scLayoutSizes: "layout", scSubvols: "layout",
		scEncryption: "encryption", scTang: "encryption", scSystem: "system", scProfile: "packages", scAccounts: "accounts",
		scNetwork: "network", scStatic: "network", scRepos: "repositories", scRepoURL: "repositories", scReview: "review",
		scConfirm: "confirm", scProgress: "installing", scDone: "done", scFailed: "failed", scWaiting: "unattended"}[m.scr]
	f := m.facts
	var facts []ui.Fact
	if m.remote == nil {
		tpm := "none"
		if f.TPM2 {
			tpm = "2.0"
		}
		machine := f.Virt
		if machine == "" {
			machine = "bare metal"
		}
		facts = []ui.Fact{
			{Label: "firmware", Value: map[bool]string{true: "UEFI", false: "BIOS"}[f.UEFI]},
			{Label: "Secure Boot", Value: f.SecureBoot}, {Label: "TPM", Value: tpm}, {Label: "machine", Value: machine},
			{Label: "memory", Value: fmt.Sprintf("%d MiB", f.MemoryMiB)}}
	} else {
		facts = []ui.Fact{{Label: i18n.T("plan"), Value: ui.Truncate(m.autoPlan, max(w-24, 10))}, {Label: i18n.T("disk"), Value: m.autoDisk}}
	}
	head := ui.Header{Title: "Basalt OS installer " + m.opt.Version, Subtitle: subtitle, Facts: facts}.Render(t, w)
	status := ui.StatusLine(t, ui.StatusError, m.err, m.statusHint(), w)
	bodyH := max(h-lipgloss.Height(head)-lipgloss.Height(status), 5)

	var body string
	switch m.scr {
	case scWelcome:
		width := min(78, max(w-4, 20))
		lines := []string{t.Title.Render(i18n.T("Welcome")), ""}
		// One translatable sentence, wrapped for the screen.
		lines = append(lines, wrap(i18n.T("This installs Basalt OS, a Fedora remix with server and desktop editions: SELinux enforcing, btrfs with snapshots before every update, LUKS2 disk encryption unlocked by the TPM, and a local assistant that only proposes changes."), width)...)
		lines = append(lines, "",
			"You choose a disk and a few settings, then review the exact list of commands",
			"before anything is written. Nothing changes until you type the disk name.", "")
		if m.note != "" {
			for _, l := range wrap(m.note, width) {
				lines = append(lines, t.Accent.Render(l))
			}
			lines = append(lines, "")
		}
		lines = append(lines, t.Muted.Render("Needs the network for the Fedora packages. Logs: /var/log/basalt-installer."), "")
		keys := t.Key.Render("enter") + t.KeyDesc.Render(" "+i18n.T("start")+"    ")
		if _, err := exec.LookPath("nmtui"); err == nil {
			keys += t.Key.Render("n") + t.KeyDesc.Render(" "+i18n.T("network and Wi-Fi")+"    ")
		}
		keys += t.Key.Render("q") + t.KeyDesc.Render(" "+i18n.T("quit"))
		lines = append(lines, keys)
		body = center(lines, w, bodyH)
	case scWaiting:
		width := min(78, max(w-4, 20))
		lines := []string{t.Title.Render(i18n.T("Unattended installation")), ""}
		lines = append(lines, wrap(fmt.Sprintf(i18n.T("The boot line names a plan (%s) and the disk it erases (%s). The installer engine reads the plan and starts on its own; this screen follows it."), m.autoPlan, m.autoDisk), width)...)
		lines = append(lines, "", t.Muted.Render(ui.RunningMessage(i18n.T("Preparing"), time.Since(m.started))))
		body = center(lines, w, bodyH)
	case scDisk:
		body = m.diskPicker.View(t, w, bodyH)
	case scLayout, scEncryption, scProfile, scNetwork:
		body = m.picker.View(t, w, bodyH)
	case scLayoutSizes, scTang, scSystem, scAccounts, scStatic, scRepoURL:
		body = m.form.View(t, w, bodyH)
	case scSubvols, scRepos:
		body = m.checklist.View(t, w, bodyH)
	case scReview:
		body = m.pager.View(t, w, bodyH)
	case scConfirm:
		body = m.confirm.View(t, w, bodyH)
	case scProgress:
		m.progress.Status = ui.RunningMessage("Installing", time.Since(m.started)) + "   log: " + m.logPath
		body = m.progress.View(t, w, bodyH, m.opt.ASCII)
	case scDone:
		body = m.doneView(w, bodyH)
	case scFailed:
		body = m.failedView(w, bodyH)
	}
	if m.ack != nil {
		body = m.ack.View(t, w, bodyH)
	}
	if m.media != nil {
		body = m.media.View(t, w, bodyH)
	}
	if m.quit != nil {
		body = m.quit.View(t, w, bodyH)
	}
	body = lipgloss.NewStyle().Height(bodyH).MaxHeight(bodyH).Render(body)
	return head + "\n" + body + "\n" + status
}

func (m *model) doneView(w, h int) string {
	t := m.t
	width := min(78, max(w-4, 20))
	disk := m.preview.Resolved.Disk.Path
	if m.remote != nil {
		disk = m.autoDisk
	}
	lines := []string{t.OK.Render(fmt.Sprintf(i18n.T("Basalt OS is installed on %s"), disk)), ""}
	lines = append(lines, wrap(i18n.T("Install record on the new system: /var/log/basalt-installer/ (plan, summary, log). The first boot takes the first snapshot, and the disk unlocks by itself while Secure Boot is unchanged. Keep the recovery key off this machine."), width)...)
	for _, s := range m.saved {
		lines = append(lines, "")
		lines = append(lines, wrap(fmt.Sprintf(i18n.T("A copy of the recovery key is on %s."), s), width)...)
	}
	lines = append(lines, "")
	fin := m.p.Finish
	if m.remote != nil {
		fin = m.finishAuto
	}
	switch {
	case m.remote != nil && fin != "none" && fin != "":
		lines = append(lines, t.Muted.Render(i18n.T("The installer engine runs the plan's end action now.")))
	case m.remote != nil:
		lines = append(lines, t.Key.Render("q")+t.KeyDesc.Render(" "+i18n.T("leave")))
	case fin != "none":
		lines = append(lines, t.Key.Render("enter")+t.KeyDesc.Render(" "+fin+" now    ")+t.Key.Render("q")+t.KeyDesc.Render(" stay in the installer"))
	default:
		lines = append(lines, t.Key.Render("q")+t.KeyDesc.Render(" leave"))
	}
	return center(lines, w, h)
}

// failedView fits an 80x24 console: the error on a few lines, where the
// log is, and as much of the last output as there is room for.
func (m *model) failedView(w, h int) string {
	t := m.t
	width := max(w-4, 20)
	errLines := wrap(strings.Join(strings.Fields(m.result), " "), width)
	if len(errLines) > 4 {
		// The whole message is in the log.
		errLines = errLines[:4]
	}
	lines := []string{t.Danger.Render(i18n.T("The installation failed"))}
	for _, l := range errLines {
		lines = append(lines, t.Base.Render(l))
	}
	lines = append(lines, "")
	for _, l := range wrap(fmt.Sprintf(i18n.T("What could be undone was rolled back. Log: %s"), m.logPath), width) {
		lines = append(lines, t.Muted.Render(l))
	}
	keys := t.Key.Render("enter") + t.KeyDesc.Render(" "+i18n.T("back to the review")+"    ") + t.Key.Render("q") + t.KeyDesc.Render(" "+i18n.T("quit"))
	if m.remote != nil {
		keys = t.Key.Render("enter") + t.KeyDesc.Render(" "+i18n.T("open the wizard with this plan")+"    ") + t.Key.Render("q") + t.KeyDesc.Render(" "+i18n.T("quit"))
	}
	room := h - len(lines) - 3
	tail := m.progress.Tail
	if room < 1 {
		tail = nil
	} else if len(tail) > room {
		tail = tail[len(tail)-room:]
	}
	if len(tail) > 0 {
		lines = append(lines, "")
		for _, l := range tail {
			lines = append(lines, t.Muted.Render(ui.Truncate(l, width)))
		}
	}
	lines = append(lines, "", keys)
	return lipgloss.NewStyle().Padding(0, 2).Render(strings.Join(lines, "\n"))
}

// wrap breaks a text into lines of at most width cells.
func wrap(s string, width int) []string {
	return strings.Split(lipgloss.NewStyle().Width(width).Render(s), "\n")
}

// center places a left-aligned block of lines in the middle of the area.
func center(lines []string, w, h int) string {
	block := lipgloss.NewStyle().Align(lipgloss.Left).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, block)
}

func (m *model) statusHint() string {
	switch {
	case m.ack != nil:
		return i18n.T("Write the key down, then type its first group.")
	case m.scr == scReview:
		return i18n.T("Every command and file is listed. enter: confirm")
	case m.scr == scProgress && m.remote != nil:
		return i18n.T("Unattended installation; ctrl+c cancels it")
	case m.scr == scProgress:
		return "ctrl+c cancels and rolls back"
	case m.scr == scWaiting:
		return i18n.T("Waiting for the installer engine")
	case m.scr == scDone || m.scr == scFailed:
		return ""
	}
	return "esc goes back; ctrl+c leaves the installer"
}

func issuesText(is plan.Issues) string {
	if len(is) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("== Read first\n")
	for _, i := range is {
		fmt.Fprintf(&b, "  %s: %s: %s\n", i.Severity, i.Field, i.Message)
	}
	return b.String() + "\n"
}

func splitKeys(s string) []string {
	// One key per field: keys contain spaces (type, base64, comment), so a
	// second key is recognized by its type prefix.
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var keys []string
	var cur []string
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "ssh-") || strings.HasPrefix(f, "ecdsa-") || strings.HasPrefix(f, "sk-") {
			if len(cur) > 0 {
				keys = append(keys, strings.Join(cur, " "))
			}
			cur = []string{f}
			continue
		}
		cur = append(cur, f)
	}
	if len(cur) > 0 {
		keys = append(keys, strings.Join(cur, " "))
	}
	return keys
}

func number(v string) string {
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil || n <= 0 || fmt.Sprint(n) != v {
		return "a positive whole number"
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func autoProfile(f probe.Facts) string {
	if f.Virt != "" {
		return "minimal"
	}
	return "standard"
}

// IsSerial reports whether the terminal is a serial line (ttyS*, ttyAMA*,
// hvc*), where the TUI falls back to ASCII borders.
func IsSerial() bool { return earlyterm.Serial() }
