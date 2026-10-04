// Package i18n is the installer's translation catalog: GNU gettext
// message catalogs (.mo), domain "basalt-installer", looked up in
// /usr/share/locale/<lang>/LC_MESSAGES/ for the language of LANGUAGE,
// LC_ALL, LC_MESSAGES or LANG. English is the reference language: the
// msgid is the English text and is returned when no translation exists.
//
// Every user-facing string goes through T (whole sentences, never
// concatenated pieces); logs and machine output stay English.
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

var (
	once    sync.Once
	catalog map[string]string
)

// T returns the translation of msgid in the current language, or msgid.
func T(msgid string) string {
	once.Do(func() { catalog = load(LocaleDir, languages()) })
	if s, ok := catalog[msgid]; ok && s != "" {
		return s
	}
	return msgid
}

// languages lists the candidate catalogs for the environment, most
// specific first ("pt_BR.UTF-8" gives "pt_BR", then "pt").
func languages() []string {
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

func load(dir string, langs []string) map[string]string {
	for _, l := range langs {
		b, err := os.ReadFile(filepath.Join(dir, l, "LC_MESSAGES", Domain+".mo"))
		if err != nil {
			continue
		}
		if m, err := ParseMO(b); err == nil {
			return m
		}
	}
	return map[string]string{}
}

// ParseMO reads a GNU .mo catalog into msgid -> msgstr (for plural
// entries, the singular msgid and the first form).
func ParseMO(b []byte) (map[string]string, error) {
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
	m := make(map[string]string, n)
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
			continue // header
		}
		m[strings.SplitN(id, "\x00", 2)[0]] = strings.SplitN(s, "\x00", 2)[0]
	}
	return m, nil
}
