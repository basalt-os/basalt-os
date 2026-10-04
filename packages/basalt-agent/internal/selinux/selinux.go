// Package selinux has the few SELinux operations basalt-agent needs,
// without libselinux: the current context, file contexts read and set
// through the security.selinux extended attribute, and a recursive relabel
// that stays on one file system.
package selinux

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const xattr = "security.selinux"

// Enabled reports whether SELinux is active.
func Enabled() bool {
	_, err := os.Stat("/sys/fs/selinux/enforce")
	return err == nil
}

// Enforcing reports whether SELinux is enforcing.
func Enforcing() bool {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// Current returns the context of this process.
func Current() (string, error) {
	b, err := os.ReadFile("/proc/self/attr/current")
	if err != nil {
		return "", err
	}
	return string(bytes.TrimRight(b, "\x00\n")), nil
}

// Context is a parsed user:role:type:level context.
type Context struct{ User, Role, Type, Level string }

// Parse splits a context; the level keeps its own colons.
func Parse(s string) (Context, error) {
	p := strings.SplitN(s, ":", 4)
	if len(p) < 3 {
		return Context{}, fmt.Errorf("bad context %q", s)
	}
	c := Context{User: p[0], Role: p[1], Type: p[2]}
	if len(p) == 4 {
		c.Level = p[3]
	}
	return c, nil
}

func (c Context) String() string {
	s := c.User + ":" + c.Role + ":" + c.Type
	if c.Level != "" {
		s += ":" + c.Level
	}
	return s
}

func lgetxattr(path string) (string, error) {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return "", err
	}
	n, err := syscall.BytePtrFromString(xattr)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	for {
		r, _, e := syscall.Syscall6(syscall.SYS_LGETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
		if e == syscall.ERANGE {
			buf = make([]byte, len(buf)*4)
			continue
		}
		if e != 0 {
			return "", e
		}
		return string(bytes.TrimRight(buf[:r], "\x00")), nil
	}
}

func lsetxattr(path, value string) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	n, err := syscall.BytePtrFromString(xattr)
	if err != nil {
		return err
	}
	v := append([]byte(value), 0)
	_, _, e := syscall.Syscall6(syscall.SYS_LSETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// FileContext returns the context of path (not following a final symlink).
func FileContext(path string) (string, error) { return lgetxattr(path) }

// SetFileContext sets the context of path (not following a final symlink).
func SetFileContext(path, ctx string) error {
	if err := lsetxattr(path, ctx); err != nil {
		return fmt.Errorf("relabel %s to %s: %w", path, ctx, err)
	}
	return nil
}

// Relabel gives every file under root (root included, one file system
// only) the type typ and level lvl, keeping each file's SELinux user. It
// returns the number of files changed. Files whose context already
// matches are not touched. skip, when set, prunes paths (relative to root).
func Relabel(root, typ, lvl string, skip func(rel string) bool) (int, error) {
	var rootDev uint64
	st, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		rootDev = s.Dev
	}
	n := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if skip != nil && rel != "." && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && p != root {
			if info, err := d.Info(); err == nil {
				if s, ok := info.Sys().(*syscall.Stat_t); ok && s.Dev != rootDev {
					return filepath.SkipDir
				}
			}
		}
		cur, err := lgetxattr(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		c, err := Parse(cur)
		if err != nil {
			return err
		}
		want := Context{User: c.User, Role: "object_r", Type: typ, Level: lvl}
		if want.String() == cur {
			return nil
		}
		if err := SetFileContext(p, want.String()); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}
