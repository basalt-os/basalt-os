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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
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
	UpdateCheck       = "update.check"       // dnf makecache --refresh (look: run without a proposal)
	UpdateInstall     = "update.install"     // download, then install exactly the previewed updates
	UpdateRollback    = "update.rollback"    // basalt-rollback to the snapshot taken before an update
	RepoEnable        = "repo.enable"        // dnf config-manager setopt REPO.enabled=1
	RepoDisable       = "repo.disable"       // dnf config-manager setopt REPO.enabled=0
	SourceAdd         = "source.add"         // a new software source: its key becomes a trust root
	SourceRemove      = "source.remove"      // remove a source Basalt added
)

// SourcePaths are the files the software source actions check (tests
// point them at a temporary directory).
var SourcePaths = sources.System

// MaxUpdatePackages bounds one update.install.
const MaxUpdatePackages = 1500

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
	return kind == FileRestore || kind == SnapshotRollback || kind == UpdateRollback
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
	// reNEVRA: name-[epoch:]version-release.arch, as dnf repoquery prints it.
	reNEVRA  = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,200}-([0-9]{1,10}:)?[A-Za-z0-9._+~^]{1,100}-[A-Za-z0-9._+~^]{1,100}\.(x86_64|noarch|i686|aarch64)$`)
	reProp   = regexp.MustCompile(`^p-[0-9a-f]{6}$`)
	reDigest = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// UpdateDigest binds an update.install's package list: the first 16 hex
// digits of the SHA-256 of the list as stored.
func UpdateDigest(packages string) string {
	sum := sha256.Sum256([]byte(packages))
	return hex.EncodeToString(sum[:])[:16]
}

// UpdatePackages splits an update.install's package list.
func UpdatePackages(packages string) []string { return strings.Fields(packages) }

// withoutEpoch is a NEVRA as rpm -q takes it.
func withoutEpoch(nevra string) string {
	if i := strings.LastIndex(nevra, ":"); i >= 0 {
		j := strings.LastIndex(nevra[:i], "-")
		if j >= 0 {
			return nevra[:j+1] + nevra[i+1:]
		}
	}
	return nevra
}

// repoParams checks a repo.enable or repo.disable: one of Basalt's
// toggleable channels, or a source Basalt added. basalt itself, Fedora's
// repositories and anything else are refused.
func repoParams(kind string, p map[string]string) error {
	repo := p["repo"]
	switch {
	case sources.Toggleable(repo):
	case sources.ReID.MatchString(repo) && !strings.HasPrefix(repo, "basalt") && sources.HasRecord(SourcePaths, repo):
	default:
		return fmt.Errorf("repository %q is not a channel or source that can be turned on or off here", repo)
	}
	if kind == RepoEnable && sources.Testing(repo) && p["consent"] != sources.TestingConsentToken {
		return fmt.Errorf("turning on %s needs the person's consent to preview builds (consent %q)", repo, sources.TestingConsentToken)
	}
	if kind == RepoDisable && p["consent"] != "" {
		return fmt.Errorf("repo.disable takes no consent")
	}
	switch p["definition"] {
	case "":
	case "install":
		if kind != RepoEnable || !strings.HasPrefix(repo, sources.ChanNonfree) {
			return fmt.Errorf("only a basalt-nonfree channel installs its definition")
		}
	default:
		return fmt.Errorf("definition %q", p["definition"])
	}
	return nil
}

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
	case UpdateCheck:
	case UpdateInstall:
		if p["scope"] != "all" && p["scope"] != "security" {
			return fmt.Errorf("scope %q (all or security)", p["scope"])
		}
		pk := UpdatePackages(p["packages"])
		if len(pk) == 0 || len(pk) > MaxUpdatePackages {
			return fmt.Errorf("an update needs 1 to %d packages, not %d", MaxUpdatePackages, len(pk))
		}
		if strings.Join(pk, " ") != p["packages"] {
			return fmt.Errorf("the package list is not in its canonical form")
		}
		seen := map[string]bool{}
		for _, n := range pk {
			if !reNEVRA.MatchString(n) {
				return fmt.Errorf("package %q is not name-version-release.arch", n)
			}
			if seen[n] {
				return fmt.Errorf("package %q twice", n)
			}
			seen[n] = true
		}
		if p["count"] != strconv.Itoa(len(pk)) {
			return fmt.Errorf("count %q does not match the %d packages", p["count"], len(pk))
		}
		if !reDigest.MatchString(p["digest"]) || p["digest"] != UpdateDigest(p["packages"]) {
			return fmt.Errorf("digest %q does not match the package list", p["digest"])
		}
	case UpdateRollback:
		if _, err := snapNum(p["snapshot"]); err != nil {
			return err
		}
		if !reProp.MatchString(p["proposal"]) {
			return fmt.Errorf("proposal %q is not an update proposal id", p["proposal"])
		}
	case RepoEnable, RepoDisable:
		return repoParams(a.Kind, p)
	case SourceAdd:
		return sources.FromMap(p).Validate()
	case SourceRemove:
		if !sources.ReID.MatchString(p["id"]) || !sources.HasRecord(SourcePaths, p["id"]) {
			return fmt.Errorf("%q is not a source Basalt added", p["id"])
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
	case UpdateCheck:
		return []runner.Command{{Argv: []string{"dnf", "makecache", "--refresh"},
			Description: "download the newest list of packages from every enabled repository (nothing is installed)"}}, nil
	case UpdateInstall:
		pk := UpdatePackages(p["packages"])
		what := p["count"] + " updates"
		if p["scope"] == "security" {
			what = p["count"] + " security updates"
		}
		return []runner.Command{
			{Argv: append([]string{"dnf", "-y", "upgrade", "--downloadonly"}, pk...),
				Description: "download " + what + " (checked against the repositories' signing keys)"},
			{Argv: append([]string{"dnf", "-y", "upgrade"}, pk...),
				Description: "install " + what + ", exactly the versions shown"},
		}, nil
	case UpdateRollback:
		return []runner.Command{{Argv: []string{"basalt-rollback", "--yes", p["snapshot"]},
			Description: "undo the update " + p["proposal"] + ": make snapshot " + p["snapshot"] + ", taken just before it, the root at the next boot (your files untouched)"}}, nil
	case RepoEnable, RepoDisable:
		var out []runner.Command
		if p["definition"] == "install" {
			out = append(out,
				runner.Command{Argv: []string{"dnf", "-y", "install", sources.NonfreeReleasePkg},
					Description: "install the definition of the basalt-nonfree channels (they come off)"},
				runner.Command{Argv: []string{"dnf", "-y", "upgrade", sources.NonfreeReleasePkg},
					Description: "bring that definition up to date (it gains the testing channel)"})
		}
		val, verb := "1", "turn the "+p["repo"]+" channel on"
		if a.Kind == RepoDisable {
			val, verb = "0", "turn the "+p["repo"]+" channel off"
		}
		return append(out, runner.Command{Argv: []string{"dnf", "config-manager", "setopt", p["repo"] + ".enabled=" + val},
			Description: verb}), nil
	case SourceAdd:
		sp := sources.FromMap(p)
		return []runner.Command{{Argv: sp.Argv(),
			Description: "add the source " + sp.Name + " (" + sp.URL + "), trusting its signing key " + sources.Spaced(sp.Fingerprint) + " after checking it again"}}, nil
	case SourceRemove:
		return []runner.Command{{Argv: []string{"basalt", "__source", "remove", "--id", p["id"]},
			Description: "remove the source " + p["id"] + " (and its key, when no other repository of it is left)"}}, nil
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
	case UpdateCheck:
		return nil
	case UpdateInstall:
		pk := UpdatePackages(p["packages"])
		return []Check{{Description: "the " + p["count"] + " updates are installed",
			Func: func(ctx context.Context, r runner.Reader) (bool, string) {
				q := make([]string, len(pk))
				for i, n := range pk {
					q[i] = withoutEpoch(n)
				}
				res := r.Read(ctx, append([]string{"rpm", "-q"}, q...)...)
				missing := strings.Count(res.Out, "is not installed")
				if missing > 0 {
					return false, fmt.Sprintf("%d of %d not installed", missing, len(pk))
				}
				return res.Code == 0, fmt.Sprintf("%d installed", len(pk))
			}}}
	case UpdateRollback:
		return Action{Kind: SnapshotRollback, Params: map[string]string{"snapshot": p["snapshot"]}}.Verify(since)
	case RepoEnable, RepoDisable:
		want := a.Kind == RepoEnable
		return []Check{{Description: fmt.Sprintf("the %s channel is %s", p["repo"], map[bool]string{true: "on", false: "off"}[want]),
			Func: func(ctx context.Context, r runner.Reader) (bool, string) {
				res := r.Read(ctx, "dnf", "repo", "list", "--enabled")
				on := false
				for _, line := range strings.Split(res.Out, "\n") {
					if f := strings.Fields(line); len(f) > 0 && f[0] == p["repo"] {
						on = true
					}
				}
				return on == want, map[bool]string{true: "enabled", false: "disabled"}[on]
			}}}
	case SourceAdd:
		sp := sources.FromMap(p)
		return []Check{{Description: "the source " + sp.ID + " is recorded with its key",
			Func: func(ctx context.Context, r runner.Reader) (bool, string) {
				return sources.HasRecord(SourcePaths, sp.ID), SourcePaths.RecordPath(sp.ID)
			}}}
	case SourceRemove:
		id := p["id"]
		return []Check{{Description: "the source " + id + " is gone",
			Func: func(ctx context.Context, r runner.Reader) (bool, string) {
				_, err := os.Stat(SourcePaths.RepoPath(id))
				return !sources.HasRecord(SourcePaths, id) && os.IsNotExist(err), ""
			}}}
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
