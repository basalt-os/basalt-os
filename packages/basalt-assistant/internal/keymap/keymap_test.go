package keymap

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// WithTestData points the package at a small registry, a few console
// keymaps and a legacy map (also used by the action tests).
func WithTestData(t *testing.T, dir string) {
	t.Helper()
	kd := filepath.Join(dir, "keymaps", "xkb")
	_ = os.MkdirAll(kd, 0o755)
	for _, k := range []string{"br", "us", "us-intl", "de-nodeadkeys"} {
		_ = os.WriteFile(filepath.Join(kd, k+".map.gz"), nil, 0o644)
	}
	_ = os.MkdirAll(filepath.Join(dir, "keymaps", "i386", "qwerty"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "keymaps", "i386", "qwerty", "br-abnt2.map.gz"), nil, 0o644)
	mm := filepath.Join(dir, "kbd-model-map")
	_ = os.WriteFile(mm, []byte("# console x11 model variant options\nbr-abnt2\tbr\tabnt2\t-\tterminate:ctrl_alt_bksp\nus-acentos\tus\tpc105\tintl\t-\n"), 0o644)
	reg, _ := filepath.Abs("testdata/evdev.xml")
	if _, err := os.Stat(reg); err != nil {
		reg, _ = filepath.Abs("../keymap/testdata/evdev.xml")
	}
	oldR, oldK, oldM, oldX, oldV := RegistryPaths, KeymapDirs, ModelMap, X11Conf, VConsoleConf
	RegistryPaths, KeymapDirs, ModelMap = []string{reg}, []string{filepath.Join(dir, "keymaps")}, mm
	X11Conf, VConsoleConf = filepath.Join(dir, "00-keyboard.conf"), filepath.Join(dir, "vconsole.conf")
	t.Cleanup(func() { RegistryPaths, KeymapDirs, ModelMap, X11Conf, VConsoleConf = oldR, oldK, oldM, oldX, oldV })
}

func TestParse(t *testing.T) {
	cs, err := ParseLayouts("br,us(intl)")
	if err != nil || FormatLayouts(cs) != "br,us(intl)" {
		t.Fatal(cs, err)
	}
	if l, v := XKBLists(cs); l != "br,us" || v != ",intl" {
		t.Errorf("%q %q", l, v)
	}
	if l, v := XKBLists([]Choice{{Layout: "br"}}); l != "br" || v != "" {
		t.Errorf("%q %q", l, v)
	}
	for _, bad := range []string{"", "br,", "br,br", "br;reboot", "us(intl", "a,b,c,d,e", "BR", "$(id)"} {
		if _, err := ParseLayouts(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if o, err := ParseOptions("grp:alt_shift_toggle,compose:ralt"); err != nil || len(o) != 2 {
		t.Error(o, err)
	}
	for _, bad := range []string{"grp", "grp:x;y", "a:b,", "grp:x y"} {
		if _, err := ParseOptions(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRegistryAndConsole(t *testing.T) {
	WithTestData(t, t.TempDir())
	r, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check([]Choice{{Layout: "br"}, {Layout: "us", Variant: "intl"}}, []string{"grp:alt_shift_toggle"}); err != nil {
		t.Error(err)
	}
	for _, bad := range [][]Choice{{{Layout: "xx"}}, {{Layout: "br", Variant: "abnt2"}}, {{Layout: "us", Variant: "nodeadkeys"}}} {
		if err := r.Check(bad, nil); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := r.Check([]Choice{{Layout: "br"}}, []string{"compose:caps"}); err == nil {
		t.Error("an option the registry lacks was accepted")
	}
	for in, want := range map[string]string{"br": "br", "us(intl),br": "us-intl", "de(nodeadkeys)": "de-nodeadkeys", "br(thinkpad)": "br", "de": "us"} {
		cs, _ := ParseLayouts(in)
		if got := ConsoleKeymap(cs); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if !KeymapExists("br-abnt2") || KeymapExists("../etc/passwd") || KeymapExists("nosuch") {
		t.Error("KeymapExists")
	}
}

func TestReadCurrent(t *testing.T) {
	dir := t.TempDir()
	WithTestData(t, dir)
	_ = os.WriteFile(X11Conf, []byte("Section \"InputClass\"\n  Option \"XkbLayout\" \"br,us\"\n  Option \"XkbModel\" \"pc105\"\n  Option \"XkbVariant\" \",intl\"\nEndSection\n"), 0o644)
	_ = os.WriteFile(VConsoleConf, []byte("KEYMAP=br\n"), 0o644)
	c := ReadCurrent()
	if !reflect.DeepEqual(c.Layouts, []Choice{{Layout: "br"}, {Layout: "us", Variant: "intl"}}) || c.Model != "pc105" || c.Console != "br" || c.Options != nil {
		t.Errorf("%+v", c)
	}
}
