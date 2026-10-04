// Package tui is the installer's text frontend: a wizard that builds a
// plan, the exact preview of every step, a typed confirmation, progress,
// and the recovery key with its acknowledgement. Built on tui-kit, so it
// looks and behaves like every tui-tools program; it works on a serial
// console (80x24 when the terminal reports no size).
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/theme"
	"github.com/tui-tools/tui-kit/ui"

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

	diskPicker kit.RowPicker
	picker     ui.Picker
	form       kit.Form
	checklist  kit.Checklist
	pager      kit.Pager
	confirm    ui.Input
	progress   kit.Progress
	ack        *kit.SecretAck
	quit       *ui.Confirm
	preview    session.Preview

	events  <-chan engine.Event
	stopSub func()
	started time.Time
	logPath string
	result  string
	acked   bool
	output  []string
}

// Run starts the TUI and returns when the person leaves it.
func Run(opt Options) error {
	t := theme.FromPalette(BasaltPalette())
	if opt.ASCII {
		t.Dialog = t.Dialog.Border(lipgloss.NormalBorder())
	}
	m := &model{opt: opt, t: t}
	p, f, err := opt.Session.Suggest(context.Background())
	if err != nil {
		return fmt.Errorf("probing the machine: %w", err)
	}
	m.p, m.facts = p, f
	prog := tea.NewProgram(m, tea.WithAltScreen())
	_, err = prog.Run()
	if m.stopSub != nil {
		m.stopSub()
	}
	return err
}

type eventMsg engine.Event
type eventsClosed struct{}

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

func (m *model) Init() tea.Cmd { return nil }

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
		m.picker = ui.NewPicker("Partitioning", []string{
			"Automatic: the Basalt layout on the whole disk (recommended)",
			"Manual: choose the sizes and the optional subvolumes"},
			map[string]string{"automatic": "Automatic: the Basalt layout on the whole disk (recommended)", "manual": "Manual: choose the sizes and the optional subvolumes"}[p.Layout.Mode])
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
		opts := []string{encTPM, encRecovery, encTang, encTPMTang, encOff}
		cur := encTPM
		switch {
		case !p.Encrypted():
			cur = encOff
		case p.Encryption.Unlock == "recovery-only":
			cur = encRecovery
		case p.Encryption.Unlock == "tang":
			cur = encTang
		case p.Encryption.Unlock == "tpm2+tang":
			cur = encTPMTang
		}
		title := "Disk encryption (LUKS2)"
		if !m.facts.TPM2 {
			title += ": no TPM 2.0 found on this machine"
		}
		m.picker = ui.NewPicker(title, opts, cur)
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
		rootKey := kit.TextField("Root SSH key", strings.Join(p.Accounts.Root.SSHKeys, " "), "ssh-ed25519 AAAA... (paste)")
		rootKey.Optional = true
		rootKey.Help = "Root can only log in with a key. Leave empty to keep root locked."
		user := kit.TextField("Admin user", "", "optional, member of wheel")
		user.Optional = true
		pw1, pw2 := kit.SecretField("Password"), kit.SecretField("Repeat password")
		pw1.Optional, pw2.Optional = true, true
		ukey := kit.TextField("User SSH key", "", "optional")
		ukey.Optional = true
		if u := p.Accounts.User; u != nil {
			user.Input.SetValue(u.Name)
			ukey.Input.SetValue(strings.Join(u.SSHKeys, " "))
		}
		m.form = kit.NewForm("Accounts", "Give root an SSH key, or create an administrator, or both. SSH accepts keys only.", rootKey, user, pw1, pw2, ukey)
		m.form.Check = func(v []string) string {
			if v[2] != v[3] {
				return "the passwords do not match"
			}
			if v[1] == "" && (v[2] != "" || v[4] != "") {
				return "give the user a name"
			}
			if v[0] == "" && v[1] == "" {
				return "nobody could log in: add a root key or an administrator"
			}
			return ""
		}
	case scNetwork:
		m.picker = ui.NewPicker("Network", []string{netDHCP, netStatic}, map[string]string{"dhcp": netDHCP, "static": netStatic}[p.Network.Mode])
	case scStatic:
		iface := kit.TextField("Interface", p.Network.Interface, strings.Join(m.facts.Interfaces, ", "))
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
		iu := kit.TextField("Installed system uses", p.Repos.Basalt.InstalledURL, "default: the URL above when it is http(s)")
		iu.Optional = true
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
		m.pager.Title = fmt.Sprintf("Review: %d steps, exactly what will run (%s)", len(pv.Steps), m.p.Summary())
		m.pager.Text = issuesText(pv.Issues) + pv.Text
	case scConfirm:
		word := m.preview.ConfirmWord()
		m.confirm = ui.NewInput(fmt.Sprintf("Erase %s and install Basalt OS?", m.preview.Resolved.Disk.Path), "", "")
		m.confirm.Help = fmt.Sprintf("Everything on %s (%s) is destroyed. Type %s to confirm.", m.preview.Resolved.Disk.Path, m.preview.Resolved.Disk.Contents, word)
	}
}

