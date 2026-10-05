package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/tui-tools/tui-kit/ui"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/tui/kit"
)

// Screens of the storage choices (ADR 0002 addendum, 2026-10-04): the use
// of a disk that holds other systems, where /home lives (a new one, or an
// existing partition that is adopted, never formatted), and what is
// encrypted.

// diskUse rows.
const (
	useErase = iota
	useFree
	useFreeSharedESP
)

type inspectedMsg struct {
	res session.HomeInspection
	err error
}

type candidatesMsg struct {
	list []probe.Partition
	err  error
}

func (m *model) targetDisk() probe.Disk {
	d, _ := m.facts.Disk(m.p.Target.Disk)
	return d
}

// afterDisk goes on from the disk screen: a disk with partitions offers to
// keep them; an empty one is erased.
func (m *model) afterDisk() tea.Cmd {
	d := m.targetDisk()
	if d.Table == "gpt" && len(d.Partitions) > 0 {
		m.go_(scDiskUse)
		return nil
	}
	m.p.Target.Wipe, m.p.Target.ESP = true, ""
	return m.toHome()
}

// toHome opens the /home screen when there are partitions to adopt.
func (m *model) toHome() tea.Cmd {
	ss := m.opt.Session
	return func() tea.Msg {
		list, err := ss.HomeCandidates(context.Background())
		return candidatesMsg{list, err}
	}
}

func (m *model) onCandidates(c candidatesMsg) {
	m.homeCands = nil
	for _, p := range c.list {
		if p.Disk == m.p.Target.Disk && m.p.Target.Wipe {
			continue
		}
		kept := false
		for _, np := range m.targetDisk().Partitions {
			kept = kept || np.Path == p.Path
		}
		if p.Disk == m.p.Target.Disk && !kept {
			continue
		}
		m.homeCands = append(m.homeCands, p)
	}
	if len(m.homeCands) == 0 {
		m.p.Home.Existing = nil
		m.go_(scLayout)
		return
	}
	m.go_(scHome)
}

func homeLabels() (fresh, existing string) {
	return i18n.T("A new /home on this disk"), i18n.T("An existing /home partition: keep its files")
}

func scopeLabels() (system, home, none string) {
	return i18n.T("The whole system (recommended)"), i18n.T("Only /home: the system, logs and /var stay unencrypted"),
		i18n.T("Nothing (servers in a controlled place)")
}

func tpmHomeLabels() (pass, tpm string) {
	return i18n.T("Ask for its passphrase at every boot"), i18n.T("Also open it with this machine's TPM (passphrase kept)")
}

