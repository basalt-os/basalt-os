// Package cli is the basalt-agent command line.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/native"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/podman"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/selinux"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/session"
)

// Version is set at build time.
var Version = "dev"

const usage = `basalt-agent: run AI coding agents confined by SELinux

  basalt-agent run PROFILE --project DIR [--mode container|native] [-- ARGS...]
  basalt-agent list                      profiles, their mode support and egress
  basalt-agent sessions                  running sessions
  basalt-agent audit [--session ID] [--verify] [-n N] [--json]
  basalt-agent audit --avc [--session ID]   SELinux denials (needs root)
  basalt-agent image build PROFILE       build the tool image (container mode)
  basalt-agent install PROFILE           install the agent for native mode
  basalt-agent grant SESSION host NAME[:PORTS]
  basalt-agent grant SESSION path DIR    (native mode) give the session one more directory
                                         grants need administrator authentication (polkit)
  basalt-agent egress PROFILE            the profile's effective network allowlist
  basalt-agent egress propose PROFILE add|remove ENTRY [--system] [--yes]
                                         change a profile's allowlist: preview, confirm, audit
                                         (--system: every user, administrator authentication)
  basalt-agent version

Profiles: ~/.config/basalt-agent/profiles, /etc/basalt-agent/profiles,
/usr/share/basalt-agent/profiles. Secrets: ~/.config/basalt-agent/secrets/PROFILE.env.
`

// Main runs the command line and returns the exit status.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	var err error
	code := 0
	switch args[0] {
	case "run":
		code, err = cmdRun(args[1:])
	case "list":
		err = cmdList()
	case "sessions":
		err = cmdSessions()
	case "audit":
		err = cmdAudit(args[1:])
	case "image":
		err = cmdImage(args[1:])
	case "install":
		err = cmdInstall(args[1:])
	case "grant":
		err = cmdGrant(args[1:])
	case "egress":
		err = cmdEgress(args[1:])
	case "version":
		fmt.Println(Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "basalt-agent: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// sharePaths returns the profile and egress lookup directories.
func sharePaths(d session.Dirs) profile.Paths {
	share := "/usr/share/basalt-agent"
	if s := os.Getenv("BASALT_AGENT_SHARE"); s != "" {
		share = s
	}
	return profile.Paths{
		ProfileDirs: []string{filepath.Join(d.Config, "profiles"), "/etc/basalt-agent/profiles", filepath.Join(share, "profiles")},
		EgressDirs:  []string{filepath.Join(d.Config, "egress"), "/etc/basalt-agent/egress", filepath.Join(share, "egress")},
	}
}

func isTTY(fd int) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}

func cmdList() error {
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	p := sharePaths(d)
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tIMAGE\tNATIVE\tSECRETS\tEGRESS\tDESCRIPTION")
	for _, n := range p.List() {
		pr, err := p.Load(n)
		if err != nil {
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\terror: %v\n", n, err)
			continue
		}
		img := "no"
		if exec.Command("podman", "image", "exists", podman.Image(n)).Run() == nil {
			img = "yes"
		}
		nat := "no"
		if _, err := os.Stat(filepath.Join(d.Tools(n), "bin", pr.Command)); err == nil {
			nat = "yes"
		} else if _, err := os.Stat(filepath.Join(d.Tools(n), "venv/bin", pr.Command)); err == nil {
			nat = "yes"
		}
		var hosts []string
		for _, e := range pr.Egress {
			hosts = append(hosts, e.String())
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d hosts\t%s\n", n, img, nat, strings.Join(pr.SecretEnv, ","), len(hosts), pr.Description)
	}
	return w.Flush()
}

func cmdSessions() error {
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tPROFILE\tMODE\tLEVEL\tSTARTED\tPROJECT")
	for _, s := range d.Running() {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Profile, s.Mode, s.Level, s.Started.Local().Format(time.TimeOnly), s.Project)
	}
	return w.Flush()
}

func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	sid := fs.String("session", "", "only this session")
	verify := fs.Bool("verify", false, "check the hash chain")
	avc := fs.Bool("avc", false, "SELinux denials of agent sessions (root)")
	n := fs.Int("n", 50, "last N records")
	asJSON := fs.Bool("json", false, "raw records")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *avc {
		return auditAVC(*sid)
	}
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	if *verify {
		c, err := audit.Verify(d.AuditLog())
		if err != nil {
			return fmt.Errorf("audit log %s: %w", d.AuditLog(), err)
		}
		fmt.Printf("audit log %s: %d records, chain intact\n", d.AuditLog(), c)
		return nil
	}
	rs, err := audit.Read(d.AuditLog(), *sid)
	if err != nil {
		return err
	}
	if len(rs) > *n {
		rs = rs[len(rs)-*n:]
	}
	for _, r := range rs {
		if *asJSON {
			b, _ := json.Marshal(r)
			fmt.Println(string(b))
			continue
		}
		var kv []string
		keys := make([]string, 0, len(r.Data))
		for k := range r.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			kv = append(kv, fmt.Sprintf("%s=%v", k, r.Data[k]))
		}
		t, _ := time.Parse(time.RFC3339Nano, r.Time)
		fmt.Printf("%s %s %-14s %-7s %s/%s %s\n", t.Local().Format("2006-01-02 15:04:05"), r.Session, r.Event, r.Outcome,
			r.Subject.Profile, r.Subject.Mode, strings.Join(kv, " "))
	}
	return nil
}

