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
	// What a denial was about (internal/report).
	i18n.M("%s port %s"),
	// Inside the reports' sentences.
	i18n.M("no name in the key"),
	i18n.M("a change you confirmed"),
	i18n.M("the change %s"),
}

// phrases are the other short texts a proposal stores in English and a
// person reads: what each check of an apply proves (action.Verify, stored
// in the result) and why a change is only a hint (action.Hint.Reason).
// They are kept apart from the titles: "%s is %s" would match too much.
var phrases = []string{
	// The checks of internal/action.
	i18n.M("the policy default for %s is %s"),
	i18n.M("%s carries its default label"),
	i18n.M("%s port %s is %s"),
	i18n.M("%s is %s"),
	i18n.M("%s is active"),
	i18n.M("no new SELinux denials since the change"),
	i18n.M("%s matches snapshot %s"),
	i18n.M("a rollback is pending: the next boot uses a copy of snapshot %s"),
	i18n.M("deleted subvolumes cleaned up (space released)"),
	i18n.M("snapshot %s is gone"),
	i18n.M("journal disk usage"),
	i18n.M("dnf cache cleaned"),
	i18n.M("the basalt-nonfree repository is on"),
	i18n.M("%s is installed"),
	i18n.M("nouveau is off from the next start"),
	i18n.M("an installed kernel has the signed NVIDIA module"),
	i18n.M("the next start is a trial of the NVIDIA driver"),
	i18n.M("the updates wait for the next start"),
	i18n.M("the %s updates are installed"),
	i18n.M("the audit wrote its results (%s)"),
	i18n.M("the risk of %s is accepted"),
	i18n.M("the risk of %s is no longer accepted"),
	i18n.M("the %s channel is on"),
	i18n.M("the %s channel is off"),
	i18n.M("the source %s is recorded with its key"),
	i18n.M("the system's keyboard is %s, console keymap %s"),
	i18n.M("the source %s is gone"),
	// The reasons of hints (internal/diag, internal/engine, internal/mcp,
	// internal/cli).
	i18n.M("snapshot %d holds a different copy of %s (%s)"),
	i18n.M("snapshot %d was taken just before the failed transaction \"%s\""),
	i18n.M("found by the confined daemon, which cannot check the snapshot"),
	i18n.M("proposed through MCP; the confined view cannot check the snapshot"),
	i18n.M("proposed by %s before hints existed"),
}

// sentences are the sentences of the reports stored in English with the
// proposals that carry no facts (updates, channels, sources, the
// keyboard, the security page, the MCP server's typed actions, an apply
// that did not finish): Report translates a stored report sentence by
// sentence.
var sentences = []string{
	// internal/updates.
	i18n.M("%d updates are ready: %d security, %d Basalt OS components, %d apps, %d system."),
	i18n.M("A snapshot is taken first, so the update can be undone from Settings, Updates and channels, or with sudo basalt updates rollback."),
	i18n.M("Some of them replace core parts of the system (%s), so they install while the computer restarts, before the desktop starts; a snapshot is taken before and after."),
	i18n.M("Undo the last update (%s, %d packages, %s): the system goes back to snapshot %d, taken just before it, at the next start."),
	i18n.M("Your files in /home and the other data volumes stay as they are."),
	// Channels and sources (internal/cli).
	i18n.M("Turn the %s channel off: no more updates come from it."),
	i18n.M("What is installed from it stays."),
	i18n.M("Turn the basalt-testing channel on: preview builds of Basalt OS components, before they reach everyone."),
	i18n.M("Turn the basalt-nonfree-testing channel on: preview builds of the additional drivers (the NVIDIA driver), for testers with that hardware."),
	i18n.M("Preview builds can break things."),
	i18n.M("A snapshot is taken before each update so you can go back."),
	i18n.M("Turn the %s channel on: updates from now on also come from it."),
	i18n.M("Its packages are signed with the OpenBasalt release key."),
	i18n.M("Remove %s: no more software comes from it, and its signing key is no longer trusted."),
	i18n.M("Programs already installed from it stay; they no longer get updates."),
	i18n.M("Add %s."),
	i18n.M("Software from this source can change your whole system: only add sources you trust."),
	i18n.M("Its signing key, %s, belongs to %s."),
	i18n.M("Basalt pins this key for %s."),
	i18n.M("Basalt does not know this source: check the fingerprint with its publisher before you trust it."),
	i18n.M("Its packages and its package lists are signed."),
	i18n.M("Its packages are signed; its package lists are not (the source does not sign them), so they come from a metalink over HTTPS."),
	// The keyboard (internal/cli).
	i18n.M("The login screen, the text console and accounts created from now on type with %s (the first is the default; the console uses the keymap %s)."),
	i18n.M("People who chose their own layouts in Settings, Keyboard keep them."),
	i18n.M("To change it again, choose other layouts the same way."),
	// The security page (internal/cli).
	i18n.M("Run the public AI audit suite on this computer: like an attacker would, it tries to get around the protections for AI agents (their confinement, their network limits, the activity record, the confirmation step)."),
	i18n.M("It runs as its own unprivileged account with fake test data and changes none of your files."),
	i18n.M("The results appear in Security and Activity."),
	i18n.M("The suite is not installed yet: it is installed first from the Basalt repositories, checked against their signing key."),
	i18n.M("Record that %s knows about the warning on %s and accepts it."),
	i18n.M("Security and Activity then shows it as accepted, with the date and who accepted it, and stops counting it."),
	i18n.M("If it ever becomes a problem it shows as a problem again."),
	i18n.M("Review again undoes this."),
	i18n.M("Forget that the risk of %s was accepted: Security and Activity counts it as a warning again."),
	// The MCP server and basalt confirm.
	i18n.M("Proposed by an MCP client: %s"),
	i18n.M("Confirmed as root: %s."),
	// An apply that did not finish (internal/report FromExecutor).
	i18n.M("Applying %s stopped before it was done."),
	i18n.M("Applying %s stopped before it was done, and its log does not say why."),
	i18n.M("The log says: %s."),
	i18n.M("The change itself (%s) still waits for you: open it to try again, or dismiss it."),
	i18n.M("Nothing will be changed by this report."),
}

