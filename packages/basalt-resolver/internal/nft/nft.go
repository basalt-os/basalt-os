// Package nft keeps the Basalt egress table: per-session default-deny
// chains matched by cgroup, and per-session sets of the addresses the
// resolver handed out for allowed names.
//
//	table inet basalt_egress
//	  chain output      filter hook output: a jump per session, matched by
//	                    "socket cgroupv2 level N <session cgroup>"
//	  chain output_nat  nat hook output: a jump per session that redirects
//	                    the session's DNS (to the system stub resolver) to
//	                    the session's own resolver port
//	  chain s_<id>      the session policy, ending in log + drop
//	  chain s_<id>_nat  the DNS redirect
//	  set s_<id>_v4     ipv4_addr . inet_service, with timeouts
//	  set s_<id>_v6     ipv6_addr . inet_service, with timeouts
//
// Changes are applied with the nft command (one atomic transaction per
// script). The table is never deleted by the service: if the resolver
// stops, the session chains stay and keep dropping (fail closed); on start
// the resolver reconciles them with its saved state.
package nft

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Table is the nftables table the resolver owns.
const Table = "basalt_egress"

// Ports used by the session rules.
const (
	ProxyPorts    = "47100-47163" // basalt-agent session proxies (loopback)
	ResolverPorts = "47200-47263" // session resolver ports (loopback)
)

// StubResolvers are the loopback DNS addresses whose traffic is
// redirected to the session resolver: systemd-resolved's stubs, and
// 127.0.0.1, which the C library and Go use when /etc/resolv.conf cannot
// be read (agents are not allowed to read it).
var StubResolvers = []string{"127.0.0.53", "127.0.0.54", "127.0.0.1"}

// Runner applies nft scripts and lists the ruleset as JSON.
type Runner interface {
	Apply(script string) error
	JSON(args ...string) ([]byte, error)
}

// Exec runs /usr/sbin/nft.
type Exec struct{ Path string }

func (e Exec) bin() string {
	if e.Path != "" {
		return e.Path
	}
	return "/usr/sbin/nft"
}