// enterStorage builds the widgets of the storage screens.
func (m *model) enterStorage(s screen) {
	p := &m.p
	switch s {
	case scDiskUse:
		d := m.targetDisk()
		var kept []string
		for _, part := range d.Partitions {
			desc := fmt.Sprintf("%s %s", part.Path, probe.HumanSize(part.Bytes))
			if n := probe.TypeName(part.Type); n != "" {
				desc += " " + n
			} else if part.FSType != "" {
				desc += " " + part.FSType
			}
			kept = append(kept, desc)
		}
		free, _ := d.LargestFree()
		need := plan.NeededBytes(*p)
		m.diskUse = kit.RowPicker{Title: fmt.Sprintf(i18n.T("%s already holds partitions"), d.Path),
			Body:    fmt.Sprintf(i18n.T("On it: %s. Basalt OS can erase the whole disk, or go into its free space and leave every other partition as it is (dual boot)."), strings.Join(kept, ", ")),
			Columns: []ui.Column{{Title: i18n.T("Use of the disk"), Width: 40, Flex: true}, {Title: i18n.T("Space"), Width: 12}}}
		noRoom := ""
		if free.Bytes < need {
			noRoom = fmt.Sprintf(i18n.T("needs %s of free space"), probe.HumanSize(need))
		}
		m.diskUse.Rows = []kit.Row{
			{Cells: []string{i18n.T("Erase the whole disk"), probe.HumanSize(d.SizeBytes)}},
			{Cells: []string{i18n.T("Keep the partitions, use the free space"), probe.HumanSize(free.Bytes)}, Disabled: noRoom},
		}
		if esp := existingESP(d); esp != "" {
			m.diskUse.Rows = append(m.diskUse.Rows, kit.Row{Cells: []string{fmt.Sprintf(i18n.T("Keep the partitions, share the EFI partition %s"), esp), probe.HumanSize(free.Bytes)}, Disabled: noRoom})
		}
		switch {
		case !p.Target.Wipe && p.Target.ESP != "":
			m.diskUse.Cursor = useFreeSharedESP
		case !p.Target.Wipe:
			m.diskUse.Cursor = useFree
		}
	case scHome:
		fresh, existing := homeLabels()
		cur := fresh
		if p.Home.Existing != nil {
			cur = existing
		}
		m.picker = ui.NewPicker(i18n.T("Where /home (the users' files) lives"), []string{fresh, existing}, cur)
	case scHomePick:
		m.homePick = kit.RowPicker{Title: i18n.T("Which partition holds the existing /home?"),
			Body:    i18n.T("It is never formatted: it is opened, mounted on /home and set up to open at boot. Only the new user's directory is created on it."),
			Columns: []ui.Column{{Title: i18n.T("Partition"), Width: 16}, {Title: i18n.T("Size"), Width: 10}, {Title: i18n.T("Contents"), Width: 14}, {Title: i18n.T("Label"), Width: 12, Flex: true}}}
		for i, c := range m.homeCands {
			m.homePick.Rows = append(m.homePick.Rows, kit.Row{Cells: []string{c.Path, probe.HumanSize(c.Bytes), c.FSType, c.Label}})
			if p.Home.Existing != nil && p.Home.Existing.Device == c.Path {
				m.homePick.Cursor = i
			}
		}
	case scHomePass:
		pw := kit.SecretField(i18n.T("Passphrase"))
		pw.Help = i18n.T("The passphrase this partition opens with today. It is checked now and never stored.")
		m.form = kit.NewForm(fmt.Sprintf(i18n.T("Open %s"), m.homeDev), "", pw)
	case scHomeOwner:
		m.homeOwner = kit.RowPicker{Title: i18n.T("Whose home is it?"),
			Body:    i18n.T("The new user gets the same name, uid and gid as these files, so they stay theirs."),
			Columns: []ui.Column{{Title: i18n.T("Directory"), Width: 24, Flex: true}, {Title: "uid", Width: 8}, {Title: "gid", Width: 8}}}
		for _, o := range m.inspection.Owners {
			m.homeOwner.Rows = append(m.homeOwner.Rows, kit.Row{Cells: []string{"/home/" + o.Name, fmt.Sprint(o.UID), fmt.Sprint(o.GID)}})
		}
		m.homeOwner.Rows = append(m.homeOwner.Rows, kit.Row{Cells: []string{i18n.T("None of these: a new user"), "", ""}})
	case scHomeUser:
		name, home := "", ""
		if u := p.Accounts.User; u != nil {
			name, home = u.Name, u.HomeDir
		}
		n := kit.TextField(i18n.T("User name"), name, "")
		h := kit.TextField(i18n.T("Home directory"), home, "/home/"+name+"-basalt")
		h.Help = i18n.T("A directory of its own keeps Basalt OS settings apart from the other system's. Nothing in the other directories is changed.")
		m.form = kit.NewForm(i18n.T("Your user on the existing /home"), "", n, h)
		m.form.Fields[1].Validate = func(v string) string {
			if !strings.HasPrefix(v, "/home/") || len(v) <= len("/home/") || strings.Contains(v, "..") {
				return i18n.T("a directory under /home")
			}
			return ""
		}
	case scHomeTPM:
		pass, tpm := tpmHomeLabels()
		cur := pass
		if p.Home.Existing != nil && p.Home.Existing.TPM2 {
			cur = tpm
		}
		m.picker = ui.NewPicker(i18n.T("Opening the existing /home at boot"), []string{pass, tpm}, cur)
	case scScope:
		system, home, none := scopeLabels()
		opts := []string{system, home, none}
		if p.Home.Existing != nil {
			opts = []string{system, none}
		}
		cur := system
		switch {
		case !p.Encrypted():
			cur = none
		case p.Encryption.Scope == "home":
			cur = home
		}
		m.picker = ui.NewPicker(i18n.T("What should be encrypted?"), opts, cur)
	}
}

func existingESP(d probe.Disk) string {
	for _, p := range d.Partitions {
		if p.Type == probe.TypeESP && p.FSType == "vfat" && p.Bytes >= 100<<20 {
			return p.Path
		}
	}
	return ""
}

