package plan

import "strings"

// The languages and keyboards the installer offers by name. A plan may
// name any locale and console keymap; these are the ones the frontends
// list, with what goes together (a language suggests its keyboard and
// time zone, a keyboard knows its X11 layout for the login screen).

// Language is one locale the frontends offer.
type Language struct {
	Locale string `json:"locale"`
	Name   string `json:"name"` // in the language itself
	// Keymap and Timezone are what choosing the language suggests.
	Keymap   string `json:"keymap"`
	Timezone string `json:"timezone"`
}

// Keyboard is one keyboard layout: the console keymap (vconsole.conf,
// also used in the initramfs, so at the disk passphrase prompt) and the
// X11/XKB layout of the login screen and the desktop.
type Keyboard struct {
	Keymap  string `json:"keymap"` // console keymap (kbd's xkb-generated maps)
	Layout  string `json:"layout"` // XKB layout
	Variant string `json:"variant,omitempty"`
	Model   string `json:"model,omitempty"`
	Name    string `json:"name"`
	// Aliases are other console keymap names for the same layout (the
	// legacy kbd maps, kbd-model-map's names).
	Aliases []string `json:"-"`
}

// Languages lists the offered languages.
func Languages() []Language {
	return []Language{
		// The United States span several zones: no suggestion.
		{"en_US.UTF-8", "English (United States)", "us", ""},
		{"en_GB.UTF-8", "English (United Kingdom)", "gb", "Europe/London"},
		{"pt_BR.UTF-8", "Português (Brasil)", "br", "America/Sao_Paulo"},
		{"pt_PT.UTF-8", "Português (Portugal)", "pt", "Europe/Lisbon"},
		{"es_ES.UTF-8", "Español (España)", "es", "Europe/Madrid"},
		{"es_MX.UTF-8", "Español (México)", "latam", "America/Mexico_City"},
		{"fr_FR.UTF-8", "Français (France)", "fr", "Europe/Paris"},
		{"de_DE.UTF-8", "Deutsch (Deutschland)", "de", "Europe/Berlin"},
		{"it_IT.UTF-8", "Italiano (Italia)", "it", "Europe/Rome"},
	}
}

// Keyboards lists the offered keyboard layouts.
func Keyboards() []Keyboard {
	return []Keyboard{
		{Keymap: "us", Layout: "us", Name: "English (US)"},
		{Keymap: "us-intl", Layout: "us", Variant: "intl", Name: "English (US, international with dead keys)", Aliases: []string{"us-acentos"}},
		{Keymap: "br", Layout: "br", Model: "abnt2", Name: "Portuguese (Brazil, ABNT2)", Aliases: []string{"br-abnt2", "br-abnt"}},
		{Keymap: "gb", Layout: "gb", Name: "English (UK)", Aliases: []string{"uk"}},
		{Keymap: "pt", Layout: "pt", Name: "Portuguese (Portugal)", Aliases: []string{"pt-latin1"}},
		{Keymap: "es", Layout: "es", Name: "Spanish (Spain)"},
		{Keymap: "latam", Layout: "latam", Name: "Spanish (Latin American)", Aliases: []string{"la-latin1"}},
		{Keymap: "fr", Layout: "fr", Name: "French", Aliases: []string{"fr-latin1", "fr-latin9", "fr-pc"}},
		{Keymap: "de", Layout: "de", Name: "German", Aliases: []string{"de-latin1"}},
		{Keymap: "de-nodeadkeys", Layout: "de", Variant: "nodeadkeys", Name: "German (no dead keys)", Aliases: []string{"de-latin1-nodeadkeys"}},
		{Keymap: "it", Layout: "it", Name: "Italian"},
		{Keymap: "ch", Layout: "ch", Name: "German (Switzerland)", Aliases: []string{"sg", "sg-latin1"}},
		{Keymap: "be", Layout: "be", Name: "Belgian", Aliases: []string{"be-latin1"}},
		{Keymap: "dk", Layout: "dk", Name: "Danish", Aliases: []string{"dk-latin1"}},
		{Keymap: "no", Layout: "no", Name: "Norwegian"},
		{Keymap: "se", Layout: "se", Name: "Swedish", Aliases: []string{"se-latin1"}},
		{Keymap: "fi", Layout: "fi", Name: "Finnish"},
		{Keymap: "pl", Layout: "pl", Name: "Polish", Aliases: []string{"pl2"}},
	}
}

// LookupKeyboard finds a keyboard by its console keymap or an alias.
func LookupKeyboard(keymap string) (Keyboard, bool) {
	for _, k := range Keyboards() {
		if k.Keymap == keymap {
			return k, true
		}
		for _, a := range k.Aliases {
			if a == keymap {
				return k, true
			}
		}
	}
	return Keyboard{}, false
}

// KeyboardForXKB finds a keyboard by its XKB layout and variant (the first
// layout of a list such as "br,us").
func KeyboardForXKB(layout, variant string) (Keyboard, bool) {
	layout, _, _ = strings.Cut(layout, ",")
	variant, _, _ = strings.Cut(variant, ",")
	for _, k := range Keyboards() {
		if k.Layout == layout && k.Variant == variant {
			return k, true
		}
	}
	return Keyboard{}, false
}

// LookupLanguage finds an offered language by its locale ("pt_BR.UTF-8",
// also "pt_BR.utf8" and "pt_BR").
func LookupLanguage(locale string) (Language, bool) {
	base, _, _ := strings.Cut(locale, ".")
	for _, l := range Languages() {
		if lb, _, _ := strings.Cut(l.Locale, "."); lb == base {
			return l, true
		}
	}
	return Language{}, false
}

// NormalLocale turns a locale of the environment into the form a plan
// uses: "pt_BR.utf8" and "pt_BR" become "pt_BR.UTF-8"; C and POSIX give "".
func NormalLocale(s string) string {
	s = strings.TrimSpace(s)
	base, enc, _ := strings.Cut(s, ".")
	if base == "" || base == "C" || base == "POSIX" {
		return ""
	}
	if enc == "" || strings.EqualFold(strings.ReplaceAll(enc, "-", ""), "utf8") {
		enc = "UTF-8"
	}
	return base + "." + enc
}
