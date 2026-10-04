// Package diag holds the diagnosers shared by the CLI, the event engine and
// the MCP server: unit failures, SELinux denials, disk usage, snapshots and
// failed package transactions. Diagnosers only read; what they find becomes
// evidence, features for the decision layer, and typed actions.
package diag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sandbox"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// FSStat is the result of statfs on a mount point.
type FSStat struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"` // available to unprivileged users
	Used  uint64 `json:"used"`
}

// UsedPct is the used share in percent.
func (s FSStat) UsedPct() float64 {
	if s.Total == 0 {
		return 0
	}
	return 100 * float64(s.Total-s.Free) / float64(s.Total)
}

// Env is what diagnosers may use. Tests replace every field.
type Env struct {
	R runner.Reader
	// Confined is true inside the daemon and the MCP server: probes that
	// need more than read access (config checkers that open log files,
	// btrfs ioctls, process ownership of sockets, reading service config
	// contents) are skipped and the report says so.
	Confined bool
	Now      func() time.Time
	Statfs   func(path string) (FSStat, error)
	// Label returns the SELinux label of a file (security.selinux xattr).
	Label func(path string) string
	// Inode returns a file's inode number.
	Inode func(path string) (uint64, bool)
	// ReadFile reads a small file (snapshot metadata, configs).
	ReadFile func(path string) ([]byte, error)
	// Glob lists paths.
	Glob   func(pattern string) []string
	Decide *decide.Layer
	// SnapshotDir is /.snapshots.
	SnapshotDir string
	// HistoryPath stores disk usage samples for the forecast.
	HistoryPath string
	// Policy answers SELinux policy queries (retries, short cache); one is
	// made from R when it is nil.
	Policy *selinux.Policy
	// Isolate wraps a config checker's argv so it runs without side effects
	// (package sandbox), with replace[dst] = src copied over dst for the
	// run. Nil only in tests: the argv then runs as it is.
	Isolate func(argv []string, replace map[string]string) []string
}

// Real returns an Env bound to this machine.
func Real(confined bool, layer *decide.Layer) *Env {
	// Only the `basalt` binary has the sandbox subcommand (the daemon and
	// the MCP server never run checkers).
	exe, err := os.Executable()
	if err != nil || filepath.Base(exe) != "basalt" {
		exe = "/usr/bin/basalt"
	}
	r := runner.Exec{}
	return &Env{
		Policy: selinux.NewPolicy(r),
		Isolate: func(argv []string, replace map[string]string) []string {
			return sandbox.Wrap(exe, argv, replace)
		},
		R:           r,
		Confined:    confined,
		Now:         time.Now,
		Statfs:      statfs,
		Label:       label,
		Inode:       inode,
		ReadFile:    os.ReadFile,
		Glob:        glob,
		Decide:      layer,
		SnapshotDir: "/.snapshots",
		HistoryPath: "/var/lib/basalt-assistant/disk-history.jsonl",
	}
}

func statfs(path string) (FSStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSStat{}, err
	}
	bs := uint64(st.Bsize)
	s := FSStat{Path: path, Total: st.Blocks * bs, Free: st.Bavail * bs}
	s.Used = s.Total - st.Bfree*bs
	return s, nil
}

func label(path string) string {
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(path, "security.selinux", buf)
	if err != nil || n <= 0 {
		return ""
	}
	return strings.TrimRight(string(buf[:n]), "\x00")
}

func inode(path string) (uint64, bool) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Ino, true
	}
	return 0, false
}

func glob(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	return m
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// policy is the shared policy reader.
func (e *Env) policy() *selinux.Policy {
	if e.Policy == nil {
		e.Policy = selinux.NewPolicy(e.R)
	}
	return e.Policy
}

// analyzer is an SELinux analyzer bound to this machine.
func (e *Env) analyzer() selinux.Analyzer {
	return selinux.Analyzer{R: e.R, Resolve: e.resolveAVCPath, Policy: e.policy()}
}

func (e *Env) ask(ctx context.Context, q decide.Question) decide.Decision {
	if e.Decide == nil {
		e.Decide = decide.NewRules(nil, nil)
	}
	return e.Decide.Ask(ctx, q)
}