const (
	encTPM      = "Encrypt, unlock with the TPM (Secure Boot state, PCR 7) (recommended)"
	encRecovery = "Encrypt, unlock with the recovery key at every boot"
	encTang     = "Encrypt, unlock from a Tang server on the network"
	encTPMTang  = "Encrypt, unlock with the TPM and a Tang server, both required"
	encOff      = "Do not encrypt (not recommended)"
	netDHCP     = "Automatic (DHCP) on every wired interface"
	netStatic   = "Static address on one interface"
)

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case ui.RunningTickMsg:
		if m.scr == scProgress {
			return m, ui.RunningTick()
		}
		return m, nil
	case eventMsg:
		m.onEvent(engine.Event(msg))
		return m, m.waitEvent()
	case eventsClosed:
		return m, nil
	case tea.KeyMsg:
		if m.quit != nil {
			m.quit.Update(msg)
			if m.quit.Done {
				ok := m.quit.Confirmed
				m.quit = nil
				if ok {
					if m.scr == scProgress {
						_ = m.opt.Session.Cancel()
						return m, nil
					}
					return m, tea.Quit
				}
			}
			return m, nil
		}
		if m.ack != nil {
			if msg.String() == "ctrl+c" {
				return m, nil
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

func (m *model) askQuit() {
	c := ui.Confirm{Title: "Leave the installer?", Body: "Nothing has been written to any disk.", Danger: false}
	switch m.scr {
	case scProgress:
		c = ui.Confirm{Title: "Cancel the installation?", Danger: true,
			Body: "The running step is stopped and what can be undone is undone (mounts, the open encrypted volume, the temporary key). The disk is already partitioned and will not boot."}
	case scDone:
		c.Body = "The system is installed."
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
			if strings.HasPrefix(m.picker.Selected(), "Manual") {
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
			p.Encryption.Enabled = plan.Bool(true)
			p.Encryption.Tang = plan.Tang{}
			switch m.picker.Selected() {
			case encOff:
				p.Encryption.Enabled = plan.Bool(false)
				m.go_(scSystem)
			case encRecovery:
				p.Encryption.Unlock = "recovery-only"
				m.go_(scSystem)
			case encTang, encTPMTang:
				p.Encryption.Unlock = map[string]string{encTang: "tang", encTPMTang: "tpm2+tang"}[m.picker.Selected()]
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
			p.Accounts.User = nil
			if v[1] != "" {
				p.Accounts.User = &plan.User{Name: v[1], Password: v[2], SSHKeys: splitKeys(v[4]), Admin: plan.Bool(true)}
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
			if m.picker.Selected() == netStatic {
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
	case scProgress:
		// Keys do nothing while the installation runs (ctrl+c asks to cancel).
	case scDone:
		switch k.String() {
		case "enter":
			if err := m.opt.Session.Finish(context.Background(), ""); err != nil {
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
	m.progress = kit.Progress{Title: "Installing Basalt OS on " + m.preview.Resolved.Disk.Path, TailSize: 12}
	return tea.Batch(m.waitEvent(), ui.RunningTick())
}

func (m *model) onEvent(e engine.Event) {
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
		if e.Kind == engine.SecretRecKey {
			key := e.Secret
			first, _, _ := strings.Cut(key, "-")
			a := kit.NewSecretAck("Recovery key: write it down now",
				"This key opens the disk when the TPM refuses (Secure Boot changed, the disk moved to another machine). It is shown only this once and is not stored anywhere. Keep it off this machine.",
				key, fmt.Sprintf("Type the first group (%d characters) to confirm you stored it:", len(first)),
				func(answer string) string {
					if err := m.opt.Session.AckRecoveryKey(answer); err != nil {
						return err.Error()
					}
					return ""
				})
			m.ack = &a
		}
	case engine.EvDone:
		m.logPath = e.LogPath
		if e.OK {
			m.scr = scDone
			m.progress.Fraction = 1
		} else {
			// A recovery key of a rolled-back volume opens nothing.
			m.scr, m.result, m.ack = scFailed, e.Error, nil
		}
	}
}

func (m *model) View() string {
	w, h := m.size()
	t := m.t
	subtitle := map[screen]string{scWelcome: "welcome", scDisk: "disk", scLayout: "layout", scLayoutSizes: "layout", scSubvols: "layout",
		scEncryption: "encryption", scTang: "encryption", scSystem: "system", scProfile: "packages", scAccounts: "accounts",
		scNetwork: "network", scStatic: "network", scRepos: "repositories", scRepoURL: "repositories", scReview: "review",
		scConfirm: "confirm", scProgress: "installing", scDone: "done", scFailed: "failed"}[m.scr]
	f := m.facts
	tpm := "none"
	if f.TPM2 {
		tpm = "2.0"
	}
	machine := f.Virt
	if machine == "" {
		machine = "bare metal"
	}
	head := ui.Header{Title: "Basalt OS installer " + m.opt.Version, Subtitle: subtitle, Facts: []ui.Fact{
		{Label: "firmware", Value: map[bool]string{true: "UEFI", false: "BIOS"}[f.UEFI]},
		{Label: "Secure Boot", Value: f.SecureBoot}, {Label: "TPM", Value: tpm}, {Label: "machine", Value: machine},
		{Label: "memory", Value: fmt.Sprintf("%d MiB", f.MemoryMiB)}}}.Render(t, w)
	status := ui.StatusLine(t, ui.StatusError, m.err, statusHint(m.scr), w)
	bodyH := max(h-lipgloss.Height(head)-lipgloss.Height(status), 5)

	var body string
	switch m.scr {
	case scWelcome:
		lines := []string{t.Title.Render("Welcome"), ""}
		// One translatable sentence, wrapped for the screen.
		lines = append(lines, strings.Split(lipgloss.NewStyle().Width(min(78, max(w-4, 20))).Render(
			i18n.T("This installs Basalt OS, a Fedora remix with server and desktop editions: SELinux enforcing, btrfs with snapshots before every update, LUKS2 disk encryption unlocked by the TPM, and a local assistant that only proposes changes.")), "\n")...)
		lines = append(lines, "",
			"You choose a disk and a few settings, then review the exact list of commands",
			"before anything is written. Nothing changes until you type the disk name.", "",
			t.Muted.Render("Needs the network for the Fedora packages. Logs: /var/log/basalt-installer."), "",
			t.Key.Render("enter")+t.KeyDesc.Render(" start    ")+t.Key.Render("q")+t.KeyDesc.Render(" quit"))
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
		lines := []string{t.OK.Render("Basalt OS is installed on " + m.preview.Resolved.Disk.Path), "",
			// Wrapped by hand to fit an 80 column serial console, like the welcome text.
			"Install record on the new system: /var/log/basalt-installer/ (plan, summary,",
			"log). The first boot takes the first snapshot and the disk unlocks by itself",
			"while Secure Boot is unchanged. Keep the recovery key off this machine.", ""}
		if fin := m.p.Finish; fin != "none" {
			lines = append(lines, t.Key.Render("enter")+t.KeyDesc.Render(" "+fin+" now    ")+t.Key.Render("q")+t.KeyDesc.Render(" stay in the installer"))
		} else {
			lines = append(lines, t.Key.Render("q")+t.KeyDesc.Render(" leave"))
		}
		body = center(lines, w, bodyH)
	case scFailed:
		lines := []string{t.Danger.Render("The installation failed")}
		for _, l := range ui.Wrap(m.result, min(w-8, 90)) {
			lines = append(lines, t.Base.Render(l))
		}
		lines = append(lines, "", t.Muted.Render("Rolled back what could be undone. Log: "+m.logPath), "")
		for _, l := range m.progress.Tail {
			lines = append(lines, t.Muted.Render(ui.Truncate(l, min(w-8, 110))))
		}
		lines = append(lines, "", t.Key.Render("enter")+t.KeyDesc.Render(" back to the review    ")+t.Key.Render("q")+t.KeyDesc.Render(" quit"))
		body = center(lines, w, bodyH)
	}
	if m.ack != nil {
		body = m.ack.View(t, w, bodyH)
	}
	if m.quit != nil {
		body = m.quit.View(t, w, bodyH)
	}
	body = lipgloss.NewStyle().Height(bodyH).MaxHeight(bodyH).Render(body)
	return head + "\n" + body + "\n" + status
}

// center places a left-aligned block of lines in the middle of the area.
func center(lines []string, w, h int) string {
	block := lipgloss.NewStyle().Align(lipgloss.Left).Render(strings.Join(lines, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, block)
}

func statusHint(s screen) string {
	switch s {
	case scReview:
		return "Read the plan: every command and file is listed. enter continues to the confirmation."
	case scProgress:
		return "ctrl+c cancels and rolls back"
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
func IsSerial() bool {
	name, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		return false
	}
	for _, p := range []string{"/dev/ttyS", "/dev/ttyAMA", "/dev/hvc", "/dev/ttyUSB"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