// A verb of a form: %s, %d, %.0f and the like; %% is a percent sign.
var reVerb = regexp.MustCompile(`%[-+# 0]*[0-9]*(?:\.[0-9]+)?[a-zA-Z%]`)

type pattern struct {
	form string
	re   *regexp.Regexp
}

var (
	once                                     sync.Once
	formPats, valPat, phrasePat, sentencePat []pattern
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
		formPats, valPat, phrasePat, sentencePat = compile(forms), compile(values), compile(phrases), compile(sentences)
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
func match(tr i18n.Tr, s string, pats []pattern, depth int) (string, bool) {
	for _, p := range pats {
		m := p.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		form := tr.Marked(p.form)
		if form == p.form && len(m) == 1 {
			return s, true
		}
		vals := make([]string, len(m)-1)
		for i, v := range m[1:] {
			vals[i] = value(tr, v, depth)
		}
		return fill(form, vals), true
	}
	return s, false
}

// value translates one value of a title when it is a known phrase or,
// once, a title itself (the quoted title of a change that did not finish).
func value(tr i18n.Tr, v string, depth int) string {
	_, vals := patterns()
	if t, ok := match(tr, v, vals, depth); ok {
		return t
	}
	if depth > 0 {
		return local(tr, v, depth-1)
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
	return local(i18n.Local(), title, 1)
}

// In returns title in the language of tr (English: title itself).
func In(tr i18n.Tr, title string) string {
	return local(tr, title, 1)
}

func local(tr i18n.Tr, title string, depth int) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return title
	}
	fs, _ := patterns()
	if out, ok := match(tr, t, fs, depth); ok {
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
			if s, ok := match(tr, strings.Join(parts[i:j], "; "), fs, depth); ok {
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

// Phrase returns a stored check description or hint reason in the
// language of tr, or s itself when it is not a known phrase.
func Phrase(tr i18n.Tr, s string) string {
	patterns()
	if out, ok := match(tr, strings.TrimSpace(s), phrasePat, 1); ok {
		return out
	}
	return s
}

// KnownPhrase reports whether s is a check description or hint reason this
// package knows: the producers' tests check their English against it.
func KnownPhrase(s string) bool {
	patterns()
	for _, p := range phrasePat {
		if p.re.MatchString(s) {
			return true
		}
	}
	return false
}

// Report returns a stored report in the language of tr, sentence by
// sentence: a known sentence is translated with its values (a value that
// is a known phrase, check, reason or title too), any other stays as it
// is. English returns s itself.
func Report(tr i18n.Tr, s string) string {
	if tr.English() || strings.TrimSpace(s) == "" {
		return s
	}
	patterns()
	parts := splitSentences(s)
	changed := false
	for i, p := range parts {
		for _, pat := range sentencePat {
			m := pat.re.FindStringSubmatch(p)
			if m == nil {
				continue
			}
			vals := make([]string, len(m)-1)
			for j, v := range m[1:] {
				vals[j] = reportValue(tr, v)
			}
			parts[i] = fill(tr.Marked(pat.form), vals)
			changed = true
			break
		}
	}
	if !changed {
		return s
	}
	return strings.Join(parts, " ")
}

// KnownReport reports whether every sentence of s is a known sentence.
func KnownReport(s string) bool {
	patterns()
	for _, p := range splitSentences(s) {
		known := false
		for _, pat := range sentencePat {
			known = known || pat.re.MatchString(p)
		}
		if !known {
			return false
		}
	}
	return true
}

// reportValue translates a value of a report sentence: a phrase (a hint
// reason), a value phrase, a title (the quoted title of a change).
func reportValue(tr i18n.Tr, v string) string {
	if out := Phrase(tr, v); out != v {
		return out
	}
	q := len(v) > 1 && v[0] == '"' && v[len(v)-1] == '"'
	if q {
		v = v[1 : len(v)-1]
	}
	out := value(tr, v, 1)
	if q {
		out = `"` + out + `"`
	}
	return out
}

// splitSentences cuts a report after each ". " that a capital letter
// follows; each piece keeps its period.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '.' && s[i+1] == ' ' && s[i+2] >= 'A' && s[i+2] <= 'Z' {
			out = append(out, s[start:i+1])
			start = i + 2
		}
	}
	return append(out, s[start:])
}
