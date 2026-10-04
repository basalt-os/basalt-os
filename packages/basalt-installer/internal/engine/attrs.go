package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// Linux inode flags (FS_IOC_GETFLAGS / FS_IOC_SETFLAGS, linux/fs.h).
const (
	fsIocGetFlags = 0x80086601
	fsIocSetFlags = 0x40086602
	fsAppendFl    = 0x00000020
	fsImmutableFl = 0x00000010
)

// skipDirs are never walked (virtual file systems mounted in the target).
var skipDirs = map[string]bool{"proc": true, "sys": true, "dev": true, "run": true}

func fileFlags(path string, set *uint32) (uint32, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var flags uint32
	req, arg := uintptr(fsIocGetFlags), uintptr(unsafe.Pointer(&flags))
	if set != nil {
		flags = *set
		req = fsIocSetFlags
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, arg); e != 0 {
		return 0, e
	}
	return flags, nil
}

// clearFileAttrs removes the append-only and immutable attributes from the
// regular files and directories below root and returns a function that
// puts them back.
func clearFileAttrs(root string) (func() error, int, error) {
	type saved struct {
		path  string
		flags uint32
	}
	var changed []saved
	restore := func() error {
		var errs []error
		for _, c := range changed {
			f := c.flags
			if _, err := fileFlags(c.path, &f); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && filepath.Dir(path) == filepath.Clean(root) && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil
		}
		flags, err := fileFlags(path, nil)
		if err != nil || flags&(fsAppendFl|fsImmutableFl) == 0 {
			return nil
		}
		cleared := flags &^ (fsAppendFl | fsImmutableFl)
		if _, err := fileFlags(path, &cleared); err != nil {
			return err
		}
		changed = append(changed, saved{path, flags})
		return nil
	})
	if err != nil {
		_ = restore()
		return nil, 0, err
	}
	return restore, len(changed), nil
}
