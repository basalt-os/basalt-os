// Package profile reads basalt-agent profiles: INI-style files with
// [section] headers and "key = value" lines; list keys may repeat.
//
//	[agent]      name, description, command, args (space separated)
//	[install]    method (npm, pip, none), package, packages (dnf packages for
//	             the container image and for native mode)
//	[env]        KEY = value, set for the agent in both modes
//	[secrets]    env = NAME (repeatable): variables taken from the secret
//	             store and injected for one session only
//	[egress]     allow = name[:ports] (repeatable, allowlist format),
//	             include = SET (repeatable): a shared list from the egress
//	             directory (SET.list),
//	             loopback = yes|no (the session may reach unprivileged
//	             loopback ports, e.g. a dev server it started, which also
//	             covers the host's own addresses), default yes
//
// Profiles are looked up in the user's directory first
// (~/.config/basalt-agent/profiles), then in /usr/share/basalt-agent/profiles.
package profile

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
)

// Paths says where profiles and shared egress lists live, in lookup order.
type Paths struct {
	ProfileDirs []string
	EgressDirs  []string
}

// Profile is one agent's confinement profile.
type Profile struct {
	Name        string
	Description string
	Command     string
	Args        []string
	Method      string // npm, pip, none
	Package     string
	Packages    []string // dnf packages
	Env         map[string]string
	SecretEnv   []string
	Allow       []allowlist.Entry // own entries
	Includes    []string
	Egress      []allowlist.Entry // own entries plus resolved includes
	Loopback    bool              // unprivileged loopback ports allowed
	Source      string
}

var (
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	envRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pkgRe  = regexp.MustCompile(`^[A-Za-z0-9@._/+-]+$`)
)

// Forbidden environment names: the launcher owns these.
var reservedEnv = map[string]bool{
	"HOME": true, "PATH": true, "HTTPS_PROXY": true, "HTTP_PROXY": true, "https_proxy": true,
	"http_proxy": true, "ALL_PROXY": true, "all_proxy": true, "NO_PROXY": true, "no_proxy": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "SSH_AUTH_SOCK": true, "DBUS_SESSION_BUS_ADDRESS": true,
}

// ValidName reports whether s can be a profile name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// Find returns the path of profile name, user directory first.
func (p Paths) Find(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("bad profile name %q", name)
	}
	for _, d := range p.ProfileDirs {
		f := filepath.Join(d, name+".conf")
		if _, err := os.Stat(f); err == nil {
			return f, nil
		}
	}
	return "", fmt.Errorf("no profile %q (basalt-agent list)", name)
}

// Load finds, parses and resolves profile name.
func (p Paths) Load(name string) (*Profile, error) {
	f, err := p.Find(name)
	if err != nil {
		return nil, err
	}
	pr, err := ParseFile(f)
	if err != nil {
		return nil, err
	}
	if pr.Name != name {
		return nil, fmt.Errorf("%s: name = %q, want %q", f, pr.Name, name)
	}
	return pr, p.Resolve(pr)
}

// Resolve fills Egress from the profile's entries and its includes.
func (p Paths) Resolve(pr *Profile) error {
	l := allowlist.New(pr.Allow)
	for _, inc := range pr.Includes {
		es, err := p.LoadSet(inc)
		if err != nil {
			return fmt.Errorf("profile %s: %w", pr.Name, err)
		}
		for _, e := range es {
			l.Add(e)
		}
	}
	pr.Egress = l.Entries()
	return nil
}

// LoadSet reads a shared egress list (SET.list).
func (p Paths) LoadSet(set string) ([]allowlist.Entry, error) {
	if !ValidName(set) {
		return nil, fmt.Errorf("bad egress set name %q", set)
	}
	for _, d := range p.EgressDirs {
		f := filepath.Join(d, set+".list")
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		defer fh.Close()
		es, err := allowlist.Parse(fh)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		return es, nil
	}
	return nil, fmt.Errorf("no egress set %q", set)
}

// List returns every profile name visible through the directories.
func (p Paths) List() []string {
	seen := map[string]bool{}
	for _, d := range p.ProfileDirs {
		m, _ := filepath.Glob(filepath.Join(d, "*.conf"))
		for _, f := range m {
			n := strings.TrimSuffix(filepath.Base(f), ".conf")
			if ValidName(n) {
				seen[n] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseFile parses one profile file (includes are not resolved).
func ParseFile(path string) (*Profile, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	pr := &Profile{Env: map[string]string{}, Method: "none", Source: path, Loopback: true}
	sc := bufio.NewScanner(fh)
	section, n := "", 0
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s:%d: %s", path, n, fmt.Sprintf(format, a...))
	}
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			switch section {
			case "agent", "install", "env", "secrets", "egress":
			default:
				return nil, fail("unknown section [%s]", section)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fail("expected key = value")
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch section + "." + k {
		case "agent.name":
			pr.Name = v
		case "agent.description":
			pr.Description = v
		case "agent.command":
			pr.Command = v
		case "agent.args":
			pr.Args = strings.Fields(v)
		case "install.method":
			pr.Method = v
		case "install.package":
			pr.Package = v
		case "install.packages":
			pr.Packages = append(pr.Packages, strings.Fields(v)...)
		case "secrets.env":
			if !envRe.MatchString(v) || reservedEnv[v] {
				return nil, fail("bad secret variable %q", v)
			}
			pr.SecretEnv = append(pr.SecretEnv, v)
		case "egress.allow":
			e, err := allowlist.ParseEntry(v)
			if err != nil {
				return nil, fail("%v", err)
			}
			pr.Allow = append(pr.Allow, e)
		case "egress.include":
			pr.Includes = append(pr.Includes, v)
		case "egress.loopback":
			b, ok := map[string]bool{"yes": true, "true": true, "1": true, "no": false, "false": false, "0": false}[v]
			if !ok {
				return nil, fail("%s must be yes or no", k)
			}
			pr.Loopback = b
		default:
			if section == "env" {
				if !envRe.MatchString(k) || reservedEnv[k] {
					return nil, fail("variable %q cannot be set by a profile", k)
				}
				pr.Env[k] = v
				continue
			}
			return nil, fail("unknown key %q in [%s]", k, section)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return pr, pr.validate()
}

func (pr *Profile) validate() error {
	if !ValidName(pr.Name) {
		return fmt.Errorf("%s: bad or missing name", pr.Source)
	}
	if pr.Command == "" || strings.ContainsAny(pr.Command, "/ \t") {
		return fmt.Errorf("%s: command must be a program name looked up in PATH", pr.Source)
	}
	switch pr.Method {
	case "npm", "pip":
		if pr.Package == "" || !pkgRe.MatchString(pr.Package) {
			return fmt.Errorf("%s: install method %s needs a valid package", pr.Source, pr.Method)
		}
	case "none":
	default:
		return fmt.Errorf("%s: unknown install method %q", pr.Source, pr.Method)
	}
	for _, p := range pr.Packages {
		if !pkgRe.MatchString(p) {
			return fmt.Errorf("%s: bad dnf package %q", pr.Source, p)
		}
	}
	return nil
}
