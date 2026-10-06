// Package action is the closed set of changes the assistant can propose.
//
// A proposal never stores free-form commands that are executed later. It
// stores typed actions (kind + validated parameters); the exact commands are
// rebuilt from them by this package at preview time and at apply time, so a
// proposal file written by the confined daemon, by an MCP client or by hand
// can only ever express one of these changes, with parameters that pass the
// validators below.
package action

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// Kinds of action.
const (
	SELinuxFcontext   = "selinux.fcontext"   // semanage fcontext -a + restorecon
	SELinuxRestorecon = "selinux.restorecon" // restorecon
	SELinuxPort       = "selinux.port"       // semanage port -a|-m
	SELinuxBoolean    = "selinux.boolean"    // setsebool -P
	UnitRestart       = "unit.restart"       // systemctl restart
	FileRestore       = "file.restore"       // copy one file back from a snapshot
	SnapshotRollback  = "snapshot.rollback"  // basalt-rollback N
	SnapshotDelete    = "snapshot.delete"    // snapper delete N
	JournalVacuum     = "journal.vacuum"     // journalctl --vacuum-size
	DnfClean          = "dnf.clean"          // dnf clean packages
	DriverInstall     = "driver.install"     // basalt-nonfree on, NVIDIA driver installed, trial boot armed
)

// NvidiaLicenseSHA256 is the SHA-256 of the NVIDIA Driver License Agreement
// that basalt-nonfree's driver ships (packages/nvidia/source.conf) and that
// the assistant shows before an install (internal/drivers). A
// driver.install carries it as the "license" parameter: the person was
// shown exactly this text.
const NvidiaLicenseSHA256 = "c692fb14ad4f499c6aee1b9e144e6905148c420e6225b17cb2937366c1e507ad"

// Driver variants of driver.install: the display stack and compute, or
// compute only (servers).
var driverPackages = map[string]string{"display": "nvidia-driver", "compute": "nvidia-driver-compute"}

// Action is one typed change.
type Action struct {
	Kind   string            `json:"kind"`
	Params map[string]string `json:"params"`
}

// NeedsRootView reports kinds whose safety rests on a snapshot the confined
// view cannot check: it sees snapshot metadata, not the file contents nor
// the package databases. From the confined daemon or the MCP server they
// are hints until the root command line confirms the snapshot
// (`basalt confirm ID`).
func NeedsRootView(kind string) bool {
	return kind == FileRestore || kind == SnapshotRollback
}

// AnyNeedsRootView reports a list with such an action.
func AnyNeedsRootView(as []Action) bool {
	for _, a := range as {
		if NeedsRootView(a.Kind) {
			return true
		}
	}
	return false
}

// Hint is a change the confined view found plausible but may not propose:
// the actions (kept together, a restore and the restart that needs it) and
// why. `basalt confirm` checks it as root and turns it into a proposal.
type Hint struct {
	Actions []Action `json:"actions"`
	Reason  string   `json:"reason"`
}

// SnapshotDir is where snapper keeps the root snapshots.
var SnapshotDir = "/.snapshots"

var (
	reUnit    = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,200}\.(service|socket|timer|mount|path|target)$`)
	reType    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,120}_t$`)
	reBoolean = regexp.MustCompile(`^[a-z][a-z0-9_]{0,120}$`)
	reSize    = regexp.MustCompile(`^[0-9]{1,6}[KMG]$`)
	rePathOK  = regexp.MustCompile(`^/[A-Za-z0-9._@+,:/ -]{1,1000}$`)
	reKernel  = regexp.MustCompile(`^[0-9][0-9A-Za-z._+-]{0,100}\.x86_64$`)
)

func cleanPath(p string) (string, error) {
	if !rePathOK.MatchString(p) {
		return "", fmt.Errorf("path %q has characters outside the allowed set", p)
	}
	c := path.Clean(p)
	if c != p || strings.Contains(p, "/../") {
		return "", fmt.Errorf("path %q is not clean", p)
	}
	if c == "/" {
		return "", fmt.Errorf("refusing the root directory")
	}
	return c, nil
}

func snapNum(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > 1000000 {
		return 0, fmt.Errorf("snapshot %q is not a snapshot number", s)
	}
	return n, nil
}

// FcontextSpec turns a directory into the regular expression semanage
// expects for "this directory and everything below it".
func FcontextSpec(dir string) string {
	return regexp.QuoteMeta(dir) + "(/.*)?"
}

