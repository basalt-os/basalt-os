package explain

import (
	"regexp"
	"strings"
)

// Redacted replaces what is removed before facts leave the machine.
const Redacted = "[redacted]"

// Before facts are sent to a remote model: secrets, tokens, keys and
// personal content are replaced. Unit names, system paths, SELinux types,
// numbers and package names stay: they are what the text is about.
var redactRules = []*regexp.Regexp{
	// key=value and key: value secrets (password=..., token: ..., api_key=...)
	regexp.MustCompile(`(?i)\b((?:pass(?:word|wd|phrase)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth(?:orization)?|bearer|session|cookie|credential)s?\s*[=:]\s*)((?i:bearer\s+|basic\s+)?(?:"[^"]*"|'[^']*'|\S+))`),
	// Authorization headers and bearer tokens
	regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{8,})`),
	// PEM blocks
	regexp.MustCompile(`(?s)()-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	// JWTs
	regexp.MustCompile(`()\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	// URLs with credentials, e-mail addresses
	regexp.MustCompile(`()\b[a-z][a-z0-9+.-]*://[^\s/@]+:[^\s/@]+@\S+`),
	regexp.MustCompile(`()\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`),
	// IPv4 and IPv6 addresses
	regexp.MustCompile(`()\b(?:\d{1,3}\.){3}\d{1,3}\b`),
	regexp.MustCompile(`()\b(?:[0-9a-fA-F]{1,4}:){5,7}[0-9a-fA-F]{1,4}\b|[0-9a-fA-F]{0,4}::[0-9a-fA-F:]{1,40}\b`),
}

// Home directories name people: /home/alice/x becomes /home/[redacted]/x.
var reHome = regexp.MustCompile(`(/home/|/var/home/|/root/)([^/\s"']+)`)

// Long random-looking strings (keys, tokens): 32 or more characters
// mixing letters and digits, without the separators of paths and names.
var reLong = regexp.MustCompile(`[A-Za-z0-9+_=-]{32,}`)

func randomLooking(s string) bool {
	var lower, upper, digit bool
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		}
	}
	return digit && (lower || upper)
}

// RedactString applies the rules to one value.
func RedactString(s string) string {
	for _, re := range redactRules {
		s = re.ReplaceAllString(s, "${1}"+Redacted)
	}
	s = reLong.ReplaceAllStringFunc(s, func(m string) string {
		if randomLooking(m) {
			return Redacted
		}
		return m
	})
	s = reHome.ReplaceAllStringFunc(s, func(m string) string {
		sub := reHome.FindStringSubmatch(m)
		if sub[1] == "/root/" {
			return m // root's home names no person
		}
		return sub[1] + Redacted
	})
	return s
}

// Redact returns a copy of the facts and changes with every value
// redacted, and the number of values that changed.
func Redact(f *Facts, changes []Change) (*Facts, []Change, int) {
	n := 0
	g := *f
	g.Values = map[string]string{}
	for k, v := range f.Values {
		r := RedactString(v)
		if r != v {
			n++
		}
		g.Values[k] = r
	}
	if r := RedactString(f.Subject); r != f.Subject {
		g.Subject, n = r, n+1
	}
	out := make([]Change, len(changes))
	for i, c := range changes {
		ps := map[string]string{}
		for k, v := range c.Params {
			r := RedactString(v)
			if r != v {
				n++
			}
			ps[k] = r
		}
		out[i] = Change{Kind: c.Kind, Params: ps}
	}
	return &g, out, n
}

// HasRedaction reports a text that repeats a redaction marker.
func HasRedaction(s string) bool {
	return strings.Contains(s, Redacted) || strings.Contains(s, "redacted")
}
