package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/keymap"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// basalt keyboard set builds the keyboard.system proposal from the
// layouts asked for (checked against the registry), the system's model
// and the console keymap that fits; it stores it only as root and never
// runs localectl itself.
func TestKeyboardSet(t *testing.T) {
	d := t.TempDir()
	kd := filepath.Join(d, "keymaps", "xkb")
	_ = os.MkdirAll(kd, 0o755)
	for _, k := range []string{"br", "us", "us-intl"} {
		_ = os.WriteFile(filepath.Join(kd, k+".map.gz"), nil, 0o644)
	}
	reg, _ := filepath.Abs("../keymap/testdata/evdev.xml")
	oldR, oldK, oldX, oldV := keymap.RegistryPaths, keymap.KeymapDirs, keymap.X11Conf, keymap.VConsoleConf
	keymap.RegistryPaths, keymap.KeymapDirs = []string{reg}, []string{filepath.Join(d, "keymaps")}
	keymap.X11Conf, keymap.VConsoleConf = filepath.Join(d, "00-keyboard.conf"), filepath.Join(d, "vconsole.conf")
	t.Cleanup(func() {
		keymap.RegistryPaths, keymap.KeymapDirs, keymap.X11Conf, keymap.VConsoleConf = oldR, oldK, oldX, oldV
	})
	_ = os.WriteFile(keymap.X11Conf, []byte("Section \"InputClass\"\n  Option \"XkbLayout\" \"us\"\n  Option \"XkbModel\" \"pc104\"\nEndSection\n"), 0o644)

	run := func(argv ...string) (string, error) {
		o, err := parse(argv)
		if err != nil {
			return "", err
		}
		cfg := config.Defaults()
		cfg.StateDir, cfg.AuditPath = d, filepath.Join(d, "audit.jsonl")
		out := &bytes.Buffer{}
		a := &app{o: o, cfg: cfg, store: proposal.Store{Dir: filepath.Join(d, "proposals")}, audit: audit.New(cfg.AuditPath, "basalt"), out: out}
		err = a.dispatch(context.Background())
		return out.String(), err
	}
	out, err := run("keyboard", "set", "us(intl),br", "--options", "grp:alt_shift_toggle", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Stored   bool     `json:"stored"`
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out), &ref); err != nil || len(ref.Commands) != 2 {
		t.Fatalf("%s %v", out, err)
	}
	if got := ref.Commands[0]; got != "localectl set-x11-keymap --no-convert us,br pc104 intl, grp:alt_shift_toggle" {
		t.Errorf("x11: %s", got)
	}
	if got := ref.Commands[1]; got != "localectl set-keymap --no-convert us-intl" {
		t.Errorf("console: %s", got)
	}
	if ref.Stored {
		t.Error("stored without root")
	}
	for _, bad := range [][]string{{"keyboard", "set", "br(abnt2)", "--json"}, {"keyboard", "set", "br;id", "--json"}, {"keyboard", "set", "br", "--options", "compose:caps", "--json"}} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if out, err := run("keyboard", "--json"); err != nil || !strings.Contains(out, `"model": "pc104"`) {
		t.Errorf("report: %s %v", out, err)
	}
}