// Validate checks parameters; Commands and Verify call it first.
func (a Action) Validate() error {
	p := a.Params
	switch a.Kind {
	case SELinuxFcontext:
		if _, err := cleanPath(p["path"]); err != nil {
			return err
		}
		if !reType.MatchString(p["type"]) {
			return fmt.Errorf("type %q is not an SELinux type", p["type"])
		}
	case SELinuxRestorecon:
		if _, err := cleanPath(p["path"]); err != nil {
			return err
		}
	case SELinuxPort:
		n, err := strconv.Atoi(p["port"])
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port %q out of range", p["port"])
		}
		if p["proto"] != "tcp" && p["proto"] != "udp" && p["proto"] != "sctp" {
			return fmt.Errorf("protocol %q", p["proto"])
		}
		if !reType.MatchString(p["type"]) {
			return fmt.Errorf("type %q is not an SELinux type", p["type"])
		}
		if p["mode"] != "add" && p["mode"] != "modify" {
			return fmt.Errorf("mode %q", p["mode"])
		}
	case SELinuxBoolean:
		if !reBoolean.MatchString(p["name"]) {
			return fmt.Errorf("boolean %q", p["name"])
		}
		if p["value"] != "on" && p["value"] != "off" {
			return fmt.Errorf("boolean value %q", p["value"])
		}
	case UnitRestart:
		if !reUnit.MatchString(p["unit"]) {
			return fmt.Errorf("unit %q", p["unit"])
		}
	case FileRestore:
		c, err := cleanPath(p["path"])
		if err != nil {
			return err
		}
		if !strings.HasPrefix(c, "/etc/") {
			return fmt.Errorf("only files under /etc are restored from snapshots")
		}
		if _, err := snapNum(p["snapshot"]); err != nil {
			return err
		}
	case SnapshotRollback, SnapshotDelete:
		if _, err := snapNum(p["snapshot"]); err != nil {
			return err
		}
	case JournalVacuum:
		if !reSize.MatchString(p["size"]) {
			return fmt.Errorf("size %q (use e.g. 200M)", p["size"])
		}
	case DnfClean:
	case DriverInstall:
		if p["driver"] != "nvidia" {
			return fmt.Errorf("driver %q (only nvidia)", p["driver"])
		}
		if _, ok := driverPackages[p["variant"]]; !ok {
			return fmt.Errorf("variant %q (display or compute)", p["variant"])
		}
		if !reKernel.MatchString(p["kernel"]) {
			return fmt.Errorf("kernel %q is not a kernel release", p["kernel"])
		}
		if p["license"] != NvidiaLicenseSHA256 {
			return fmt.Errorf("license %q is not the NVIDIA Driver License Agreement the assistant shows", p["license"])
		}
	default:
		return fmt.Errorf("unknown action kind %q", a.Kind)
	}
	return nil
}

