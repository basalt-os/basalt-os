// Package i18n is the assistant's translation catalog: GNU gettext message
// catalogs (.mo), domain "basalt-assistant", looked up in
// /usr/share/locale/<lang>/LC_MESSAGES/ for the language of LANGUAGE,
// LC_ALL, LC_MESSAGES or LANG. English is the reference language: the
// msgid is the English text and is returned when no translation exists.
//
// Every user-facing string goes through T or N (whole sentences, with
// placeholders for values, never concatenated pieces); logs, audit records
// and machine output (--json) stay English. Text that is stored in English
// and translated when it is shown (proposal titles) is marked with M.
// Text written both for people and for machines (a proposal's rendering)
// takes a Tr: the zero Tr is English, Local the reader's language. The
// sources of the catalogs are po/basalt-assistant.pot and po/<lang>.po in
// the package; a test checks that every T, N, tr.T and tr.N call is in the
// template and translated.
package i18n

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Domain is the gettext text domain of the assistant.
const Domain = "basalt-assistant"

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

// Use makes c the loaded catalog (tests of the packages that translate).
func Use(c *Catalog) {
	mu.Lock()
	current = c
	mu.Unlock()
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

// M marks msgid as a message of the catalog and returns it untranslated:
// for text that is written in English (stored, logged) and translated only
// where a person reads it, with T. The catalog template collects M calls
// like T calls.
func M(msgid string) string { return msgid }

// Marked translates a message that M marked, where the text is held in a
// variable (T takes literals only, so the template sees every message).
func Marked(msgid string) string { return T(msgid) }

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

// Tr translates with one catalog, chosen by the caller: code that writes
// both for people and for machines (a proposal's rendering is shown at the
// command line and logged by the daemon, sent to MCP clients) takes a Tr
// instead of the process-wide catalog. The zero Tr is the English
// reference text; Local is the reader's language. The catalog template
// collects tr.T and tr.N calls (a variable or field named tr) like i18n.T
// and i18n.N calls.
type Tr struct{ c *Catalog }

// Local is the reader's language: the catalog of the environment.
func Local() Tr { return Tr{c: get()} }

// With translates with c (tests, or a catalog loaded by hand).
func With(c *Catalog) Tr { return Tr{c: c} }

// T returns the translation of msgid, or msgid.
func (t Tr) T(msgid string) string {
	if t.c != nil {
		if f := t.c.Messages[msgid]; len(f) > 0 && f[0] != "" {
			return f[0]
		}
	}
	return msgid
}

// Marked translates a message that M marked, held in a variable.
func (t Tr) Marked(msgid string) string { return t.T(msgid) }

// N returns the singular or plural form for n.
func (t Tr) N(singular, plural string, n int) string {
	if t.c != nil {
		if f := t.c.Messages[singular]; len(f) > 0 && t.c.Plural != nil {
			i := t.c.Plural(n)
			if i >= 0 && i < len(f) && f[i] != "" {
				return f[i]
			}
		}
	}
	if n == 1 {
		return singular
	}
	return plural
}

// English reports whether t writes the reference text (no catalog was
// found for the reader's language).
func (t Tr) English() bool { return t.c == nil || t.c.Lang == "" }

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
