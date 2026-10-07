package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/keymap"
)

// keyboard.system names only layouts, variants and options the system's
// XKB registry lists and a console keymap kbd has; the commands are
// rebuilt from them (localectl, in the executor).
func TestKeyboardSystem(t *testing.T) {
	dir := t.TempDir()
	withKeymaps(t, dir)
	good := Action{Kind: KeyboardSystem, Params: map[string]string{"layouts": "br,us(intl)", "options": "grp:alt_shift_toggle", "model": "pc105", "keymap": "br"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cmds, err := good.Commands()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range cmds {
		lines = append(lines, c.String())
	}
	want := "localectl set-x11-keymap --no-convert br,us pc105 ,intl grp:alt_shift_toggle\nlocalectl set-keymap --no-convert br"
	if strings.Join(lines, "\n") != want {
		t.Errorf("commands:\n%s", strings.Join(lines, "\n"))
	}
	bad := map[string]map[string]string{
		"unknown layout":   {"layouts": "xx", "model": "pc105", "keymap": "us"},
		"unknown variant":  {"layouts": "br(abnt2)", "model": "pc105", "keymap": "br"},
		"arbitrary string": {"layouts": "br;reboot", "model": "pc105", "keymap": "br"},
		"not canonical":    {"layouts": "br, us", "model": "pc105", "keymap": "br"},
		"unknown option":   {"layouts": "br", "options": "compose:caps", "model": "pc105", "keymap": "br"},
		"bad option":       {"layouts": "br", "options": "x", "model": "pc105", "keymap": "br"},
		"no model":         {"layouts": "br", "keymap": "br"},
		"missing keymap":   {"layouts": "br", "model": "pc105", "keymap": "nosuch"},
		"keymap path":      {"layouts": "br", "model": "pc105", "keymap": "../../etc/shadow"},
		"extra parameter":  {"layouts": "br", "model": "pc105", "keymap": "br", "file": "/etc/passwd"},
		"five layouts":     {"layouts": "br,us,de,us(intl),br(thinkpad)", "model": "pc105", "keymap": "br"},
	}
	for name, p := range bad {
		if err := (Action{Kind: KeyboardSystem, Params: p}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The check after applying reads localed's files.
	checks := good.Verify(timeZero)
	if len(checks) != 1 {
		t.Fatalf("%d checks", len(checks))
	}
	if ok, _ := checks[0].Run(context.Background(), nil); ok {
		t.Error("passed before anything changed")
	}
	_ = os.WriteFile(keymap.X11Conf, []byte("Section \"InputClass\"\n  Option \"XkbLayout\" \"br,us\"\n  Option \"XkbModel\" \"pc105\"\n  Option \"XkbVariant\" \",intl\"\n  Option \"XkbOptions\" \"grp:alt_shift_toggle\"\nEndSection\n"), 0o644)
	_ = os.WriteFile(keymap.VConsoleConf, []byte("KEYMAP=\"br\"\n"), 0o644)
	if ok, got := checks[0].Run(context.Background(), nil); !ok {
		t.Errorf("after: %s", got)
	}
}

// withKeymaps points the keymap package at a small registry and a few
// console keymaps.
func withKeymaps(t *testing.T, dir string) {
	t.Helper()
	kd := filepath.Join(dir, "keymaps", "xkb")
	_ = os.MkdirAll(kd, 0o755)
	for _, k := range []string{"br", "us", "us-intl"} {
		_ = os.WriteFile(filepath.Join(kd, k+".map.gz"), nil, 0o644)
	}
	reg, _ := filepath.Abs("../keymap/testdata/evdev.xml")
	oldR, oldK, oldX, oldV := keymap.RegistryPaths, keymap.KeymapDirs, keymap.X11Conf, keymap.VConsoleConf
	keymap.RegistryPaths, keymap.KeymapDirs = []string{reg}, []string{filepath.Join(dir, "keymaps")}
	keymap.X11Conf, keymap.VConsoleConf = filepath.Join(dir, "00-keyboard.conf"), filepath.Join(dir, "vconsole.conf")
	t.Cleanup(func() {
		keymap.RegistryPaths, keymap.KeymapDirs, keymap.X11Conf, keymap.VConsoleConf = oldR, oldK, oldX, oldV
	})
}