// Commands rebuilds the exact commands for an action.
func (a Action) Commands() ([]runner.Command, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	p := a.Params
	switch a.Kind {
	case SELinuxFcontext:
		return []runner.Command{
			{Argv: []string{"semanage", "fcontext", "-a", "-t", p["type"], FcontextSpec(p["path"])},
				Description: "label " + p["path"] + " and everything below it " + p["type"] + " (persistent rule)"},
			{Argv: []string{"restorecon", "-Rv", p["path"]}, Description: "apply the rule to the existing files"},
		}, nil
	case SELinuxRestorecon:
		argv := []string{"restorecon", "-v", p["path"]}
		if p["recursive"] == "yes" {
			argv = []string{"restorecon", "-Rv", p["path"]}
		}
		return []runner.Command{{Argv: argv, Description: "reset the label of " + p["path"] + " to the policy default"}}, nil
	case SELinuxPort:
		flag := "-a"
		if p["mode"] == "modify" {
			flag = "-m"
		}
		return []runner.Command{{Argv: []string{"semanage", "port", flag, "-t", p["type"], "-p", p["proto"], p["port"]},
			Description: "label " + p["proto"] + " port " + p["port"] + " " + p["type"]}}, nil
	case SELinuxBoolean:
		return []runner.Command{{Argv: []string{"setsebool", "-P", p["name"], p["value"]},
			Description: "set the boolean " + p["name"] + " " + p["value"] + " (persistent)"}}, nil
	case UnitRestart:
		return []runner.Command{{Argv: []string{"systemctl", "restart", p["unit"]}, Description: "restart " + p["unit"]}}, nil
	case FileRestore:
		src := SnapshotDir + "/" + p["snapshot"] + "/snapshot" + p["path"]
		return []runner.Command{
			{Argv: []string{"cp", "--preserve=mode,ownership,timestamps", src, p["path"]},
				Description: "restore " + p["path"] + " from snapshot " + p["snapshot"]},
			{Argv: []string{"restorecon", "-v", p["path"]}, Description: "label the restored file"},
		}, nil
	case SnapshotRollback:
		return []runner.Command{{Argv: []string{"basalt-rollback", "--yes", p["snapshot"]},
			Description: "make snapshot " + p["snapshot"] + " the root at the next boot (data subvolumes untouched)"}}, nil
	case SnapshotDelete:
		return []runner.Command{{Argv: []string{"snapper", "-c", "root", "delete", p["snapshot"]},
			Description: "delete snapshot " + p["snapshot"] + " and free the space only it holds"}}, nil
	case JournalVacuum:
		return []runner.Command{{Argv: []string{"journalctl", "--vacuum-size=" + p["size"]},
			Description: "shrink archived journal files to " + p["size"]}}, nil
	case DnfClean:
		return []runner.Command{{Argv: []string{"dnf", "clean", "packages"}, Description: "remove cached package files"}}, nil
	case DriverInstall:
		pkg := driverPackages[p["variant"]]
		return []runner.Command{
			{Argv: []string{"dnf", "-y", "install", "basalt-nonfree-release"},
				Description: "install the definition of the basalt-nonfree repository (it comes off)"},
			{Argv: []string{"dnf", "config-manager", "setopt", "basalt-nonfree.enabled=1"},
				Description: "turn the basalt-nonfree repository on"},
			{Argv: []string{"dnf", "-y", "install", "--skip-unavailable", pkg, "kmod-nvidia-open-" + p["kernel"]},
				Description: "install " + pkg + " (NVIDIA's userspace, unmodified, and the signed open kernel modules; nouveau goes off) and the module for the running kernel " + p["kernel"]},
			{Argv: []string{"basalt-nvidia", "arm"},
				Description: "make the next start a trial: it checks the driver and goes back to nouveau if it fails"},
		}, nil
	}
	return nil, fmt.Errorf("unknown action kind %q", a.Kind)
}

// Check is one verification step after an apply.
type Check struct {
	Description string   `json:"description"`
	Argv        []string `json:"argv,omitempty"`
	// Want: every string must appear in the output (empty: exit 0 is enough).
	Want []string `json:"want,omitempty"`
	// Absent: none of these may appear.
	Absent []string `json:"absent,omitempty"`
	// Func, when set, replaces Argv (checks that compare two outputs).
	Func func(ctx context.Context, r runner.Reader) (bool, string) `json:"-"`
}

