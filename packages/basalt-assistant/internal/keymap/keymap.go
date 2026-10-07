// Package keymap is the system's keyboard: what systemd-localed keeps for
// the login screen, the text console and new accounts (the X11 keymap in
// /etc/X11/xorg.conf.d/00-keyboard.conf, the console keymap in
// /etc/vconsole.conf). The keyboard.system action changes it with
// localectl, in the assistant's executor only. Every layout, variant and
// option is checked against the XKB registry the system ships
// (xkeyboard-config's evdev.xml) and the console keymap against the kbd
// keymaps that exist, so a proposal can only name real ones.
package keymap

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Paths of the system's keyboard data (tests point them elsewhere).
var (
	RegistryPaths = []string{"/usr/share/X11/xkb/rules/evdev.xml", "/usr/share/X11/xkb/rules/evdev.extras.xml"}
	KeymapDirs    = []string{"/usr/lib/kbd/keymaps"}
	ModelMap      = "/usr/share/systemd/kbd-model-map"
	X11Conf       = "/etc/X11/xorg.conf.d/00-keyboard.conf"
	VConsoleConf  = "/etc/vconsole.conf"
)

// MaxLayouts is XKB's limit of layouts at once.
const MaxLayouts = 4

var (
	reLayout  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	reVariant = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,47}$`)
	reOption  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}:[A-Za-z0-9_+.-]{1,48}$`)
	reChoice  = regexp.MustCompile(`^([a-z][a-z0-9_-]{0,31})(?:\(([A-Za-z0-9][A-Za-z0-9_.+-]{0,47})\))?$`)
	reModel   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// ReKeymap is a console keymap name ("br-abnt2", "us-acentos").
	ReKeymap = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,63}$`)
)

// Choice is a layout with an optional variant: "br", "us(intl)".
type Choice struct {
	Layout  string `json:"layout"`
	Variant string `json:"variant,omitempty"`
}

func (c Choice) String() string {
	if c.Variant == "" {
		return c.Layout
	}
	return c.Layout + "(" + c.Variant + ")"
}

// ParseLayouts reads "br,us(intl)": one to MaxLayouts layouts, no twice.
func ParseLayouts(s string) ([]Choice, error) {
	var out []Choice
	seen := map[Choice]bool{}
	for _, part := range strings.Split(s, ",") {
		m := reChoice.FindStringSubmatch(part)
		if m == nil {
			return nil, fmt.Errorf("%q is not a keyboard layout (like br or us(intl))", part)
		}
		c := Choice{Layout: m[1], Variant: m[2]}
		if seen[c] {
			return nil, fmt.Errorf("the layout %s is listed twice", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 || len(out) > MaxLayouts {
		return nil, fmt.Errorf("1 to %d keyboard layouts, not %d", MaxLayouts, len(out))
	}
	return out, nil
}

// FormatLayouts is the canonical form of a list: "br,us(intl)".
func FormatLayouts(cs []Choice) string {
	p := make([]string, len(cs))
	for i, c := range cs {
		p[i] = c.String()
	}
	return strings.Join(p, ",")
}

// XKBLists are localectl's comma-separated layout and variant lists
// ("br,us" and ",intl"; the variant list is empty when none has one).
func XKBLists(cs []Choice) (layout, variant string) {
	ls, vs := make([]string, len(cs)), make([]string, len(cs))
	has := false
	for i, c := range cs {
		ls[i], vs[i] = c.Layout, c.Variant
		has = has || c.Variant != ""
	}
	if has {
		variant = strings.Join(vs, ",")
	}
	return strings.Join(ls, ","), variant
}

// ParseOptions reads "grp:alt_shift_toggle,compose:ralt" ("" is none).
func ParseOptions(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	out := strings.Split(s, ",")
	if len(out) > 16 {
		return nil, fmt.Errorf("too many XKB options (%d)", len(out))
	}
	for _, o := range out {
		if !reOption.MatchString(o) {
			return nil, fmt.Errorf("%q is not an XKB option", o)
		}
	}
	return out, nil
}

// Registry is what the system's XKB data lists.
type Registry struct {
	names    map[string]string            // layout -> description
	variants map[string]map[string]string // layout -> variant -> description
	options  map[string]bool
}

// LoadRegistry reads the registries that exist (at least one must).
func LoadRegistry() (*Registry, error) {
	r := &Registry{names: map[string]string{}, variants: map[string]map[string]string{}, options: map[string]bool{}}
	type item struct {
		Name        string `xml:"name"`
		Description string `xml:"description"`
	}
	var x struct {
		Layouts []struct {
			Item     item `xml:"configItem"`
			Variants []struct {
				Item item `xml:"configItem"`
			} `xml:"variantList>variant"`
		} `xml:"layoutList>layout"`
		Groups []struct {
			Options []struct {
				Item item `xml:"configItem"`
			} `xml:"option"`
		} `xml:"optionList>group"`
	}
	read := 0
	for _, p := range RegistryPaths {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		x.Layouts, x.Groups = nil, nil
		if err := xml.Unmarshal(b, &x); err != nil {
			return nil, fmt.Errorf("%s: %v", filepath.Base(p), err)
		}
		read++
		for _, l := range x.Layouts {
			n := strings.TrimSpace(l.Item.Name)
			if !reLayout.MatchString(n) {
				continue
			}
			if _, ok := r.names[n]; !ok {
				r.names[n] = strings.TrimSpace(l.Item.Description)
				r.variants[n] = map[string]string{}
			}
			for _, v := range l.Variants {
				if vn := strings.TrimSpace(v.Item.Name); reVariant.MatchString(vn) {
					r.variants[n][vn] = strings.TrimSpace(v.Item.Description)
				}
			}
		}
		for _, g := range x.Groups {
			for _, o := range g.Options {
				if n := strings.TrimSpace(o.Item.Name); reOption.MatchString(n) {
					r.options[n] = true
				}
			}
		}
	}
	if read == 0 {
		return nil, fmt.Errorf("no XKB registry on this system (%s)", strings.Join(RegistryPaths, ", "))
	}
	return r, nil
}

// Check refuses layouts, variants and options the registry does not list.
func (r *Registry) Check(cs []Choice, options []string) error {
	for _, c := range cs {
		if _, ok := r.names[c.Layout]; !ok {
			return fmt.Errorf("%q is not a keyboard layout of this system", c.Layout)
		}
		if c.Variant != "" {
			if _, ok := r.variants[c.Layout][c.Variant]; !ok {
				return fmt.Errorf("%q is not a variant of the keyboard layout %q", c.Variant, c.Layout)
			}
		}
	}
	for _, o := range options {
		if !r.options[o] {
			return fmt.Errorf("the XKB option %s is not available on this system", o)
		}
	}
	return nil
}

// Describe is the registry's name of a layout ("Portuguese (Brazil)").
func (r *Registry) Describe(c Choice) string {
	if c.Variant != "" {
		if d := r.variants[c.Layout][c.Variant]; d != "" {
			return d
		}
	} else if d := r.names[c.Layout]; d != "" {
		return d
	}
	return c.String()
}

// CheckModel accepts an XKB model name ("pc105").
func CheckModel(m string) error {
	if !reModel.MatchString(m) {
		return fmt.Errorf("%q is not a keyboard model", m)
	}
	return nil
}

// KeymapExists reports a console keymap kbd can load.
func KeymapExists(name string) bool {
	if !ReKeymap.MatchString(name) {
		return false
	}
	found := false
	for _, d := range KeymapDirs {
		_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err != nil || found {
				return nil
			}
			if !e.IsDir() {
				b := e.Name()
				if b == name+".map" || b == name+".map.gz" || b == name+".map.bz2" || b == name+".map.xz" || b == name+".map.zst" {
					found = true
					return fs.SkipAll
				}
			}
			return nil
		})
	}
	return found
}

// ConsoleKeymap is the console keymap for a list of layouts, the way
// systemd-localed converts an X11 keymap: the keymap kbd generated from
// the first layout and variant ("us-intl"), else the legacy map's entry
// for that layout and variant ("br-abnt2"), else the layout's own keymap,
// else "us".
func ConsoleKeymap(cs []Choice) string {
	if len(cs) == 0 {
		return "us"
	}
	first := cs[0]
	if first.Variant != "" && KeymapExists(first.Layout+"-"+first.Variant) {
		return first.Layout + "-" + first.Variant
	}
	if first.Variant == "" && KeymapExists(first.Layout) {
		return first.Layout
	}
	if km := legacyKeymap(first); km != "" && KeymapExists(km) {
		return km
	}
	if KeymapExists(first.Layout) {
		return first.Layout
	}
	return "us"
}

// legacyKeymap reads systemd's kbd-model-map: "console xlayout xmodel
// xvariant xoptions" per line.
func legacyKeymap(c Choice) string {
	f, err := os.Open(ModelMap)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fl := strings.Fields(line)
		if len(fl) < 4 {
			continue
		}
		layout, _, _ := strings.Cut(fl[1], ",")
		variant, _, _ := strings.Cut(fl[3], ",")
		if variant == "-" {
			variant = ""
		}
		if layout == c.Layout && variant == c.Variant {
			return fl[0]
		}
	}
	return ""
}

// Current is the system's keyboard as localed keeps it.
type Current struct {
	Layouts []Choice `json:"layouts"`
	Model   string   `json:"model"`
	Options []string `json:"options"`
	Console string   `json:"console"`
}

// ReadCurrent reads the system's keyboard from localed's files.
func ReadCurrent() Current {
	var c Current
	vals := map[string]string{}
	re := regexp.MustCompile(`^\s*Option\s+"(Xkb[A-Za-z]+)"\s+"([^"]*)"`)
	if b, err := os.ReadFile(X11Conf); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if m := re.FindStringSubmatch(line); m != nil {
				if _, ok := vals[m[1]]; !ok {
					vals[m[1]] = m[2]
				}
			}
		}
	}
	if b, err := os.ReadFile(VConsoleConf); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == "KEYMAP" {
				c.Console = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	var variants []string
	if v := vals["XkbVariant"]; v != "" {
		variants = strings.Split(v, ",")
	}
	if l := vals["XkbLayout"]; l != "" {
		for i, name := range strings.Split(l, ",") {
			ch := Choice{Layout: strings.TrimSpace(name)}
			if i < len(variants) {
				ch.Variant = strings.TrimSpace(variants[i])
			}
			c.Layouts = append(c.Layouts, ch)
		}
	}
	c.Model = vals["XkbModel"]
	if o := vals["XkbOptions"]; o != "" {
		c.Options = strings.Split(o, ",")
	}
	return c
}