// Apply runs nft -f - with script on stdin.
func (e Exec) Apply(script string) error {
	cmd := exec.Command(e.bin(), "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// JSON runs nft -j args.
func (e Exec) JSON(args ...string) ([]byte, error) {
	cmd := exec.Command(e.bin(), append([]string{"-j"}, args...)...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nft -j %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out, nil
}

// Session is what the ruleset needs to know about one session.
type Session struct {
	Name     string // nft-safe name, e.g. s_b4896c08ff03
	Cgroup   string // cgroup v2 path, absolute ("/user.slice/...")
	DNSPort  int    // the session resolver port on 127.0.0.1
	Loopback bool   // allow loopback to unprivileged ports (local dev servers)
	LogGroup int    // nflog group for drops
}

var nameRe = regexp.MustCompile(`^s_[a-z0-9_]{1,40}$`)

// ValidName reports whether s can name a session's chains and sets.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Level is the cgroup level of path for "socket cgroupv2 level".
func Level(path string) int {
	return len(strings.Split(strings.Trim(path, "/"), "/"))
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, ``) + `"` }

// LogPrefix is the nflog prefix of a session's drops; kind is "drop" or
// "dns" (a DNS server other than the session resolver).
func LogPrefix(name, kind string) string { return "basalt-egress " + name + " " + kind }

// ParsePrefix splits a LogPrefix.
func ParsePrefix(p string) (name, kind string, ok bool) {
	f := strings.Fields(strings.TrimRight(p, "\x00 "))
	if len(f) != 3 || f[0] != "basalt-egress" || !ValidName(f[1]) {
		return "", "", false
	}
	return f[1], f[2], true
}

// BaseScript creates the table and its two base chains if missing.
func BaseScript() string {
	return fmt.Sprintf(`add table inet %[1]s
add chain inet %[1]s output { type filter hook output priority filter; policy accept; }
add chain inet %[1]s output_nat { type nat hook output priority dstnat; policy accept; }
`, Table)
}

// AddScript adds one session: sets, chains, policy, and the two jumps.
func AddScript(s Session) (string, error) {
	if !ValidName(s.Name) {
		return "", fmt.Errorf("bad session name %q", s.Name)
	}
	if !strings.HasPrefix(s.Cgroup, "/") || strings.ContainsAny(s.Cgroup, "\"\n ") {
		return "", fmt.Errorf("bad cgroup path %q", s.Cgroup)
	}
	if s.DNSPort < 1 || s.DNSPort > 65535 {
		return "", fmt.Errorf("bad DNS port %d", s.DNSPort)
	}
	t, n := Table, s.Name
	cg := fmt.Sprintf("socket cgroupv2 level %d %s", Level(s.Cgroup), quote(strings.TrimPrefix(s.Cgroup, "/")))
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("add set inet %s %s_v4 { type ipv4_addr . inet_service; flags timeout; }", t, n)
	w("add set inet %s %s_v6 { type ipv6_addr . inet_service; flags timeout; }", t, n)
	w("add chain inet %s %s", t, n)
	w("add chain inet %s %s_nat", t, n)
	for _, stub := range StubResolvers {
		w("add rule inet %s %s_nat ip daddr %s meta l4proto { tcp, udp } th dport 53 redirect to :%d", t, n, stub, s.DNSPort)
	}
	// The session policy, in order.
	w("add rule inet %s %s ct state established,related accept", t, n)
	w("add rule inet %s %s ip daddr 127.0.0.1 meta l4proto { tcp, udp } th dport %d accept", t, n, s.DNSPort)
	w("add rule inet %s %s meta l4proto { tcp, udp } th dport { 53, 853, 5353 } limit rate 10/second burst 20 packets log group %d prefix %s", t, n, s.LogGroup, quote(LogPrefix(n, "dns")))
	w("add rule inet %s %s meta l4proto { tcp, udp } th dport { 53, 853, 5353 } drop", t, n)
	w("add rule inet %s %s oifname \"lo\" tcp dport %s accept", t, n, ProxyPorts)
	w("add rule inet %s %s oifname \"lo\" meta l4proto { tcp, udp } th dport %s drop", t, n, ResolverPorts)
	if s.Loopback {
		w("add rule inet %s %s oifname \"lo\" meta l4proto { tcp, udp } th dport 1024-65535 accept", t, n)
	}
	w("add rule inet %s %s meta l4proto { tcp, udp } ip daddr . th dport @%s_v4 accept", t, n, n)
	w("add rule inet %s %s meta l4proto { tcp, udp } ip6 daddr . th dport @%s_v6 accept", t, n, n)
	w("add rule inet %s %s limit rate 10/second burst 20 packets log group %d prefix %s", t, n, s.LogGroup, quote(LogPrefix(n, "drop")))
	w("add rule inet %s %s drop", t, n)
	// The jumps, tagged with the session name so they can be found again.
	w("add rule inet %s output %s jump %s comment %s", t, cg, n, quote(n))
	w("add rule inet %s output_nat %s jump %s_nat comment %s", t, cg, n, quote(n))
	return b.String(), nil
}

// Element is an address and port the session may reach until it expires.
type Element struct {
	IP      net.IP
	Port    int
	Timeout time.Duration
}

// ElementsScript adds elements or refreshes their timeout. "add element"
// keeps an existing element's old timeout, so each element is added,
// deleted and added again in the same transaction: the result is one
// element with the new timeout whether or not it existed.
func ElementsScript(name string, els []Element) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("bad session name %q", name)
	}
	var b strings.Builder
	for _, e := range els {
		if e.Port < 1 || e.Port > 65535 {
			return "", fmt.Errorf("bad port %d", e.Port)
		}
		set, ip := name+"_v6", e.IP.To16()
		if v4 := e.IP.To4(); v4 != nil {
			set, ip = name+"_v4", v4
		}
		if ip == nil {
			return "", fmt.Errorf("bad address %v", e.IP)
		}
		secs := max(int(e.Timeout.Seconds()), 1)
		key := fmt.Sprintf("%s . %d", ip, e.Port)
		fmt.Fprintf(&b, "add element inet %s %s { %s timeout %ds }\n", Table, set, key, secs)
		fmt.Fprintf(&b, "delete element inet %s %s { %s }\n", Table, set, key)
		fmt.Fprintf(&b, "add element inet %s %s { %s timeout %ds }\n", Table, set, key, secs)
	}
	return b.String(), nil
}

