// Package session holds what one basalt-agent session needs: its id, its
// SELinux MCS level (unique among running sessions), the per-user
// directories, project checks and the per-profile lock.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dirs are the per-user locations. They are fixed below $HOME (no XDG
// overrides) because the SELinux file contexts name these paths.
type Dirs struct {
	Home    string // the user's home
	Runtime string // $XDG_RUNTIME_DIR/basalt-agent: one directory per running session
	State   string // ~/.local/state/basalt-agent: audit log
	Data    string // ~/.local/share/basalt-agent: home/PROFILE, tools/PROFILE
	Config  string // ~/.config/basalt-agent: profiles/, secrets/
}

// UserDirs returns the directories of the current user.
func UserDirs() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, err
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return Dirs{
		Home:    home,
		Runtime: filepath.Join(rt, "basalt-agent"),
		State:   filepath.Join(home, ".local/state/basalt-agent"),
		Data:    filepath.Join(home, ".local/share/basalt-agent"),
		Config:  filepath.Join(home, ".config/basalt-agent"),
	}, nil
}

// AgentHome is the per-profile home directory given to the agent.
func (d Dirs) AgentHome(profile string) string { return filepath.Join(d.Data, "home", profile) }

// Tools is the per-profile tool directory for native mode.
func (d Dirs) Tools(profile string) string { return filepath.Join(d.Data, "tools", profile) }

// SecretDir holds the per-profile secret files (SELinux type
// basalt_agent_secret_t: no agent domain may read it).
func (d Dirs) SecretDir() string { return filepath.Join(d.Config, "secrets") }

// SecretFile is the per-profile secret store.
func (d Dirs) SecretFile(profile string) string {
	return filepath.Join(d.SecretDir(), profile+".env")
}

// AuditLog is the session audit log.
func (d Dirs) AuditLog() string { return filepath.Join(d.State, "audit.jsonl") }

var idRe = regexp.MustCompile(`^s-[0-9a-f]{12}$`)

// ValidID reports whether s is a session id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// NewID returns a random session id.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "s-" + hex.EncodeToString(b)
}

// RandomToken returns a random secret for the proxy password.
func RandomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Info is written to RUNTIME/ID/session.json while a session runs.
type Info struct {
	ID      string    `json:"id"`
	Profile string    `json:"profile"`
	Mode    string    `json:"mode"`
	Level   string    `json:"level"`
	Project string    `json:"project"`
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Proxy   string    `json:"proxy,omitempty"`
}

// Running lists the sessions whose launcher is still alive.
func (d Dirs) Running() []Info {
	m, _ := filepath.Glob(filepath.Join(d.Runtime, "s-*", "session.json"))
	var out []Info
	for _, f := range m {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var in Info
		if json.Unmarshal(b, &in) != nil || !ValidID(in.ID) {
			continue
		}
		if syscall.Kill(in.PID, 0) != nil {
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Level formats an MCS level with two categories.
func Level(c1, c2 int) string {
	if c1 > c2 {
		c1, c2 = c2, c1
	}
	return fmt.Sprintf("s0:c%d,c%d", c1, c2)
}

// PickLevel returns a random two-category level not used by any level in used.
func PickLevel(used []string) (string, error) {
	taken := map[string]bool{}
	for _, u := range used {
		taken[u] = true
	}
	for i := 0; i < 1000; i++ {
		a, _ := rand.Int(rand.Reader, big.NewInt(1024))
		b, _ := rand.Int(rand.Reader, big.NewInt(1024))
		if a.Int64() == b.Int64() {
			continue
		}
		l := Level(int(a.Int64()), int(b.Int64()))
		if !taken[l] {
			return l, nil
		}
	}
	return "", fmt.Errorf("no free MCS level")
}

// ParseLevel checks a level of the form s0:cA,cB.
func ParseLevel(s string) (int, int, error) {
	rest, ok := strings.CutPrefix(s, "s0:c")
	if !ok {
		return 0, 0, fmt.Errorf("bad level %q", s)
	}
	a, b, ok := strings.Cut(rest, ",c")
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || x < 0 || y > 1023 || x >= y {
		return 0, 0, fmt.Errorf("bad level %q", s)
	}
	return x, y, nil
}

// Sensitive names that must never be inside a project given to an agent.
var sensitive = []string{".ssh", ".gnupg", ".password-store", ".mozilla", ".config/google-chrome",
	".config/chromium", ".local/share/keyrings", ".aws", ".kube", ".docker"}

// CheckProject validates a project directory and returns its clean,
// symlink-free absolute path. A project must be a directory owned by the
// user, below the user's home or /srv, and must not be the home itself,
// contain it, or contain credentials directories.
func CheckProject(dir string, d Dirs) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", real)
	}
	if s, ok := st.Sys().(*syscall.Stat_t); !ok || int(s.Uid) != os.Getuid() {
		return "", fmt.Errorf("%s is not owned by you", real)
	}
	home, err := filepath.EvalSymlinks(d.Home)
	if err != nil {
		return "", err
	}
	inside := func(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }
	if !(inside(real, home) && real != home) && !inside(real, "/srv") {
		return "", fmt.Errorf("%s: a project must be a directory below your home or /srv", real)
	}
	if inside(home, real) {
		return "", fmt.Errorf("%s contains your home directory", real)
	}
	data, _ := filepath.EvalSymlinks(filepath.Dir(d.Data))
	cfg, _ := filepath.EvalSymlinks(filepath.Dir(d.Config))
	for _, p := range []string{data, cfg} {
		if p != "" && (inside(real, p) || inside(p, real)) {
			return "", fmt.Errorf("%s overlaps %s", real, p)
		}
	}
	for _, s := range sensitive {
		if _, err := os.Lstat(filepath.Join(real, s)); err == nil {
			return "", fmt.Errorf("%s contains %s; refusing to give it to an agent", real, s)
		}
	}
	return real, nil
}

// Lock takes an exclusive lock on path (created if needed), without
// waiting. The returned function releases it.
func Lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another session holds %s", path)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
