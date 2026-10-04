// Package sandbox runs a read-only check (a service's config checker such
// as `nginx -t`) so that it cannot change the machine it diagnoses.
//
// Config checkers are not read-only: `nginx -t` opens, and so creates, the
// log files its configuration names; `postfix check` creates missing
// directories. Run as root during a diagnosis, that changes the fault being
// diagnosed (a log directory moved from /root gets a new file with a new
// label). The checker therefore runs in a private mount namespace where
// every disk-backed or tmpfs file system is an overlay whose upper layer is
// a throwaway tmpfs: it sees the real files, may create and write files as
// usual, and everything it writes disappears with the namespace. A mount
// that cannot be overlaid is bound read-only (a write then fails instead of
// reaching the disk). Pseudo file systems (/proc, /sys, /dev) are bound as
// they are.
//
// The same tree can hold a different copy of one file (Replace): a snapshot
// copy of a config file is tested in place of the current one, without
// touching the current one.
//
// Linux only; needs CAP_SYS_ADMIN (the root command line). The confined
// daemon never runs checkers.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Command is the hidden subcommand of the `basalt` binary that runs a
// check in the sandbox.
const Command = "__sandbox"

// FailCode is the exit code when the sandbox itself could not be set up
// (the check did not run). Its output starts with Prefix.
const FailCode = 125

// Prefix starts every message of the sandbox itself.
const Prefix = "basalt-sandbox:"

// UpperSize bounds the throwaway layer. A check that writes more (a large
// existing log opened for writing is copied up whole) fails with "No space
// left on device" inside the sandbox; the caller reports the check as
// inconclusive instead of letting it fill memory.
const UpperSize = "256m"

const innerEnv = "BASALT_SANDBOX_INNER"

// oPath is O_PATH (not in package syscall): a handle on a directory that
// only names it, enough to reach a mount hidden by the new tree.
const oPath = 0x200000

// Wrap returns the argv that runs argv in the sandbox through the binary
// exe, with each replace[dst] = src copied over dst inside the sandbox.
func Wrap(exe string, argv []string, replace map[string]string) []string {
	out := []string{exe, Command}
	keys := make([]string, 0, len(replace))
	for k := range replace {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, dst := range keys {
		out = append(out, "--replace", dst+"="+replace[dst])
	}
	out = append(out, "--")
	return append(out, argv...)
}

// Failed reports output of a run whose sandbox could not be set up.
func Failed(code int, out string) bool {
	return code == FailCode && strings.HasPrefix(strings.TrimSpace(out), Prefix)
}

type options struct {
	replace map[string]string
	argv    []string
}

func parse(args []string) (options, error) {
	o := options{replace: map[string]string{}}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--":
			o.argv = args[i+1:]
			if len(o.argv) == 0 {
				return o, errors.New("no command")
			}
			return o, nil
		case "--replace":
			if i+1 >= len(args) {
				return o, errors.New("--replace needs DST=SRC")
			}
			i++
			dst, src, ok := strings.Cut(args[i], "=")
			if !ok || !filepath.IsAbs(dst) || !filepath.IsAbs(src) || filepath.Clean(dst) != dst || filepath.Clean(src) != src {
				return o, fmt.Errorf("--replace %q: want absolute, clean DST=SRC", args[i])
			}
			o.replace[dst] = src
		default:
			return o, fmt.Errorf("unknown argument %q", a)
		}
	}
	return o, errors.New("missing -- before the command")
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, Prefix+" "+format+"\n", a...)
	return FailCode
}

// Main is the entry of the hidden subcommand: args after Command.
func Main(args []string) int {
	o, err := parse(args)
	if err != nil {
		return fail("%v", err)
	}
	if os.Getenv(innerEnv) != "1" {
		return outer(args)
	}
	return inner(o)
}

// outer starts this binary again in a new mount namespace (Go makes every
// mount private there before the new program runs) and passes the exit code
// through.
func outer(args []string) int {
	if os.Geteuid() != 0 {
		return fail("needs root (a private mount namespace); run the diagnosis as root")
	}
	self, err := os.Executable()
	if err != nil {
		return fail("%v", err)
	}
	cmd := exec.Command(self, append([]string{Command}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), innerEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ee.ExitCode()
		}
		return fail("%v", err)
	}
	return 0
}

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	ID, Parent int
	Point      string
	FSType     string
	ReadOnly   bool
}

// ParseMountinfo reads /proc/self/mountinfo text.
func ParseMountinfo(text string) []Mount {
	var out []Mount
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(f) {
			continue
		}
		id, _ := strconv.Atoi(f[0])
		parent, _ := strconv.Atoi(f[1])
		ro := false
		for _, opt := range strings.Split(f[5], ",") {
			if opt == "ro" {
				ro = true
			}
		}
		out = append(out, Mount{ID: id, Parent: parent, Point: unescape(f[4]), FSType: f[sep+1], ReadOnly: ro})
	}
	return out
}

