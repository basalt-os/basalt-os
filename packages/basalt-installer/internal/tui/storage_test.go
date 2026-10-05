package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
)

// A 100 GiB disk with Windows (EFI, MSR, NTFS, recovery) on its first
// 60 GiB, and a second disk with an encrypted /home.
const lsblkDual = `{"blockdevices":[
{"name":"nvme0n1","path":"/dev/nvme0n1","size":107374182400,"type":"disk","rm":false,"ro":false,"rota":false,"model":"NVMe","serial":"N","tran":"nvme","fstype":null,"mountpoints":[null],"label":null,"pttype":"gpt","log-sec":512,
 "children":[
  {"name":"nvme0n1p1","path":"/dev/nvme0n1p1","size":104857600,"type":"part","fstype":"vfat","mountpoints":[null],"label":"SYSTEM","uuid":"1A2B-3C4D","parttype":"c12a7328-f81f-11d2-ba4b-00a0c93ec93b","partn":1,"start":2048},
  {"name":"nvme0n1p2","path":"/dev/nvme0n1p2","size":16777216,"type":"part","fstype":null,"mountpoints":[null],"label":null,"uuid":null,"parttype":"e3c9e316-0b5c-4db8-817d-f92df00215ae","partn":2,"start":206848},
  {"name":"nvme0n1p3","path":"/dev/nvme0n1p3","size":63350767616,"type":"part","fstype":"ntfs","mountpoints":[null],"label":"Windows","uuid":"01D8","parttype":"ebd0a0a2-b9e5-4433-87c0-68b6b72699c7","partn":3,"start":239616},
  {"name":"nvme0n1p4","path":"/dev/nvme0n1p4","size":838860800,"type":"part","fstype":"ntfs","mountpoints":[null],"label":"Recovery","uuid":"02D8","parttype":"de94bba4-06d1-4d40-a16a-bfd50179d6ac","partn":4,"start":123967488}]},
{"name":"sda","path":"/dev/sda","size":214748364800,"type":"disk","rm":false,"ro":false,"rota":true,"model":"Data","serial":"D","tran":"sata","fstype":null,"mountpoints":[null],"label":null,"pttype":"gpt","log-sec":512,
 "children":[{"name":"sda1","path":"/dev/sda1","size":214747316224,"type":"part","fstype":"crypto_LUKS","mountpoints":[null],"label":null,"uuid":"11111111-2222-3333-4444-555555555555","parttype":"0fc63daf-8483-4772-8e79-3d69d8477de4","partn":1,"start":2048}]}]}`

func fakeInspector(_ context.Context, part probe.Partition, pass string) (session.HomeInspection, error) {
	if pass != "home passphrase" {
		return session.HomeInspection{}, errWrongPass
	}
	return session.HomeInspection{Device: part.Path, LUKS: true, UUID: part.UUID, FSType: "ext4",
		Owners: []session.HomeOwner{{Name: "edimar", UID: 1000, GID: 1000}}}, nil
}

type wrongPass struct{}

func (wrongPass) Error() string { return "that is not the passphrase of this partition" }

var errWrongPass = wrongPass{}

// press sends a key and runs the command it returns (one level), like the
// program loop would.
func press(m *model, k tea.KeyMsg) {
	_, cmd := m.Update(k)
	run(m, cmd)
}

