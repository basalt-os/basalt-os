package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/tui-tools/tui-kit/theme"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/session"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

const lsblk = `{"blockdevices":[{"name":"vda","path":"/dev/vda","size":32212254720,"type":"disk","rm":false,"ro":false,"rota":true,"model":null,"serial":null,"tran":null,"fstype":null,"mountpoints":[null],"label":null},
{"name":"sdb","path":"/dev/sdb","size":8012345344,"type":"disk","rm":true,"ro":false,"rota":false,"model":"USB stick","serial":"U","tran":"usb","fstype":null,"mountpoints":[null],"label":null,
 "children":[{"name":"sdb1","path":"/dev/sdb1","size":8011296768,"type":"part","rm":true,"ro":false,"rota":false,"model":null,"serial":null,"tran":"usb","fstype":"vfat","mountpoints":[null],"label":"KEYS"}]}]}`

// testModel is the TUI on a made-up machine (UEFI, Secure Boot, TPM, one
// network interface) at 80x24, the size of a serial console.
func testModel(t *testing.T, cmdline string) *model {
	t.Helper()
	return testModelWith(t, cmdline, lsblk, nil)
}

func testModelWith(t *testing.T, cmdline, lsblk string, inspector session.HomeInspector) *model {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c": "\x06\x00\x00\x00\x01",
		"sys/class/tpm/tpm0/tpm_version_major":                                     "2\n",
		"etc/os-release":                                                           "VERSION_ID=44\n",
		"proc/meminfo":                                                             "MemTotal: 4000000 kB\n",
		"sys/class/net/enp0s2/type":                                                "1\n",
		"proc/cmdline":                                                             cmdline,
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	ss := session.New(session.Options{
		Prober: probe.Prober{Root: root, Read: func(_ context.Context, argv ...string) (string, error) {
			if argv[0] == "lsblk" {
				return lsblk, nil
			}
			return "kvm\n", nil
		}},
		Steps:         steps.Options{Root: filepath.Join(dir, "sysroot"), Work: filepath.Join(dir, "work"), MediaDir: filepath.Join(dir, "media")},
		Engine:        engine.Options{Runner: &engine.FakeRunner{}, LogDir: filepath.Join(dir, "log"), LockPath: filepath.Join(dir, "lock"), LogNoSync: true},
		Cmdline:       filepath.Join(root, "proc/cmdline"),
		HomeInspector: inspector,
	})
	m := &model{opt: Options{Session: ss, Version: "0.2.0", ASCII: true}, t: theme.FromPalette(BasaltPalette()), be: ss, w: 80, h: 24}
	s := suggest(ss)
	if s.err != nil {
		t.Fatal(s.err)
	}
	m.p, m.facts, m.note = s.p, s.f, s.note
	m.p.Target.Wipe = true
	m.p.Accounts.Root.SSHKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl lab@basalt"}
	return m
}

// fits checks that a screen fits 80x24 without truncating its text.
func fits(t *testing.T, name string, m *model, noEllipsis bool) string {
	t.Helper()
	v := m.View()
	lines := strings.Split(v, "\n")
	if !strings.Contains(v, "╰") && !strings.Contains(v, "+-") && m.ack == nil && m.scr != scReview && m.scr != scFailed && m.scr != scDone && m.scr != scWelcome && m.scr != scProgress {
		t.Errorf("%s: the dialog's bottom border is cut off:\n%s", name, v)
	}
	if len(lines) > 24 {
		t.Errorf("%s: %d lines, more than 24:\n%s", name, len(lines), v)
	}
	for _, l := range lines {
		if w := lipgloss.Width(l); w > 80 {
			t.Errorf("%s: a line is %d columns wide: %q", name, w, l)
		}
	}
	if noEllipsis && strings.Contains(v, "…") {
		t.Errorf("%s: text cut at 80 columns:\n%s", name, v)
	}
	return v
}

