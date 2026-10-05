package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// HomeOwner is one top-level directory of an existing /home.
type HomeOwner struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
}

// HomeInspection is what an existing /home partition holds.
type HomeInspection struct {
	Device string      `json:"device"`
	LUKS   bool        `json:"luks"`
	UUID   string      `json:"uuid"`
	FSType string      `json:"fstype"` // the file system (inside LUKS)
	Owners []HomeOwner `json:"owners"`
}

// HomeInspector opens an existing /home read-only and lists its
// directories (tests and the demo replace it).
type HomeInspector func(ctx context.Context, part probe.Partition, passphrase string) (HomeInspection, error)

// HomeCandidates lists the partitions that can be an existing /home: LUKS
// volumes and Linux file systems, not the installer media.
func (s *Session) HomeCandidates(ctx context.Context) ([]probe.Partition, error) {
	f, err := s.Facts(ctx, true)
	if err != nil {
		return nil, err
	}
	var out []probe.Partition
	for _, d := range f.Disks {
		for _, p := range d.Partitions {
			switch p.FSType {
			case "crypto_LUKS", "ext4", "xfs", "btrfs":
				if p.Label != probe.InstallerMediaLabel && p.Mountpoint == "" {
					out = append(out, p)
				}
			}
		}
	}
	return out, nil
}

// InspectHome checks the passphrase of an existing /home partition and
// lists who owns its directories, so the new user can get the same name,
// uid and gid. Nothing is written: the volume is opened and mounted
// read-only, then closed again.
func (s *Session) InspectHome(ctx context.Context, device, passphrase string) (HomeInspection, error) {
	f, err := s.Facts(ctx, false)
	if err != nil {
		return HomeInspection{}, err
	}
	part, ok := f.Partition(device)
	if !ok {
		return HomeInspection{}, fmt.Errorf(i18n.T("%s is not a partition on this machine"), device)
	}
	in := s.opt.HomeInspector
	if in == nil {
		in = InspectReadOnly
	}
	return in(ctx, part, passphrase)
}

// InspectReadOnly is the default HomeInspector.
func InspectReadOnly(ctx context.Context, part probe.Partition, passphrase string) (HomeInspection, error) {
	res := HomeInspection{Device: part.Path, UUID: part.UUID, LUKS: part.FSType == "crypto_LUKS", FSType: part.FSType}
	dev := part.Path
	if res.LUKS {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		name := "basalt-inspect-" + hex.EncodeToString(b)
		cmd := exec.CommandContext(ctx, "cryptsetup", "open", "--readonly", "--key-file=-", part.Path, name)
		cmd.Stdin = strings.NewReader(passphrase)
		if out, err := cmd.CombinedOutput(); err != nil {
			if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.ExitCode() == 2 {
				return res, errors.New(i18n.T("that is not the passphrase of this partition"))
			}
			return res, fmt.Errorf("cryptsetup open: %v: %s", err, strings.TrimSpace(string(out)))
		}
		defer func() { _ = exec.Command("cryptsetup", "close", name).Run() }()
		dev = "/dev/mapper/" + name
		if out, err := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "TYPE", dev).Output(); err == nil {
			res.FSType = strings.TrimSpace(string(out))
		}
	}
	if err := os.MkdirAll("/run/basalt-installer", 0o700); err != nil {
		return res, err
	}
	dir, err := os.MkdirTemp("/run/basalt-installer", "home-")
	if err != nil {
		return res, err
	}
	defer os.Remove(dir)
	opts := "ro"
	switch res.FSType {
	case "ext4", "ext3", "xfs":
		opts = "ro,norecovery" // no journal replay: nothing is written
	case "btrfs":
		opts = "ro,rescue=nologreplay"
	}
	if out, err := exec.CommandContext(ctx, "mount", "-o", opts, dev, dir).CombinedOutput(); err != nil {
		return res, fmt.Errorf("mount: %v: %s", err, strings.TrimSpace(string(out)))
	}
	defer func() { _ = exec.Command("umount", dir).Run() }()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return res, err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "lost+found" {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid >= 1000 {
			res.Owners = append(res.Owners, HomeOwner{Name: e.Name(), UID: int(st.Uid), GID: int(st.Gid)})
		}
	}
	sort.Slice(res.Owners, func(i, j int) bool { return res.Owners[i].Name < res.Owners[j].Name })
	return res, nil
}
