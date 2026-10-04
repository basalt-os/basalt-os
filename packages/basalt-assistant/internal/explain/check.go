package explain

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// DefaultMaxChars bounds the prose a model may write.
const DefaultMaxChars = 600

// Allowed is everything a text about one finding may mention: the values of
// its facts and the parameters of its planned changes. A model's text that
// names a number, path, unit, SELinux type or boolean, proposal id or
// command outside this set is rejected.
type Allowed struct {
	corpus  string
	numbers map[string]bool
	paths   []string
	words   map[string]bool
	subject string
	values  []string // the values themselves, longest first
	ok      bool     // nothing is wrong
	changes bool     // a change is planned (or hinted)
}

var (
	reNumber  = regexp.MustCompile(`\d+(?:[.,]\d+)*`)
	rePath    = regexp.MustCompile(`(?:^|[\s("'=])(/[^\s,;)"'\x60]*)`)
	reUnitRef = regexp.MustCompile(`\b[A-Za-z0-9@_:\\-][A-Za-z0-9@._:\\-]*\.(?:service|socket|timer|mount|path|target|slice|scope)\b`)
	reSEType  = regexp.MustCompile(`\b[a-z][a-z0-9_]*_t\b`)
	reSnake   = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+){2,}\b`)
	reIDRef   = regexp.MustCompile(`\bp-[0-9a-f]{6}\b`)
	// File names without a directory (nginx.conf, key.pem).
	reFileRef = regexp.MustCompile(`\b[A-Za-z0-9_][A-Za-z0-9_.-]*\.(?:conf|cfg|cf|ini|xml|json|ya?ml|toml|log|pem|key|crt|db|sock|rpm|repo|env|sh|py)\b`)
	reURL     = regexp.MustCompile(`(?i)\b(?:https?|ftp)://|(?:^|\s)www\.|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reWord    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_-]*`)
	reLetters = regexp.MustCompile(`[a-z]+`)
	// Claims that contradict the facts' shape: a change when none is
	// planned, a fault when nothing is wrong, health when something is.
	reApplyClaim  = regexp.MustCompile(`(?i)\bappl(?:y|ying|ied)\b|\bthe (?:planned |proposed )?(?:fix|change)s? (?:will|would)\b`)
	reFaultClaim  = regexp.MustCompile(`(?i)\b(?:fail\w*|error\w*|crash\w*|problem\w*|issue\w*|broken|denied|blocked|wrong)\b`)
	reHealthClaim = regexp.MustCompile(`(?i)\brunning (?:normally|fine)\b|\bno (?:issues?|problems?)\b|\bnothing (?:is )?wrong\b|\bnot experiencing\b|\bnothing to (?:do|fix)\b`)
	// Advice the assistant never gives, whatever the facts say.
	reForbidden = regexp.MustCompile(`(?i)\b(?:disabl\w*|turn\w* off|switch\w* off)\s+(?:the\s+)?selinux\b|\bpermissive mode\b|\bsetenforce\b|\baudit2allow\b|\bchmod\s+777\b|\bcurl\b.*\|\s*(?:ba)?sh\b`)
)

// Command words a text may not use unless the facts do (commands belong
// in the template's fixed slots).
var commandWords = map[string]bool{"systemctl": true, "semanage": true, "restorecon": true, "setsebool": true, "chcon": true,
	"chmod": true, "chown": true, "rm": true, "dnf": true, "yum": true, "rpm": true, "snapper": true, "journalctl": true,
	"kill": true, "killall": true, "reboot": true, "sudo": true, "basalt": true, "basalt-rollback": true, "mv": true, "cp": true,
	"sed": true, "curl": true, "wget": true, "firewall-cmd": true, "iptables": true, "nft": true, "mkfs": true, "dd": true,
	"btrfs": true, "passwd": true, "useradd": true, "usermod": true, "ssh": true, "scp": true}

var numberWords = map[string]string{"zero": "0", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7",
	"eight": "8", "nine": "9", "ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13", "fourteen": "14", "fifteen": "15",
	"sixteen": "16", "seventeen": "17", "eighteen": "18", "nineteen": "19", "twenty": "20", "thirty": "30", "forty": "40",
	"fifty": "50", "sixty": "60", "seventy": "70", "eighty": "80", "ninety": "90", "hundred": "100", "thousand": "1000",
	"million": "1000000", "billion": "1000000000", "dozen": "12", "half": "0.5", "twice": "2", "double": "2"}

// NewAllowed collects what a text about f and its changes may mention.
func NewAllowed(f *Facts, changes []action.Action) *Allowed {
	var parts []string
	parts = append(parts, f.Subject)
	for _, k := range f.Keys() {
		parts = append(parts, f.Values[k])
	}
	for _, a := range changes {
		for _, v := range a.Params {
			parts = append(parts, v)
		}
	}
	al := &Allowed{corpus: strings.Join(parts, "\n"), numbers: map[string]bool{}, words: map[string]bool{}, subject: f.Subject,
		ok: f.OK, changes: len(changes) > 0}
	for _, v := range parts {
		if len(v) >= 3 {
			al.values = append(al.values, v)
		}
	}
	sort.Slice(al.values, func(i, j int) bool { return len(al.values[i]) > len(al.values[j]) })
	for _, n := range reNumber.FindAllString(al.corpus, -1) {
		al.numbers[normNumber(n)] = true
		for _, piece := range strings.FieldsFunc(n, func(r rune) bool { return r == '.' || r == ',' }) {
			al.numbers[normNumber(piece)] = true
		}
	}
	for _, m := range rePath.FindAllStringSubmatch(al.corpus, -1) {
		al.paths = append(al.paths, trimPunct(m[1]))
	}
	for _, w := range reWord.FindAllString(strings.ToLower(al.corpus), -1) {
		al.words[w] = true
	}
	return al
}