// Run evaluates a check.
func (c Check) Run(ctx context.Context, r runner.Reader) (bool, string) {
	if c.Func != nil {
		return c.Func(ctx, r)
	}
	res := r.Read(ctx, c.Argv...)
	if res.Err != nil {
		return false, res.Err.Error()
	}
	if len(c.Want) == 0 && len(c.Absent) == 0 && res.Code != 0 {
		return false, fmt.Sprintf("exit %d: %s", res.Code, firstLine(res.Out))
	}
	for _, w := range c.Want {
		if !strings.Contains(res.Out, w) {
			return false, fmt.Sprintf("%q not in output: %s", w, firstLine(res.Out))
		}
	}
	for _, w := range c.Absent {
		if strings.Contains(res.Out, w) {
			return false, fmt.Sprintf("%q still in output", w)
		}
	}
	return true, firstLine(res.Out)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Verify returns the checks that prove an action worked. since is when the
// apply started (used to look for new denials).
func (a Action) Verify(since time.Time) []Check {
	if a.Validate() != nil {
		return nil
	}
	p := a.Params
	switch a.Kind {
	case SELinuxFcontext:
		return []Check{
			{Description: "the policy default for " + p["path"] + " is " + p["type"],
				Argv: []string{"matchpathcon", "-n", p["path"]}, Want: []string{":" + p["type"] + ":"}},
			{Description: p["path"] + " carries its default label",
				Argv: []string{"matchpathcon", "-V", p["path"]}, Want: []string{"verified"}},
		}
	case SELinuxRestorecon:
		return []Check{{Description: p["path"] + " carries its default label",
			Argv: []string{"matchpathcon", "-V", p["path"]}, Want: []string{"verified"}}}
	case SELinuxPort:
		return []Check{{Description: p["proto"] + " port " + p["port"] + " is " + p["type"],
			Func: func(ctx context.Context, r runner.Reader) (bool, string) {
				res := r.Read(ctx, "semanage", "port", "-l", "-C")
				for _, line := range strings.Split(res.Out, "\n") {
					f := strings.Fields(line)
					if len(f) >= 3 && f[0] == p["type"] && f[1] == p["proto"] && portListHas(strings.Join(f[2:], ""), p["port"]) {
						return true, strings.TrimSpace(line)
					}
				}
				return false, "no local port rule found"
			}}}
	case SELinuxBoolean:
		return []Check{{Description: p["name"] + " is " + p["value"],
			Argv: []string{"getsebool", p["name"]}, Want: []string{"--> " + p["value"]}}}
	case UnitRestart:
		return []Check{
			{Description: p["unit"] + " is active", Argv: []string{"systemctl", "is-active", p["unit"]}, Want: []string{"active"}, Absent: []string{"inactive", "failed", "activating"}},
			{Description: "no new SELinux denials since the change",
				Func: func(ctx context.Context, r runner.Reader) (bool, string) {
					res := r.Read(ctx, "journalctl", "--no-pager", "-o", "cat", "_TRANSPORT=audit",
						"--since", "@"+strconv.FormatInt(since.Unix(), 10))
					n := strings.Count(res.Out, "avc:  denied")
					return n == 0, fmt.Sprintf("%d denials", n)
				}},
		}
	case FileRestore:
		src := SnapshotDir + "/" + p["snapshot"] + "/snapshot" + p["path"]
		return []Check{{Description: p["path"] + " matches snapshot " + p["snapshot"], Argv: []string{"cmp", src, p["path"]}}}
	case SnapshotRollback:
		return []Check{{Description: "a rollback is pending: the next boot uses a copy of snapshot " + p["snapshot"],
			Func: func(ctx context.Context, r runner.Reader) (bool, string) {
				def := r.Read(ctx, "btrfs", "subvolume", "get-default", "/").Out
				root := strings.TrimSpace(r.Read(ctx, "btrfs", "inspect-internal", "rootid", "/").Out)
				f := strings.Fields(def)
				if len(f) >= 2 && f[1] != root {
					return true, "default subvolume " + strings.TrimSpace(def) + ", running root id " + root
				}
				return false, "default subvolume is still the running root"
			}}}
	case SnapshotDelete:
		return []Check{{Description: "deleted subvolumes cleaned up (space released)", Argv: []string{"btrfs", "subvolume", "sync", "/"}},
			{Description: "snapshot " + p["snapshot"] + " is gone",
				Func: func(ctx context.Context, r runner.Reader) (bool, string) {
					res := r.Read(ctx, "test", "-e", SnapshotDir+"/"+p["snapshot"])
					return res.Code != 0, ""
				}}}
	case JournalVacuum:
		return []Check{{Description: "journal disk usage", Argv: []string{"journalctl", "--disk-usage"}}}
	case DnfClean:
		return []Check{{Description: "dnf cache cleaned", Argv: []string{"true"}}}
	case DriverInstall:
		pkg := driverPackages[p["variant"]]
		return []Check{
			{Description: "the basalt-nonfree repository is on", Argv: []string{"dnf", "repo", "list", "--enabled"}, Want: []string{"basalt-nonfree"}},
			{Description: pkg + " is installed", Argv: []string{"rpm", "-q", pkg}},
			{Description: "nouveau is off from the next start", Argv: []string{"modprobe", "-c"}, Want: []string{"blacklist nouveau"}},
			{Description: "an installed kernel has the signed NVIDIA module", Argv: []string{"basalt-nvidia", "status", "--json"}, Absent: []string{`"kernels_with_module":""`}},
			{Description: "the next start is a trial of the NVIDIA driver", Argv: []string{"basalt-nvidia", "status", "--json"}, Want: []string{`"mode":"trial"`}},
		}
	}
	return nil
}

// portListHas reads "8080,8081-8090" style lists from semanage port -l.
func portListHas(list, port string) bool {
	n, _ := strconv.Atoi(port)
	for _, part := range strings.Split(list, ",") {
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			l, _ := strconv.Atoi(lo)
			h, _ := strconv.Atoi(hi)
			if n >= l && n <= h {
				return true
			}
		} else if part == port {
			return true
		}
	}
	return false
}

// Describe is a one-line summary of an action.
func (a Action) Describe() string {
	cmds, err := a.Commands()
	if err != nil {
		return a.Kind + " (invalid: " + err.Error() + ")"
	}
	parts := make([]string, len(cmds))
	for i, c := range cmds {
		parts[i] = c.Description
	}
	return strings.Join(parts, "; ")
}