// onStorageKey handles the keys of the storage screens.
func (m *model) onStorageKey(k tea.KeyMsg) tea.Cmd {
	p := &m.p
	switch m.scr {
	case scDiskUse:
		m.diskUse.Update(k)
		if !m.diskUse.Done {
			return nil
		}
		m.diskUse.Done = false
		if !m.diskUse.Accepted {
			m.goBack()
			return nil
		}
		switch m.diskUse.Cursor {
		case useErase:
			p.Target.Wipe, p.Target.ESP = true, ""
		case useFree:
			p.Target.Wipe, p.Target.ESP = false, ""
		case useFreeSharedESP:
			p.Target.Wipe, p.Target.ESP = false, existingESP(m.targetDisk())
		}
		return m.toHome()
	case scHome:
		m.picker.Update(k)
		if !m.picker.Done {
			return nil
		}
		if !m.picker.Accepted {
			m.goBack()
			return nil
		}
		if _, existing := homeLabels(); m.picker.Selected() == existing {
			m.go_(scHomePick)
			return nil
		}
		p.Home.Existing = nil
		m.fixSubvolumes()
		m.go_(scLayout)
	case scHomePick:
		m.homePick.Update(k)
		if !m.homePick.Done {
			return nil
		}
		m.homePick.Done = false
		if !m.homePick.Accepted {
			m.goBack()
			return nil
		}
		c := m.homeCands[m.homePick.Cursor]
		m.homeDev, m.homeLUKS = c.Path, c.FSType == "crypto_LUKS"
		if m.homeLUKS {
			m.go_(scHomePass)
			return nil
		}
		return m.inspect("")
	case scHomePass:
		cmd, _ := m.form.Update(k)
		if !m.form.Done {
			return cmd
		}
		if !m.form.Accepted {
			m.goBack()
			return nil
		}
		m.form.Done, m.form.Accepted = false, false
		m.form.Error = i18n.T("Opening the partition read-only.")
		return m.inspect(m.form.Value(0))
	case scHomeOwner:
		m.homeOwner.Update(k)
		if !m.homeOwner.Done {
			return nil
		}
		m.homeOwner.Done = false
		if !m.homeOwner.Accepted {
			m.goBack()
			return nil
		}
		u := p.Accounts.User
		if u == nil {
			u = &plan.User{Admin: plan.Bool(true)}
		}
		if i := m.homeOwner.Cursor; i < len(m.inspection.Owners) {
			o := m.inspection.Owners[i]
			u.Name, u.UID, u.GID, u.HomeDir = o.Name, o.UID, o.GID, "/home/"+o.Name+"-basalt"
		} else {
			u.UID, u.GID = 0, 0
		}
		p.Accounts.User = u
		m.go_(scHomeUser)
	case scHomeUser:
		cmd, _ := m.form.Update(k)
		if !m.form.Done {
			return cmd
		}
		if !m.form.Accepted {
			m.goBack()
			return nil
		}
		name, home := m.form.Value(0), m.form.Value(1)
		if home == "" && name != "" {
			home = "/home/" + name + "-basalt"
		}
		if p.Accounts.User == nil {
			p.Accounts.User = &plan.User{Admin: plan.Bool(true)}
		}
		p.Accounts.User.Name, p.Accounts.User.HomeDir = name, home
		if m.homeLUKS && m.facts.TPM2 {
			m.go_(scHomeTPM)
			return nil
		}
		m.fixSubvolumes()
		m.go_(scLayout)
	case scHomeTPM:
		m.picker.Update(k)
		if !m.picker.Done {
			return nil
		}
		if !m.picker.Accepted {
			m.goBack()
			return nil
		}
		_, tpm := tpmHomeLabels()
		p.Home.Existing.TPM2 = m.picker.Selected() == tpm
		m.fixSubvolumes()
		m.go_(scLayout)
	case scScope:
		m.picker.Update(k)
		if !m.picker.Done {
			return nil
		}
		if !m.picker.Accepted {
			m.goBack()
			return nil
		}
		system, home, _ := scopeLabels()
		switch m.picker.Selected() {
		case system:
			p.Encryption.Enabled, p.Encryption.Scope = plan.Bool(true), "system"
		case home:
			p.Encryption.Enabled, p.Encryption.Scope = plan.Bool(true), "home"
		default:
			p.Encryption.Enabled, p.Encryption.Scope = plan.Bool(false), "none"
		}
		m.fixSubvolumes()
		if p.Encrypted() {
			m.go_(scEncryption)
		} else {
			p.Encryption.Tang = plan.Tang{}
			m.go_(scSystem)
		}
	}
	return nil
}

// fixSubvolumes keeps the home subvolume in step with where /home lives.
func (m *model) fixSubvolumes() {
	p := &m.p
	var out []string
	has := false
	for _, s := range p.Layout.Subvolumes {
		if s == "home" {
			has = true
			if p.HomeElsewhere() {
				continue
			}
		}
		out = append(out, s)
	}
	if !p.HomeElsewhere() && !has {
		out = append([]string{"home"}, out...)
	}
	p.Layout.Subvolumes = out
}

func (m *model) inspect(pass string) tea.Cmd {
	ss, dev := m.opt.Session, m.homeDev
	return func() tea.Msg {
		res, err := ss.InspectHome(context.Background(), dev, pass)
		if err == nil {
			res.Device = dev
		}
		return inspectedMsg{res, err}
	}
}

func (m *model) onInspected(msg inspectedMsg, pass string) {
	if msg.err != nil {
		if m.scr == scHomePass {
			m.form.Error = msg.err.Error()
			return
		}
		m.err = msg.err.Error()
		return
	}
	m.inspection = msg.res
	tpm := false
	if m.p.Home.Existing != nil && m.p.Home.Existing.Device == m.homeDev {
		tpm = m.p.Home.Existing.TPM2
	}
	m.p.Home.Existing = &plan.ExistingHome{Device: m.homeDev, Passphrase: pass, TPM2: tpm}
	if m.p.Encryption.Scope == "home" {
		m.p.Encryption.Scope = "system"
	}
	m.go_(scHomeOwner)
}