// auditAVC prints SELinux denials of agent domains (and, with a session,
// of the container at that session's level).
func auditAVC(sid string) error {
	if os.Geteuid() != 0 {
		return errors.New("SELinux denials are in the system audit log, readable by root: sudo basalt-agent audit --avc")
	}
	out, _ := exec.Command("ausearch", "-m", "AVC,USER_AVC,SELINUX_ERR", "-i", "-ts", "boot").CombinedOutput()
	level := ""
	if sid != "" {
		if !session.ValidID(sid) {
			return fmt.Errorf("bad session id %q", sid)
		}
		// The session's level is in the audit log of the user who ran it.
		u := os.Getenv("SUDO_USER")
		if u != "" {
			home := "/home/" + u
			if rs, err := audit.Read(filepath.Join(home, ".local/state/basalt-agent/audit.jsonl"), sid); err == nil && len(rs) > 0 {
				level = rs[0].Subject.Level
			}
		}
	}
	shown := 0
	for _, block := range strings.Split(string(out), "----") {
		// With a session: its level (native and container denials alike);
		// without: every denial of a basalt_agent domain.
		if level != "" && !strings.Contains(block, level) || level == "" && !strings.Contains(block, "basalt_agent") {
			continue
		}
		fmt.Print(strings.TrimLeft(block, "\n"))
		shown++
	}
	fmt.Fprintf(os.Stderr, "%d denial records\n", shown)
	return nil
}

// relabelSkip keeps the git hooks and configuration read-only for agents:
// they run later with the user's full rights.
var readOnlyPaths = []string{".git/hooks", ".git/config"}

func cmdImage(args []string) error {
	if len(args) != 2 || args[0] != "build" {
		return errors.New("usage: basalt-agent image build PROFILE")
	}
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	pr, err := sharePaths(d).Load(args[1])
	if err != nil {
		return err
	}
	ctx, err := os.MkdirTemp("", "basalt-agent-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ctx)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := copyFile(self, filepath.Join(ctx, "basalt-agent-entry"), 0o755); err != nil {
		return err
	}
	fedora := os.Getenv("BASALT_AGENT_FEDORA")
	if fedora == "" {
		fedora = osReleaseVersion()
	}
	if err := os.WriteFile(filepath.Join(ctx, "Containerfile"), []byte(podman.Containerfile(pr, fedora)), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("podman", "build", "--pull=missing", "-t", podman.Image(pr.Name), "-f", filepath.Join(ctx, "Containerfile"), ctx)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("podman build: %w", err)
	}
	_, err = audit.Open(d.AuditLog()).Append(audit.Record{UID: os.Getuid(), Event: audit.Install,
		Subject: audit.Subject{Profile: pr.Name, Mode: "container"},
		Data:    map[string]any{"method": pr.Method, "package": pr.Package, "target": podman.Image(pr.Name)}})
	return err
}

// osReleaseVersion is the Fedora release the container images build on:
// the major number of VERSION_ID (Basalt OS 44.0 runs on Fedora 44).
func osReleaseVersion() string {
	b, _ := os.ReadFile("/etc/os-release")
	return fedoraFromOSRelease(string(b))
}

func fedoraFromOSRelease(osRelease string) string {
	for _, l := range strings.Split(osRelease, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "VERSION_ID="); ok {
			major, _, _ := strings.Cut(strings.Trim(v, `"'`), ".")
			if _, err := strconv.Atoi(major); err == nil {
				return major
			}
		}
	}
	return "44"
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func cmdInstall(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: basalt-agent install PROFILE")
	}
	d, err := session.UserDirs()
	if err != nil {
		return err
	}
	pr, err := sharePaths(d).Load(args[0])
	if err != nil {
		return err
	}
	tools := d.Tools(pr.Name)
	if err := os.MkdirAll(tools, 0o700); err != nil {
		return err
	}
	if err := installToolsPresent(pr.Method, pr.Packages, exec.LookPath); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch pr.Method {
	case "npm":
		cmd = exec.Command("npm", "install", "-g", "--prefix", tools, "--no-fund", "--no-audit", pr.Package)
	case "pip":
		venv := filepath.Join(tools, "venv")
		c := exec.Command("python3", "-m", "venv", venv)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return err
		}
		cmd = exec.Command(filepath.Join(venv, "bin/pip"), "install", pr.Package)
	default:
		return fmt.Errorf("profile %s has no install method", pr.Name)
	}
	fmt.Fprintf(os.Stderr, "installing %s into %s (needs: %s)\n", pr.Package, tools, strings.Join(pr.Packages, " "))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	if selinux.Enabled() {
		if _, err := selinux.Relabel(tools, native.ToolType, "s0", nil); err != nil {
			return err
		}
	}
	_, err = audit.Open(d.AuditLog()).Append(audit.Record{UID: os.Getuid(), Event: audit.Install,
		Subject: audit.Subject{Profile: pr.Name, Mode: "native"},
		Data:    map[string]any{"method": pr.Method, "package": pr.Package, "target": tools}})
	return err
}

// installToolsPresent checks that the program an install method runs is
// there, and otherwise names the command that installs what the profile
// needs (the desktop edition ships neither npm nor pip): before, the
// person got "exec: npm: executable file not found in $PATH".
func installToolsPresent(method string, packages []string, lookPath func(string) (string, error)) error {
	var need string
	switch method {
	case "npm":
		need = "npm"
	case "pip":
		need = "python3"
	default:
		return nil
	}
	if _, err := lookPath(need); err == nil {
		return nil
	}
	pkgs := strings.Join(packages, " ")
	if pkgs == "" {
		pkgs = need
	}
	return fmt.Errorf("%s is not installed; install what this profile needs first:\n  sudo dnf install %s", need, pkgs)
}
