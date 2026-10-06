// Package peer establishes who is on the other end of the gate's socket,
// from the kernel only: uid and pid (SO_PEERCRED), the SELinux context
// (SO_PEERSEC), and the basalt-agent session from the process's cgroup.
// Nothing a client says about itself changes what it is classified as;
// a trusted relay (the shell daemon, the system assistant) may only say
// on whose behalf it asks, within the kinds its configuration lists.
package peer

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Peer is a socket client as the kernel sees it.
type Peer struct {
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	PID     int    `json:"pid"`
	Context string `json:"context,omitempty"`
}

// Type returns the type field of the SELinux context ("" without SELinux).
func (p Peer) Type() string { return ContextType(p.Context) }

// ContextType returns the type field of an SELinux context.
func ContextType(ctx string) string {
	f := strings.Split(ctx, ":")
	if len(f) >= 3 {
		return f[2]
	}
	return ""
}

// Of returns the kernel's view of a Unix socket client.
func Of(c *net.UnixConn) (Peer, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var p Peer
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		p.UID, p.GID, p.PID = int(cred.Uid), int(cred.Gid), int(cred.Pid)
		buf := make([]byte, 256)
		n := uint32(len(buf))
		_, _, e2 := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_SOCKET, syscall.SO_PEERSEC,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0)
		if e2 == 0 && n > 0 && int(n) <= len(buf) {
			p.Context = strings.TrimRight(string(buf[:n]), "\x00")
		}
	})
	if err != nil {
		return p, err
	}
	return p, cerr
}

// Roles a peer may hold.
type Roles struct {
	// Kind and Name of the peer as a requester (agent, system-assistant,
	// app, tool).
	Kind string
	Name string
	// Session is the basalt-agent session (agents).
	Session string
	// Decider: may decide requests; Polkit: every decision is checked with
	// polkit for the peer's process (the terminal decider), else only C4
	// and C5 decisions are.
	Decider bool
	Polkit  bool
	// Relay: kinds the peer may report as the requester it relays for.
	Relay []string
	// Agent: the peer is in an agent domain (requester only, never a
	// decider, an executor of someone else's action or a rule writer).
	Agent bool
}

// Config is the classification configuration (gate.conf).
type Config struct {
	// Deciders are SELinux types that decide in a session (the shell UI,
	// the Approvals app); TTYDeciders are terminal deciders (polkit every
	// time).
	Deciders    []string
	TTYDeciders []string
	// Relays maps an SELinux type to the requester kinds it may report.
	Relays map[string][]string
	// AgentPrefixes mark agent domains (type prefixes).
	AgentPrefixes []string
	// SystemAssistant types ask as the system assistant.
	SystemAssistant []string
	// AppPrefix marks desktop app domains (basalt_app_<name>_t).
	AppPrefix string
	// RootRelays are the requester kinds root (uid 0) outside the agent
	// domains may report (the system assistant's command line, run as
	// root, asks as the system assistant).
	RootRelays []string
}

// DefaultConfig is the packaged classification.
func DefaultConfig() Config {
	return Config{
		Deciders:        []string{"basalt_shell_ui_t", "basalt_app_approvals_t"},
		TTYDeciders:     []string{"basalt_gate_tty_t"},
		Relays:          map[string][]string{"basalt_shell_t": {"person", "assistant", "agent", "app"}, "basalt_assistant_t": {"system-assistant", "agent"}},
		AgentPrefixes:   []string{"basalt_agent", "container_", "basalt_skill"},
		SystemAssistant: []string{"basalt_assistant_t"},
		AppPrefix:       "basalt_app_",
		RootRelays:      []string{"system-assistant"},
	}
}

func has(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Classify returns a peer's roles. client is the name the client gave
// in hello (used only to name a tool, never to raise its roles).
func (c Config) Classify(p Peer, client string) Roles {
	t := p.Type()
	r := Roles{Kind: "tool", Name: ToolName(client)}
	for _, pre := range c.AgentPrefixes {
		if t != "" && strings.HasPrefix(t, pre) {
			r.Kind, r.Name, r.Agent = "agent", "", true
			r.Session = SessionOf(p.PID)
			return r
		}
	}
	if has(c.Deciders, t) {
		r.Decider = true
	}
	if has(c.TTYDeciders, t) {
		r.Decider, r.Polkit = true, true
	}
	if kinds, ok := c.Relays[t]; ok {
		r.Relay = append(r.Relay, kinds...)
	}
	if p.UID == 0 {
		for _, k := range c.RootRelays {
			if !has(r.Relay, k) {
				r.Relay = append(r.Relay, k)
			}
		}
	}
	switch {
	case has(c.SystemAssistant, t):
		r.Kind, r.Name = "system-assistant", "basalt-assistant"
	case c.AppPrefix != "" && strings.HasPrefix(t, c.AppPrefix):
		r.Kind, r.Name = "app", strings.TrimSuffix(strings.TrimPrefix(t, c.AppPrefix), "_t")
	}
	return r
}

var toolRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}`)

// ToolName returns the tool part of a hello client string
// ("tui-systemd/0.4.0" -> "tui-systemd"); "cli" when there is none.
func ToolName(client string) string {
	name, _, _ := strings.Cut(client, "/")
	if m := toolRe.FindString(strings.ToLower(name)); m != "" {
		return m
	}
	return "cli"
}

var sliceRe = regexp.MustCompile(`basaltagent-([0-9a-f]{12})\.slice`)

// ProcRoot is /proc (tests point it elsewhere).
var ProcRoot = "/proc"

// SessionOf returns the basalt-agent session a process runs in, from its
// cgroup (basaltagent-<id>.slice), or "".
func SessionOf(pid int) string {
	f, err := os.Open(fmt.Sprintf("%s/%d/cgroup", ProcRoot, pid))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := sliceRe.FindStringSubmatch(sc.Text()); m != nil {
			return "s-" + m[1]
		}
	}
	return ""
}

// StartTime returns a process's start time in clock ticks after boot
// (field 22 of /proc/PID/stat), which polkit uses with the pid to name a
// process unambiguously.
func StartTime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", ProcRoot, pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("unreadable stat of %d", pid)
	}
	f := strings.Fields(s[i+1:])
	// After ")": state is field 3, so starttime (22) is index 19.
	if len(f) < 20 {
		return 0, fmt.Errorf("short stat of %d", pid)
	}
	return strconv.ParseUint(f[19], 10, 64)
}