func normNumber(s string) string {
	s = strings.ReplaceAll(s, ",", "")
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	t := strings.TrimLeft(s, "0")
	if t == "" || strings.HasPrefix(t, ".") {
		t = "0" + t
	}
	return t
}

func trimPunct(s string) string { return strings.TrimRight(s, ".,;:!?'\")") }

func (al *Allowed) hasPath(p string) bool {
	for _, a := range al.paths {
		if p == a {
			return true
		}
		// A leading part of a known path, ending at a component boundary.
		if strings.HasPrefix(a, p) && (strings.HasSuffix(p, "/") || a[len(p)] == '/') {
			return true
		}
	}
	return false
}

func (al *Allowed) inCorpus(s string) bool { return strings.Contains(al.corpus, s) }

// Check lists what a text mentions that the facts do not hold, and other
// reasons to reject it. An empty list means the text is faithful.
func (al *Allowed) Check(text string, maxChars int) []string {
	var bad []string
	add := func(what, v string) { bad = append(bad, what+" "+strconv.Quote(v)) }
	if strings.TrimSpace(text) == "" {
		return []string{"empty text"}
	}
	if maxChars > 0 && len(text) > maxChars {
		bad = append(bad, "longer than "+strconv.Itoa(maxChars)+" characters")
	}
	for _, r := range text {
		if r > 0x24F && !strings.ContainsRune("‘’“”", r) {
			bad = append(bad, "non-Latin character "+strconv.QuoteRune(r))
			break
		}
	}
	// Characters a value itself holds (a quoted config line) are fine
	// where the text quotes that value.
	quoted := text
	for _, v := range al.values {
		quoted = strings.ReplaceAll(quoted, v, " ")
	}
	if strings.ContainsAny(quoted, "`#*$|<>{}[]") {
		bad = append(bad, "markup or command characters")
	}
	if m := reURL.FindString(quoted); m != "" {
		add("address", m)
	}
	if m := reForbidden.FindString(text); m != "" {
		add("forbidden advice", m)
	}
	rest := text
	for _, m := range rePath.FindAllStringSubmatch(text, -1) {
		p := trimPunct(m[1])
		if p == "/" || p == "" {
			continue
		}
		if !al.hasPath(p) {
			add("path", p)
		}
		rest = strings.Replace(rest, p, " ", 1)
	}
	for _, u := range reUnitRef.FindAllString(rest, -1) {
		if !al.inCorpus(u) {
			add("unit", u)
		}
	}
	for _, fn := range reFileRef.FindAllString(rest, -1) {
		if !al.inCorpus(fn) {
			add("file", fn)
		}
	}
	for _, id := range reIDRef.FindAllString(rest, -1) {
		if !al.inCorpus(id) {
			add("proposal id", id)
		}
	}
	for _, t := range reSEType.FindAllString(rest, -1) {
		if !al.inCorpus(t) {
			add("SELinux type", t)
		}
	}
	for _, t := range reSnake.FindAllString(rest, -1) {
		if !al.inCorpus(t) {
			add("name", t)
		}
	}
	// Numbers: outside paths and names, as digits or words.
	scrub := reUnitRef.ReplaceAllString(rest, " ")
	scrub = reIDRef.ReplaceAllString(scrub, " ")
	for _, n := range reNumber.FindAllString(scrub, -1) {
		n = strings.TrimRight(n, ".,")
		if n == "" {
			continue
		}
		if !al.numbers[normNumber(n)] {
			add("number", n)
		}
	}
	for _, w := range reLetters.FindAllString(strings.ToLower(scrub), -1) {
		if d, ok := numberWords[w]; ok && !al.numbers[d] && !al.words[w] {
			add("number", w)
		}
	}
	for _, w := range reWord.FindAllString(strings.ToLower(scrub), -1) {
		if commandWords[w] && !al.words[w] {
			add("command", w)
		}
	}
	if !al.changes {
		if m := reApplyClaim.FindString(text); m != "" {
			add("claims a change, none is planned:", m)
		}
	}
	if al.ok {
		if m := reFaultClaim.FindString(text); m != "" {
			add("claims a fault, nothing is wrong:", m)
		}
	} else if m := reHealthClaim.FindString(text); m != "" {
		add("claims all is well, it is not:", m)
	}
	if al.subject != "" && !mentions(text, al.subject) {
		add("does not name", al.subject)
	}
	return dedup(bad)
}

// mentions reports the subject or, for a unit, its name without the
// suffix (nginx for nginx.service).
func mentions(text, subject string) bool {
	lt := strings.ToLower(text)
	s := strings.ToLower(subject)
	if strings.Contains(lt, s) {
		return true
	}
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		stem := s[:i]
		idx := strings.Index(lt, stem)
		for idx >= 0 {
			end := idx + len(stem)
			if (idx == 0 || !isWordRune(rune(lt[idx-1]))) && (end == len(lt) || !isWordRune(rune(lt[end]))) {
				return true
			}
			next := strings.Index(lt[end:], stem)
			if next < 0 {
				break
			}
			idx = end + next
		}
	}
	return false
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

func dedup(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

var reEmphasis = regexp.MustCompile("`+|\\*\\*|__")

// Normalize removes the formatting chat models add around names (code
// quotes, bold) and joins lines; the names themselves are still checked.
func Normalize(s string) string {
	s = reEmphasis.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}