func run(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	// Only the installer's own commands; cursor blinks would wait.
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		switch msg.(type) {
		case candidatesMsg, inspectedMsg:
			m.Update(msg)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func typeText(m *model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestDualBootAndExistingHomeFlow(t *testing.T) {
	m := testModelWith(t, "", lsblkDual, fakeInspector)
	m.p.Accounts.User = nil
	m.enter(scDisk)
	// The NVMe disk is the first one.
	press(m, key("enter"))
	if m.scr != scDiskUse {
		t.Fatalf("a disk with Windows asks how to use it: screen %d", m.scr)
	}
	v := fits(t, "disk use", m, false)
	if !strings.Contains(v, "Windows") || !strings.Contains(v, "free space") {
		t.Fatalf("disk use screen:\n%s", v)
	}
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, key("enter"))
	if m.p.Target.Wipe || m.scr != scHome {
		t.Fatalf("free space chosen: wipe %v, screen %d", m.p.Target.Wipe, m.scr)
	}
	fits(t, "home", m, true)
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, key("enter"))
	if m.scr != scHomePick {
		t.Fatalf("screen %d", m.scr)
	}
	fits(t, "home partition", m, false)
	press(m, key("enter"))
	if m.scr != scHomePass {
		t.Fatalf("a LUKS /home asks for its passphrase: screen %d", m.scr)
	}
	typeText(m, "wrong")
	press(m, key("enter"))
	if m.scr != scHomePass || !strings.Contains(m.form.Error, "not the passphrase") {
		t.Fatalf("wrong passphrase: screen %d, %q", m.scr, m.form.Error)
	}
	m.enter(scHomePass)
	typeText(m, "home passphrase")
	press(m, key("enter"))
	if m.scr != scHomeOwner {
		t.Fatalf("screen %d, error %q", m.scr, m.form.Error)
	}
	fits(t, "home owner", m, false)
	press(m, key("enter"))
	u := m.p.Accounts.User
	if m.scr != scHomeUser || u == nil || u.Name != "edimar" || u.UID != 1000 || u.HomeDir != "/home/edimar-basalt" {
		t.Fatalf("user from the existing /home: %+v (screen %d)", u, m.scr)
	}
	fits(t, "home user", m, true)
	press(m, key("ctrl+s"))
	if m.scr != scHomeTPM {
		t.Fatalf("screen %d", m.scr)
	}
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, key("enter"))
	e := m.p.Home.Existing
	if m.scr != scLayout || e == nil || e.Device != "/dev/sda1" || e.Passphrase != "home passphrase" || !e.TPM2 {
		t.Fatalf("existing home: %+v (screen %d)", e, m.scr)
	}
	for _, sv := range m.p.Layout.Subvolumes {
		if sv == "home" {
			t.Fatal("no home subvolume with an existing /home")
		}
	}
	press(m, key("enter")) // automatic layout
	if m.scr != scScope {
		t.Fatalf("screen %d", m.scr)
	}
	v = fits(t, "scope", m, true)
	if strings.Contains(v, "Only /home") {
		t.Fatal("with an existing /home, a new encrypted /home is not offered")
	}
	press(m, key("enter"))
	if m.scr != scEncryption {
		t.Fatalf("screen %d", m.scr)
	}
	// The plan is valid and previews.
	m.p.Accounts.User.Password = "a-password-123"
	m.enter(scReview)
	if m.preview.Token == "" {
		t.Fatalf("no preview:\n%s", m.pager.Text)
	}
	text := m.preview.Text
	for _, want := range []string{"sgdisk --new=5:", "luks-11111111-2222-3333-4444-555555555555", "--home-dir /home/edimar-basalt --uid 1000"} {
		if !strings.Contains(text, want) {
			t.Fatalf("preview lacks %q", want)
		}
	}
	if strings.Contains(text, "--zap-all") {
		t.Fatal("the Windows disk is erased")
	}
}

func TestScopeHomeOnly(t *testing.T) {
	m := testModel(t, "")
	m.enter(scScope)
	press(m, tea.KeyMsg{Type: tea.KeyDown})
	press(m, key("enter"))
	if m.p.Encryption.Scope != "home" || m.scr != scEncryption {
		t.Fatalf("scope %q, screen %d", m.p.Encryption.Scope, m.scr)
	}
	m.enter(scReview)
	if m.preview.Token == "" || !strings.Contains(m.preview.Text, "--label basalt-home") {
		t.Fatalf("home-only preview:\n%s", m.pager.Text)
	}
}
