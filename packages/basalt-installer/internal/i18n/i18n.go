// Package i18n is the installer's translation catalog: GNU gettext message
// catalogs (.mo), domain "basalt-installer", looked up in
// /usr/share/locale/<lang>/LC_MESSAGES/ for the language of LANGUAGE,
// LC_ALL, LC_MESSAGES or LANG. English is the reference language: the
// msgid is the English text and is returned when no translation exists.
//
// Every user-facing string goes through T or N (whole sentences, with
// placeholders for values, never concatenated pieces); logs, the install log,
// the plan format and machine output stay English. The sources of the catalogs
// are po/basalt-installer.pot and po/<lang>.po in the package; a test
// checks that every T and N call is in the template and translated.
package i18n

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Domain is the gettext text domain of the installer.
const Domain = "basalt-installer"

// LocaleDir is where compiled catalogs are installed.
var LocaleDir = "/usr/share/locale"

// Catalog is one loaded language.
type Catalog struct {
	Lang     string
	Messages map[string][]string // msgid -> forms (one, or one per plural form)
	Plural   func(n int) int     // index of the plural form for n
}

var (
	mu      sync.Mutex
	current *Catalog
)

func get() *Catalog {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		current = Load(LocaleDir, Languages())
	}
	return current
}

// Reset forgets the loaded catalog (tests, or after a locale change).
func Reset() {
	mu.Lock()
	current = nil
	mu.Unlock()
}

// T returns the translation of msgid in the current language, or msgid.
func T(msgid string) string {
	if f := get().Messages[msgid]; len(f) > 0 && f[0] != "" {
		return f[0]
	}
	return msgid
}

// N returns the singular or plural form for n, translated with the
// catalog's plural rule (English: one for 1, other otherwise).
func N(singular, plural string, n int) string {
	c := get()
	if f := c.Messages[singular]; len(f) > 0 {
		i := c.Plural(n)
		if i >= 0 && i < len(f) && f[i] != "" {
			return f[i]
		}
	}
	if n == 1 {
		return singular
	}
	return plural
}

// Lang is the language of the loaded catalog ("" for the reference
// English text).
func Lang() string { return get().Lang }

// Languages lists the candidate catalogs for the environment, most
// specific first ("pt_BR.UTF-8" gives "pt_BR", then "pt").
func Languages() []string {
	var raw []string
	if v := os.Getenv("LANGUAGE"); v != "" {
		raw = strings.Split(v, ":")
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			raw = append(raw, v)
			break
		}
	}
	var out []string
	for _, l := range raw {
		l = strings.SplitN(strings.SplitN(l, ".", 2)[0], "@", 2)[0]
		if l == "" || l == "C" || l == "POSIX" {
			continue
		}
		out = append(out, l)
		if i := strings.IndexByte(l, '_'); i > 0 {
			out = append(out, l[:i])
		}
	}
	return out
}

// Load returns the first catalog found for langs, or an empty one.
func Load(dir string, langs []string) *Catalog {
	for _, l := range langs {
		if strings.HasPrefix(l, "en") {
			break // English is the reference text
		}
		b, err := os.ReadFile(filepath.Join(dir, l, "LC_MESSAGES", Domain+".mo"))
		if err != nil {
			continue
		}
		if c, err := ParseMO(b); err == nil {
			c.Lang = l
			return c
		}
	}
	return &Catalog{Messages: map[string][]string{}, Plural: english}
}

func english(n int) int {
	if n == 1 {
		return 0
	}
	return 1
}

// pluralRule understands the Plural-Forms expressions of the languages we
// ship or expect; anything else falls back to the English rule.
func pluralRule(header string) func(int) int {
	for _, line := range strings.Split(header, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "Plural-Forms") {
			continue
		}
		_, expr, _ := strings.Cut(v, "plural=")
		expr = strings.NewReplacer(" ", "", ";", "", "(", "", ")", "").Replace(expr)
		switch expr {
		case "n>1": // pt_BR, fr
			return func(n int) int {
				if n > 1 {
					return 1
				}
				return 0
			}
		case "0": // ja, zh, ko
			return func(int) int { return 0 }
		}
	}
	return english
}

// ParseMO reads a GNU .mo catalog.
func ParseMO(b []byte) (*Catalog, error) {
	if len(b) < 28 {
		return nil, errors.New("short .mo file")
	}
	var bo binary.ByteOrder
	switch binary.LittleEndian.Uint32(b) {
	case 0x950412de:
		bo = binary.LittleEndian
	case 0xde120495:
		bo = binary.BigEndian
	default:
		return nil, errors.New("not a .mo file")
	}
	n := int(bo.Uint32(b[8:]))
	orig, trans := int(bo.Uint32(b[12:])), int(bo.Uint32(b[16:]))
	str := func(table, i int) (string, error) {
		off := table + 8*i
		if off < 0 || off+8 > len(b) {
			return "", errors.New("bad .mo table")
		}
		l, p := int(bo.Uint32(b[off:])), int(bo.Uint32(b[off+4:]))
		if p < 0 || l < 0 || p+l > len(b) {
			return "", errors.New("bad .mo string")
		}
		return string(b[p : p+l]), nil
	}
	c := &Catalog{Messages: make(map[string][]string, n), Plural: english}
	for i := 0; i < n; i++ {
		id, err := str(orig, i)
		if err != nil {
			return nil, err
		}
		s, err := str(trans, i)
		if err != nil {
			return nil, err
		}
		if id == "" {
			c.Plural = pluralRule(s)
			continue
		}
		c.Messages[strings.SplitN(id, "\x00", 2)[0]] = strings.Split(s, "\x00")
	}
	return c, nil
}
