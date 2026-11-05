package plan

import "testing"

func TestKeyboardsAndLanguages(t *testing.T) {
	for _, km := range []string{"br", "br-abnt2", "br-abnt"} {
		if k, ok := LookupKeyboard(km); !ok || k.Keymap != "br" || k.Layout != "br" || k.Model != "abnt2" {
			t.Errorf("%s: %+v", km, k)
		}
	}
	if k, ok := KeyboardForXKB("br,us", ""); !ok || k.Keymap != "br" {
		t.Errorf("xkb br,us: %+v", k)
	}
	if _, ok := LookupKeyboard("dvorak-xyz"); ok {
		t.Error("unknown keymap found")
	}
	l, ok := LookupLanguage("pt_BR.utf8")
	if !ok || l.Keymap != "br" || l.Timezone != "America/Sao_Paulo" {
		t.Errorf("pt_BR: %+v", l)
	}
	for in, want := range map[string]string{"pt_BR.utf8": "pt_BR.UTF-8", "pt_BR": "pt_BR.UTF-8", "C.UTF-8": "", "POSIX": "", "": "", "de_DE.ISO-8859-1": "de_DE.ISO-8859-1"} {
		if got := NormalLocale(in); got != want {
			t.Errorf("NormalLocale(%q) = %q, want %q", in, got, want)
		}
	}
	// Every language's keyboard is one the installer knows.
	for _, l := range Languages() {
		if _, ok := LookupKeyboard(l.Keymap); !ok {
			t.Errorf("%s suggests the unknown keyboard %s", l.Locale, l.Keymap)
		}
	}
}