// Manager applies session changes through a Runner.
type Manager struct {
	R Runner
}

// Init creates the table and base chains (kept if they exist).
func (m Manager) Init() error { return m.R.Apply(BaseScript()) }

// Add installs a session (atomic: all of it or nothing).
func (m Manager) Add(s Session) error {
	script, err := AddScript(s)
	if err != nil {
		return err
	}
	return m.R.Apply(script)
}

// AddElements adds or refreshes set elements of a session.
func (m Manager) AddElements(name string, els []Element) error {
	if len(els) == 0 {
		return nil
	}
	script, err := ElementsScript(name, els)
	if err != nil {
		return err
	}
	return m.R.Apply(script)
}

// nftJSON is the subset of `nft -j list ...` output used here.
type nftJSON struct {
	Nftables []map[string]json.RawMessage `json:"nftables"`
}

type ruleJSON struct {
	Chain   string `json:"chain"`
	Handle  int    `json:"handle"`
	Comment string `json:"comment"`
}

type namedJSON struct {
	Name string `json:"name"`
}

// Remove deletes a session's jumps, chains and sets (whatever of them
// exists), in one transaction.
func (m Manager) Remove(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("bad session name %q", name)
	}
	st, err := m.State()
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range st.Jumps[name] {
		fmt.Fprintf(&b, "delete rule inet %s %s handle %d\n", Table, r.Chain, r.Handle)
	}
	for _, c := range []string{name, name + "_nat"} {
		if st.Chains[c] {
			fmt.Fprintf(&b, "flush chain inet %s %s\ndelete chain inet %s %s\n", Table, c, Table, c)
		}
	}
	for _, s := range []string{name + "_v4", name + "_v6"} {
		if st.Sets[s] {
			fmt.Fprintf(&b, "delete set inet %s %s\n", Table, s)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return m.R.Apply(b.String())
}

// State is what the table holds now.
type State struct {
	Exists bool
	Chains map[string]bool
	Sets   map[string]bool
	Jumps  map[string][]Jump // by session name (the rule comment)
}

// Jump is a rule in a base chain that jumps to a session chain.
type Jump struct {
	Chain  string
	Handle int
}

// Sessions returns the session names present in the table, sorted.
func (s State) Sessions() []string {
	seen := map[string]bool{}
	for c := range s.Chains {
		if ValidName(c) && !strings.HasSuffix(c, "_nat") {
			seen[c] = true
		}
	}
	for n := range s.Jumps {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// State reads the table.
func (m Manager) State() (State, error) {
	st := State{Chains: map[string]bool{}, Sets: map[string]bool{}, Jumps: map[string][]Jump{}}
	out, err := m.R.JSON("-a", "list", "table", "inet", Table)
	if err != nil {
		if strings.Contains(err.Error(), "No such file") {
			return st, nil
		}
		return st, err
	}
	return ParseState(out)
}

// ParseState reads `nft -j -a list table inet basalt_egress` output.
func ParseState(out []byte) (State, error) {
	st := State{Chains: map[string]bool{}, Sets: map[string]bool{}, Jumps: map[string][]Jump{}}
	var doc nftJSON
	if err := json.Unmarshal(out, &doc); err != nil {
		return st, fmt.Errorf("nft json: %w", err)
	}
	for _, obj := range doc.Nftables {
		for k, raw := range obj {
			switch k {
			case "table":
				st.Exists = true
			case "chain":
				var c namedJSON
				if json.Unmarshal(raw, &c) == nil {
					st.Chains[c.Name] = true
				}
			case "set":
				var s namedJSON
				if json.Unmarshal(raw, &s) == nil {
					st.Sets[s.Name] = true
				}
			case "rule":
				var r ruleJSON
				if json.Unmarshal(raw, &r) == nil && (r.Chain == "output" || r.Chain == "output_nat") && ValidName(r.Comment) {
					st.Jumps[r.Comment] = append(st.Jumps[r.Comment], Jump{Chain: r.Chain, Handle: r.Handle})
				}
			}
		}
	}
	return st, nil
}
