// Package egress connects basalt-agent sessions to basalt-resolver, the
// system service that makes a session's network default-deny in the
// kernel (docs/network.md): every process of the session runs in one
// cgroup (a systemd user slice per session), the resolver matches that
// cgroup with nftables, drops everything the session did not resolve
// through an allowed name, and records what it refused.
package egress

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"time"
)

// ResolverSocket is basalt-resolver's control socket.
var ResolverSocket = "/run/basalt-resolver/control.sock"

// Available reports whether the resolver is installed and listening.
func Available() bool {
	st, err := os.Stat(ResolverSocket)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// SliceName is the systemd user slice of a session: every process of the
// session is started in it (basaltagent.slice/basaltagent-HEX.slice).
func SliceName(sessionID string) string {
	return "basaltagent-" + strings.TrimPrefix(sessionID, "s-") + ".slice"
}

// ScopeArgs returns the systemd-run prefix that starts a command in a new
// scope unit of the session slice. systemd-run moves itself into the scope
// and executes the command in place (same process, same descriptors).
func ScopeArgs(sessionID, role string) []string {
	unit := "basaltagent-" + strings.TrimPrefix(sessionID, "s-") + "-" + role
	return []string{"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--slice=" + SliceName(sessionID), "--unit=" + unit, "--"}
}

// CgroupOf returns the cgroup v2 path of a process.
func CgroupOf(pid int) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return p, nil
		}
	}
	return "", errors.New("no cgroup v2 entry")
}

// SessionSlice returns the session slice path from the cgroup of a
// process started with ScopeArgs, checking it is the expected one.
func SessionSlice(scopePath, sessionID string) (string, error) {
	slice := path.Dir(scopePath)
	if path.Base(slice) != SliceName(sessionID) || !strings.HasSuffix(scopePath, ".scope") {
		return "", fmt.Errorf("process is in %s, not in the session slice %s", scopePath, SliceName(sessionID))
	}
	return slice, nil
}

// Request is a resolver control request (see basalt-resolver).
type Request struct {
	Op       string   `json:"op"`
	Session  string   `json:"session,omitempty"`
	Profile  string   `json:"profile,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Level    string   `json:"level,omitempty"`
	Project  string   `json:"project,omitempty"`
	Cgroup   string   `json:"cgroup,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	Entry    string   `json:"entry,omitempty"`
	Loopback *bool    `json:"loopback,omitempty"`
	ByUID    int      `json:"by_uid,omitempty"`
}

// Reply from the resolver.
type Reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	DNS   string `json:"dns,omitempty"`
}

// Do sends one request to the resolver.
func Do(req Request) (Reply, error) {
	var rep Reply
	c, err := net.DialTimeout("unix", ResolverSocket, 3*time.Second)
	if err != nil {
		return rep, fmt.Errorf("basalt-resolver: %w", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return rep, err
	}
	if err := json.NewDecoder(c).Decode(&rep); err != nil {
		return rep, fmt.Errorf("basalt-resolver: no reply: %w", err)
	}
	if !rep.OK {
		return rep, fmt.Errorf("basalt-resolver: %s", rep.Error)
	}
	return rep, nil
}
