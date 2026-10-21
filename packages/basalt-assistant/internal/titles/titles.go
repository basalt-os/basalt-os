// Package titles shows the titles of proposals in the reader's language.
//
// A proposal's title is written once, in English (the reference text), by
// the daemon, the command line or the MCP server, and stored with the
// proposal; logs, the audit log and the gate keep it as it is. Local turns
// a stored title into the language of the person reading it (the catalog
// of LANGUAGE, LC_ALL, LC_MESSAGES or LANG, see internal/i18n): it matches
// the title against the English forms below, each one a catalog message
// whose verbs (%s, %d, %.0f) stand for the values, translates the form and
// puts the same values back in the same order. A value that is itself a
// known phrase (a cause, "3 updates", a quoted title) is translated too.
// A title of commands joined by "; " (a typed action's description) is
// translated part by part. Text that matches no form stays English, so
// titles of older versions and of names (units, packages) are safe.
package titles

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
)

// forms are whole titles, and the descriptions of the commands a typed
// action runs (action.Describe joins them with "; ").
var forms = []string{
	// Findings of the daemon (internal/report).
	i18n.M("%s stopped: %s"),
	i18n.M("%s is running"),
	i18n.M("SELinux blocked %s (%s) on %s: %s"),
	i18n.M("root file system %.0f %% full: %s"),
	i18n.M("root file system %.0f %% full"),
	i18n.M("package transaction failed: %s"),
	i18n.M("roll back to snapshot %d (%s, \"%s\")"),
	i18n.M("Applying \"%s\" did not finish"),
	i18n.M("Applying a change you confirmed did not finish"),
	i18n.M("Applying the change %s did not finish"),
	// Updates and drivers (internal/updates, internal/drivers).
	i18n.M("install %d updates (%s to download)"),
	i18n.M("install %d security updates (%s to download)"),
	i18n.M("restart and install %d updates (%s to download)"),
	i18n.M("restart and install %d security updates (%s to download)"),
	i18n.M("undo the update %s: back to snapshot %d"),
	i18n.M("install the NVIDIA driver %s for %s"),
	// The command line (internal/cli): Settings stores these.
	i18n.M("turn the %s channel on"),
	i18n.M("turn the %s channel off"),
	i18n.M("add the software source %s"),
	i18n.M("remove the software source %s"),
	i18n.M("use %s for the login screen, the console and new accounts"),
	i18n.M("run the AI audit suite on this computer"),
	i18n.M("accept the risk of %s"),
	i18n.M("review the risk of %s again"),
	// The commands of typed actions (internal/action).
	i18n.M("label %s and everything below it %s (persistent rule)"),
	i18n.M("apply the rule to the existing files"),
	i18n.M("reset the label of %s to the policy default"),
	i18n.M("label %s port %s %s"),
	i18n.M("set the boolean %s %s (persistent)"),
	i18n.M("restart %s"),
	i18n.M("restore %s from snapshot %s"),
	i18n.M("label the restored file"),
	i18n.M("make snapshot %s the root at the next boot (data subvolumes untouched)"),
	i18n.M("delete snapshot %s and free the space only it holds"),
	i18n.M("shrink archived journal files to %s"),
	i18n.M("remove cached package files"),
	i18n.M("install the definition of the basalt-nonfree repository (it comes off)"),
	i18n.M("turn the basalt-nonfree repository on"),
	i18n.M("install %s (NVIDIA's userspace, unmodified, and the signed open kernel modules; nouveau goes off) and the module for the running kernel %s"),
	i18n.M("make the next start a trial: it checks the driver and goes back to nouveau if it fails"),
	i18n.M("download the newest list of packages from every enabled repository (nothing is installed)"),
	i18n.M("download %s (checked against the repositories' signing keys) and prepare them to install at the next start"),
	i18n.M("restart into them (after the desktop's countdown, or sudo basalt updates restart): they install before the desktop starts"),
	i18n.M("download %s (checked against the repositories' signing keys)"),
	i18n.M("install %s, exactly the versions shown"),
	i18n.M("undo the update %s: make snapshot %s, taken just before it, the root at the next boot (your files untouched)"),
	i18n.M("install the definition of the basalt-nonfree channels (they come off)"),
	i18n.M("bring that definition up to date (it gains the testing channel)"),
	i18n.M("add the source %s (%s), trusting its signing key %s after checking it again"),
	i18n.M("remove the source %s (and its key, when no other repository of it is left)"),
	i18n.M("make %s the keyboard layouts of the login screen and of new accounts"),
	i18n.M("use the keymap %s in the text console"),
	i18n.M("install the AI audit suite (%s) from the Basalt repositories, checked against their signing key"),
	i18n.M("run the AI audit suite on this computer, as its own unprivileged account, and keep the results in %s"),
	i18n.M("record that %s accepts the risk of %s on this computer (%s)"),
	i18n.M("forget the accepted risk of %s: it shows as a warning again"),
}