// unescape decodes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Order sorts mounts parents first (depth-first from the root mount, in
// mount order among siblings), so a mount is recreated after the one it
// sits on.
func Order(ms []Mount) []Mount {
	byID := map[int]bool{}
	for _, m := range ms {
		byID[m.ID] = true
	}
	children := map[int][]Mount{}
	var roots []Mount
	for _, m := range ms {
		if m.Parent == m.ID || !byID[m.Parent] {
			roots = append(roots, m)
			continue
		}
		children[m.Parent] = append(children[m.Parent], m)
	}
	var out []Mount
	var walk func(m Mount)
	walk = func(m Mount) {
		out = append(out, m)
		for _, c := range children[m.ID] {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// Pseudo file systems: bound into the tree as they are.
var pseudo = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "securityfs": true, "selinuxfs": true,
	"cgroup": true, "cgroup2": true, "pstore": true, "efivarfs": true, "bpf": true, "tracefs": true,
	"debugfs": true, "configfs": true, "fusectl": true, "mqueue": true, "hugetlbfs": true, "binfmt_misc": true,
	"rpc_pipefs": true, "nsfs": true, "ramfs": false,
}

// Kind is what the sandbox does with a mount.
func Kind(m Mount) string {
	switch {
	case m.FSType == "autofs":
		return "skip" // binding it would trigger the automount
	case pseudo[m.FSType]:
		return "bind"
	case m.ReadOnly:
		return "bind-ro"
	}
	return "overlay"
}

// inner builds the tree and runs the check in it. It runs in the new mount
// namespace (outer), where nothing it mounts is visible to the system.
func inner(o options) int {
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fail("%v", err)
	}
	mounts := Order(ParseMountinfo(string(info)))
	if len(mounts) == 0 || mounts[0].Point != "/" {
		return fail("cannot read the mount table")
	}
	// Read the replacement files before anything is mounted over them.
	repl := map[string][]byte{}
	modes := map[string]os.FileMode{}
	for dst, src := range o.replace {
		b, err := os.ReadFile(src)
		if err != nil {
			return fail("replacement %s: %v", src, err)
		}
		st, err := os.Stat(src)
		if err != nil {
			return fail("replacement %s: %v", src, err)
		}
		repl[dst], modes[dst] = b, st.Mode().Perm()
	}
	// Keep a handle on every original mount: once the tree is built over
	// /tmp they are reached through /proc/self/fd.
	fds := make([]int, len(mounts))
	for i, m := range mounts {
		fds[i] = -1
		if Kind(m) == "skip" {
			continue
		}
		fd, err := syscall.Open(m.Point, oPath|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if err != nil {
			if m.Point == "/" {
				return fail("open /: %v", err)
			}
			continue // gone or unreachable: it is left out of the tree
		}
		fds[i] = fd
	}
	// The scratch file system: the new root and every upper layer.
	scratch := "/tmp"
	if err := syscall.Mount("tmpfs", scratch, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0700,size="+UpperSize); err != nil {
		return fail("tmpfs: %v", err)
	}
	root := scratch + "/root"
	if err := os.Mkdir(root, 0o755); err != nil {
		return fail("%v", err)
	}
	placed := map[int]string{} // mount id -> how it is in the tree
	for i, m := range mounts {
		if fds[i] < 0 {
			continue
		}
		src := "/proc/self/fd/" + strconv.Itoa(fds[i])
		target := root + m.Point
		if m.Point == "/" {
			target = root
		}
		kind := Kind(m)
		err := place(kind, src, target, fmt.Sprintf("%s/layers/%d", scratch, i))
		if err != nil && kind == "overlay" {
			if bindReadOnly(src, target) == nil {
				kind, err = "bind-ro", nil
			}
		}
		switch {
		case err == nil:
			placed[m.ID] = kind
		case m.Point == "/":
			return fail("/ (%s): %v", m.FSType, err)
		case kind == "bind":
			// A pseudo file system that cannot be bound is left out.
		case placed[m.Parent] == "overlay":
			// Left out: its mount point is an empty directory of the parent
			// overlay, so whatever the check writes there is thrown away too.
		default:
			// A writable file system that could be neither overlaid nor
			// made read-only, on a parent that is not thrown away either:
			// the check must not run.
			return fail("%s (%s): %v", m.Point, m.FSType, err)
		}
	}
	for _, fd := range fds {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
	}
	for dst, b := range repl {
		p := root + dst
		if err := os.WriteFile(p, b, modes[dst]); err != nil {
			return fail("replace %s: %v", dst, err)
		}
	}
	if err := syscall.Chroot(root); err != nil {
		return fail("chroot: %v", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fail("chdir: %v", err)
	}
	bin, err := exec.LookPath(o.argv[0])
	if err != nil {
		for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			if st, e := os.Stat(dir + "/" + o.argv[0]); e == nil && !st.IsDir() {
				bin, err = dir+"/"+o.argv[0], nil
				break
			}
		}
	}
	if err != nil {
		return fail("%s: command not found", o.argv[0])
	}
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, innerEnv+"=") {
			env = append(env, kv)
		}
	}
	err = syscall.Exec(bin, o.argv, env)
	return fail("exec %s: %v", bin, err)
}

// place puts one original mount (reached through src) at target.
func place(kind, src, target, layer string) error {
	switch kind {
	case "bind":
		return syscall.Mount(src, target, "", syscall.MS_BIND, "")
	case "bind-ro":
		return bindReadOnly(src, target)
	}
	upper, work := layer+"/upper", layer+"/work"
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	opts := "lowerdir=" + src + ",upperdir=" + upper + ",workdir=" + work
	if err := syscall.Mount("overlay", target, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay: %w", err)
	}
	return nil
}

func bindReadOnly(src, target string) error {
	if err := syscall.Mount(src, target, "", syscall.MS_BIND, ""); err != nil {
		return err
	}
	return syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, "")
}
