// Package allowlist implements the egress allowlist format shared by
// basalt-agent's filtering proxy and, later, by a DNS-aware resolver that
// fills per-session nftables sets.
//
// Format: one entry per line, '#' starts a comment.
//
//	api.anthropic.com            exact name, port 443
//	registry.npmjs.org:443,80    exact name, explicit ports
//	*.githubusercontent.com      any name below the suffix (not the suffix itself)
//	ollama.lab.example:11434 private
//	                             the name may resolve to loopback, private or
//	                             link-local addresses (a local model server);
//	                             without "private" such addresses are refused
//
// Names are matched case-insensitively, without a trailing dot. IP
// literals, a bare "*" and wildcards anywhere but the first label are
// rejected: entries are names, never addresses, so the policy keeps its
// meaning when the addresses behind a name change.
package allowlist

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultPorts applies when an entry names no port.
var DefaultPorts = []int{443}

// Entry is one allowlist line.
type Entry struct {
	Pattern string // "name.example" or "*.example"
	Ports   []int
	Private bool // may resolve to non-global addresses
}

// String renders the entry in the file format.
func (e Entry) String() string {
	s := e.Pattern
	if !(len(e.Ports) == 1 && e.Ports[0] == 443) {
		p := make([]string, len(e.Ports))
		for i, v := range e.Ports {
			p[i] = strconv.Itoa(v)
		}
		s += ":" + strings.Join(p, ",")
	}
	if e.Private {
		s += " private"
	}
	return s
}

// Matches reports whether host:port is covered by the entry.
func (e Entry) Matches(host string, port int) bool {
	host = Normalize(host)
	if !portIn(port, e.Ports) {
		return false
	}
	if strings.HasPrefix(e.Pattern, "*.") {
		suffix := e.Pattern[1:] // ".example"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return host == e.Pattern
}

func portIn(p int, ports []int) bool {
	for _, v := range ports {
		if v == p {
			return true
		}
	}
	return false
}

// Normalize lower-cases a host name and drops a trailing dot.
func Normalize(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// ParseEntry parses one entry ("name[:port[,port...]] [private]").
func ParseEntry(s string) (Entry, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Entry{}, fmt.Errorf("empty entry")
	}
	private := false
	if f := strings.Fields(s); len(f) == 2 && f[1] == "private" {
		s, private = f[0], true
	} else if len(f) != 1 {
		return Entry{}, fmt.Errorf("%q: expected name[:ports] [private]", s)
	}
	name, portSpec, hasPorts := strings.Cut(s, ":")
	name = Normalize(name)
	if err := validName(name); err != nil {
		return Entry{}, fmt.Errorf("%q: %w", s, err)
	}
	e := Entry{Pattern: name, Private: private}
	if !hasPorts {
		e.Ports = append([]int(nil), DefaultPorts...)
		return e, nil
	}
	for _, p := range strings.Split(portSpec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 65535 {
			return Entry{}, fmt.Errorf("%q: bad port %q", s, p)
		}
		if !portIn(n, e.Ports) {
			e.Ports = append(e.Ports, n)
		}
	}
	sort.Ints(e.Ports)
	return e, nil
}

func validName(name string) error {
	if name == "" || name == "*" {
		return fmt.Errorf("a name is required")
	}
	if net.ParseIP(strings.Trim(name, "[]")) != nil {
		return fmt.Errorf("IP addresses are not allowed, use a name")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("a fully qualified name is required")
	}
	for i, l := range labels {
		if l == "*" && i == 0 {
			continue
		}
		if l == "" || len(l) > 63 {
			return fmt.Errorf("bad label %q", l)
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return fmt.Errorf("bad character %q (wildcards only as the first label)", c)
			}
		}
	}
	if strings.HasPrefix(name, "*.") && len(labels) < 3 {
		return fmt.Errorf("a wildcard needs at least two labels after it")
	}
	return nil
}

// Parse reads entries from r, one per line, skipping comments and blanks.
func Parse(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		e, err := ParseEntry(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// List is a concurrency-safe set of entries.
type List struct {
	mu      sync.RWMutex
	entries []Entry
}

// New returns a list holding entries.
func New(entries []Entry) *List {
	l := &List{}
	for _, e := range entries {
		l.Add(e)
	}
	return l
}

// Add merges an entry (ports of an existing pattern are joined).
func (l *List) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.entries {
		if l.entries[i].Pattern == e.Pattern {
			for _, p := range e.Ports {
				if !portIn(p, l.entries[i].Ports) {
					l.entries[i].Ports = append(l.entries[i].Ports, p)
				}
			}
			sort.Ints(l.entries[i].Ports)
			l.entries[i].Private = l.entries[i].Private || e.Private
			return
		}
	}
	l.entries = append(l.entries, Entry{Pattern: e.Pattern, Ports: append([]int(nil), e.Ports...), Private: e.Private})
}

// Allows reports whether host:port matches any entry. IP literals never do.
func (l *List) Allows(host string, port int) bool {
	ok, _ := l.Lookup(host, port)
	return ok
}

// Lookup is Allows that also says whether a matching entry permits
// non-global addresses.
func (l *List) Lookup(host string, port int) (allowed, private bool) {
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return false, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.Matches(host, port) {
			allowed = true
			private = private || e.Private
		}
	}
	return allowed, private
}

// Entries returns a sorted copy of the entries.
func (l *List) Entries() []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// Format renders entries in the file format.
func Format(entries []Entry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return b.String()
}