// values are phrases that appear only as a value inside a title: the
// causes of internal/report and the update counts of the actions.
var values = []string{
	i18n.M("configuration error"),
	i18n.M("blocked by SELinux"),
	i18n.M("port already in use"),
	i18n.M("a unit it needs failed"),
	i18n.M("disk full"),
	i18n.M("missing file"),
	i18n.M("crashed"),
	i18n.M("keeps crashing"),
	i18n.M("out of memory"),
	i18n.M("file permissions"),
	i18n.M("no such unit"),
	i18n.M("cause unknown"),
	i18n.M("wrong file label"),
	i18n.M("no label rule for this place"),
	i18n.M("port not labeled for it"),
	i18n.M("a boolean is off"),
	i18n.M("no known fix"),
	i18n.M("looks deliberate"),
	i18n.M("snapshots hold the space"),
	i18n.M("large journal"),
	i18n.M("large package cache"),
	i18n.M("data"),
	i18n.M("%s updates"),
	i18n.M("%s security updates"),
}

// A verb of a form: %s, %d, %.0f and the like; %% is a percent sign.
var reVerb = regexp.MustCompile(`%[-+# 0]*[0-9]*(?:\.[0-9]+)?[a-zA-Z%]`)

type pattern struct {
	form string
	re   *regexp.Regexp
}

var (
	once             sync.Once
	formPats, valPat []pattern
)

// compile turns forms into anchored patterns, the most specific (the
// longest fixed text) first, so "restart into them (...)" wins over
// "restart %s".
func compile(list []string) []pattern {
	out := make([]pattern, 0, len(list))
	for _, f := range list {
		var b strings.Builder
		b.WriteString(`^`)
		last := 0
		for _, m := range reVerb.FindAllStringIndex(f, -1) {
			b.WriteString(regexp.QuoteMeta(f[last:m[0]]))
			switch {
			case f[m[1]-1] == '%':
				b.WriteString(`%`)
			case m[0] > 0 && m[1] < len(f) && f[m[0]-1] == '"' && f[m[1]] == '"':
				// A quoted title: anything.
				b.WriteString(`(.+?)`)
			default:
				// A value never holds "; ", which joins the commands of an
				// action: "install %s (...) and the module for the running
				// kernel %s" must not swallow the commands around it.
				b.WriteString(`((?:[^;]|;[^ ])+?)`)
			}
			last = m[1]
		}
		b.WriteString(regexp.QuoteMeta(f[last:]) + `$`)
		out = append(out, pattern{f, regexp.MustCompile(b.String())})
	}
	fixed := func(f string) int { return len(reVerb.ReplaceAllString(f, "")) }
	sort.SliceStable(out, func(i, j int) bool { return fixed(out[i].form) > fixed(out[j].form) })
	return out
}

func patterns() ([]pattern, []pattern) {
	once.Do(func() {
		formPats, valPat = compile(forms), compile(values)
	})
	return formPats, valPat
}

// fill puts the values in place of the verbs of a translated form, in
// order; %% is a percent sign.
func fill(form string, vals []string) string {
	i := 0
	return reVerb.ReplaceAllStringFunc(form, func(v string) string {
		if v == "%%" {
			return "%"
		}
		if i >= len(vals) {
			return v
		}
		i++
		return vals[i-1]
	})
}

// match translates s with the first pattern it matches; ok is false when
// none does. depth limits the translation of values that are titles.
func match(s string, pats []pattern, depth int) (string, bool) {
	for _, p := range pats {
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		tr := i18n.Marked(p.form)
		if tr == p.form && len(m) == 1 {
			return s, true
		}
		vals := make([]string, len(m)-1)
		for i, v := range m[1:] {
			vals[i] = value(v, depth)
		}
		return fill(tr, vals), true
	}
	return s, false
}

// value translates one value of a title when it is a known phrase or,
// once, a title itself (the quoted title of a change that did not finish).
func value(v string, depth int) string {
	_, vals := patterns()
	if t, ok := match(v, vals, depth); ok {
		return t
	}
	if depth > 0 {
		return local(v, depth-1)
	}
	return v
}

// Known reports whether s is a title or a command description this package
// knows (a form, or parts that are forms), whatever the reader's language:
// the producers' tests check their English against it.
func Known(s string) bool {
	fs, _ := patterns()
	for _, p := range fs {
		if p.re.MatchString(s) {
			return true
		}
	}
	return false
}

// Local returns title in the reader's language, or title itself when the
// catalog has no translation for it.
func Local(title string) string {
	return local(title, 1)
}

func local(title string, depth int) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return title
	}
	fs, _ := patterns()
	if out, ok := match(t, fs, depth); ok {
		return out
	}
	// The commands of an action, joined by "; ": translate each run of
	// parts that makes a known command (a description may itself contain
	// "; "), keep the others.
	parts := strings.Split(t, "; ")
	if len(parts) == 1 {
		return title
	}
	var out []string
	changed := false
	for i := 0; i < len(parts); {
		j := len(parts)
		for ; j > i; j-- {
			if s, ok := match(strings.Join(parts[i:j], "; "), fs, depth); ok {
				out = append(out, s)
				changed = true
				break
			}
		}
		if j == i {
			out = append(out, parts[i])
			j = i + 1
		}
		i = j
	}
	if !changed {
		return title
	}
	return strings.Join(out, "; ")
}