func key(s string) tea.KeyMsg {
	switch s {
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestScreensFitASerialConsole(t *testing.T) {
	m := testModel(t, "console=ttyS0")
	for _, sc := range []struct {
		name       string
		s          screen
		noEllipsis bool
	}{
		{"welcome", scWelcome, true}, {"disk", scDisk, false}, {"layout", scLayout, true}, {"encryption", scEncryption, true},
		{"system", scSystem, true}, {"profile", scProfile, true}, {"accounts", scAccounts, true}, {"network", scNetwork, true},
		{"static", scStatic, true}, {"repositories", scRepos, false}, {"repository url", scRepoURL, true}, {"review", scReview, true},
	} {
		m.enter(sc.s)
		fits(t, sc.name, m, sc.noEllipsis)
	}
	m.enter(scReview)
	if m.preview.Token == "" {
		t.Fatalf("no preview: %s", m.pager.Text)
	}
	m.enter(scConfirm)
	fits(t, "confirm", m, true)

	// A failure with a long, multi-line message and a full output tail.
	m.scr = scFailed
	m.result = "step 75 (Install the minimal profile) failed: exit status 1:\nFailed to resolve the transaction:\nNo match for argument: basalt-prompt\nYou can try to add to command line:\n  --skip-unavailable to skip unavailable packages\nmore\nand more"
	m.logPath = "/var/log/basalt-installer/install-20261004T201500Z.jsonl"
	for i := 0; i < 12; i++ {
		m.progress.Add("rollback: cryptsetup close luks-8786737a-5922-4a98-a169-30d0f87c0147 and a long tail that goes on")
	}
	v := fits(t, "failed", m, false)
	if !strings.Contains(v, "The installation failed") || !strings.Contains(v, "Basalt OS installer") {
		t.Fatalf("failed screen lost its title or header:\n%s", v)
	}

	m.scr = scDone
	m.saved = []string{"/dev/sdb1 (KEYS): basalt-recovery-key-basalt-20261004-201500.txt"}
	fits(t, "done", m, true)

	m.scr = scProgress
	m.showKey("fjkldhgr-uvbntckr-hnilbvcj-ldiekgnb-rtfhduje-cvjgnbtr-ikluhdcb-nvrkrfdh")
	v = fits(t, "recovery key", m, true)
	if !strings.Contains(v, "ctrl+o") {
		t.Fatalf("the key dialog does not offer the USB stick:\n%s", v)
	}
	m.ack.Note = "A copy is on /dev/sdc1 (KEYS): basalt-recovery-key-basalt-demo-20261004-231048.txt. Keep that stick somewhere safe, then type the first group to confirm."
	v = fits(t, "recovery key with a copy saved", m, false)
	if !strings.Contains(v, "╰") || !strings.Contains(v, "ctrl+o") {
		t.Fatalf("the key dialog lost its frame or its keys:\n%s", v)
	}
}

func TestStaticNetworkPrefillsTheInterface(t *testing.T) {
	m := testModel(t, "")
	m.enter(scStatic)
	if got := m.form.Value(0); got != "enp0s2" {
		t.Fatalf("interface field: %q, want the detected enp0s2", got)
	}
}

func TestPlanPasswordHashSurvivesTheAccountsScreen(t *testing.T) {
	m := testModel(t, "")
	hash := "$6$Q68QMNtsq0e4Z7Oz$eKFxxRA6M/AX6PXjlPJKg4g702quOf/qgDHcmWTtFuXTcZAAOY4ryZLa.tEh0J8Cd7qvOQR3Y3i2viJ289fnI1"
	m.p.Accounts.User = &plan.User{Name: "admin", PasswordHash: hash, FullName: "Admin", Admin: plan.Bool(true)}
	m.enter(scAccounts)
	m.onKey(key("ctrl+s"))
	if m.scr != scNetwork {
		t.Fatalf("the accounts screen did not accept the plan's user: %q", m.form.Error)
	}
	u := m.p.Accounts.User
	if u == nil || u.PasswordHash != hash || u.Password != "" || u.FullName != "Admin" {
		t.Fatalf("user after the accounts screen: %+v", u)
	}
	// A new password replaces the hash.
	m.enter(scAccounts)
	m.form.Fields[2].Input.SetValue("a-new-password-1")
	m.form.Fields[3].Input.SetValue("a-new-password-1")
	m.onKey(key("ctrl+s"))
	if u := m.p.Accounts.User; u.Password != "a-new-password-1" || u.PasswordHash != "" {
		t.Fatalf("user after typing a password: %+v", u)
	}
	// Another user name does not inherit the hash, and needs a password or a key.
	m.p.Accounts.User = &plan.User{Name: "admin", PasswordHash: hash}
	m.enter(scAccounts)
	m.form.Fields[1].Input.SetValue("ana")
	m.onKey(key("ctrl+s"))
	if m.scr == scNetwork {
		t.Fatal("a new user without password or key was accepted")
	}
}

func TestPlanLoadedIsShownOnTheWelcome(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(pf, []byte("apiVersion: basalt-install-plan/v1\ntarget: {disk: /dev/vda, wipe: true}\nhostname: from-plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, "basalt.inst.plan="+pf)
	if m.p.Hostname != "from-plan" || !strings.Contains(m.note, pf) {
		t.Fatalf("plan %q, note %q", m.p.Hostname, m.note)
	}
	m.enter(scWelcome)
	fits(t, "welcome with a plan", m, false)
	bad := testModel(t, "basalt.inst.plan="+filepath.Join(dir, "missing.yaml"))
	if bad.p.Hostname != plan.DefaultHostname || !strings.Contains(bad.note, "could not be read") {
		t.Fatalf("missing plan: %q, note %q", bad.p.Hostname, bad.note)
	}
}
