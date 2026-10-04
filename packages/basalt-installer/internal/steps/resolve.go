// Package steps turns a validated plan into the exact, ordered list of steps
// the engine runs: every command line (argv, no shell) and every file the
// installer writes is known before anything is changed, so the preview a
// person confirms is the installation itself.
package steps

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// Paths used by the installer on the live system.
const (
	DefaultRoot     = "/mnt/sysroot"
	DefaultWork     = "/run/basalt-installer"
	DefaultMediaDir = "/run/basalt/media"
)

// Options tune Resolve. The zero value uses the defaults above and
// crypto/rand.
type Options struct {
	Root     string
	Work     string
	MediaDir string
	// Random is the source of the identifiers (UUIDs, machine ID).
	Random io.Reader
	// InstallerVersion is recorded in the install log.
	InstallerVersion string
}

// Resolved is a plan bound to this machine: the disk, the profile chosen by
// "auto", and every identifier the installation will create (partition,
// file system and LUKS UUIDs, machine ID), so that the step list is exact.
type Resolved struct {
	Plan          plan.Plan  `json:"plan"`
	Disk          probe.Disk `json:"disk"`
	Release       int        `json:"release"`
	Arch          string     `json:"arch"`
	Virt          string     `json:"virt"`
	SecureBoot    string     `json:"secure_boot"`
	TPM2          bool       `json:"tpm2"`
	Profile       string     `json:"profile"`
	ProfileReason string     `json:"profile_reason"`
	// EnrollTPM is true when the unlock method uses the TPM and one exists.
	EnrollTPM bool `json:"enroll_tpm"`

	ESPDev    string `json:"esp_dev"`
	BootDev   string `json:"boot_dev"`
	SystemDev string `json:"system_dev"`
	// ESPVolID is the FAT volume ID, 8 hex digits (fstab shows XXXX-XXXX).
	ESPVolID  string `json:"esp_vol_id"`
	BootUUID  string `json:"boot_uuid"`
	LUKSUUID  string `json:"luks_uuid,omitempty"`
	BtrfsUUID string `json:"btrfs_uuid"`
	MachineID string `json:"machine_id"`

	Root      string `json:"root"`
	Work      string `json:"work"`
	MediaDir  string `json:"media_dir"`
	Installer string `json:"installer_version"`
}

// CryptName is the device-mapper name of the open LUKS volume.
func (r Resolved) CryptName() string { return "luks-" + r.LUKSUUID }

// BtrfsDev is the block device holding the btrfs file system.
func (r Resolved) BtrfsDev() string {
	if r.Plan.Encrypted() {
		return "/dev/mapper/" + r.CryptName()
	}
	return r.SystemDev
}

// ESPUUID is the FAT volume ID the way fstab and blkid write it.
func (r Resolved) ESPUUID() string {
	return strings.ToUpper(r.ESPVolID[:4] + "-" + r.ESPVolID[4:])
}

// Target joins a path inside the installed system with the mount root.
func (r Resolved) Target(path string) string { return filepath.Join(r.Root, path) }

// KeyFile is the temporary LUKS key (tmpfs, removed after enrollment).
func (r Resolved) KeyFile() string { return filepath.Join(r.Work, "luks.key") }

// ReposDir holds the temporary repository files for the package step.
func (r Resolved) ReposDir() string { return filepath.Join(r.Work, "repos.d") }

// TopMount is where the btrfs top level is mounted to create subvolumes.
func (r Resolved) TopMount() string { return filepath.Join(r.Work, "btrfs-top") }

// UsesTang reports whether the unlock method includes a Tang server.
func (r Resolved) UsesTang() bool {
	return r.Plan.Encrypted() && strings.Contains(r.Plan.Encryption.Unlock, "tang")
}

// Resolve binds a valid plan to the machine. It fails when the plan has
// blocking issues.
func Resolve(p plan.Plan, f probe.Facts, opt Options) (Resolved, error) {
	if err := plan.Validate(p, &f, "").Err(); err != nil {
		return Resolved{}, err
	}
	disk, _ := f.Disk(p.Target.Disk)
	if opt.Root == "" {
		opt.Root = DefaultRoot
	}
	if opt.Work == "" {
		opt.Work = DefaultWork
	}
	if opt.MediaDir == "" {
		opt.MediaDir = DefaultMediaDir
	}
	if opt.Random == nil {
		opt.Random = rand.Reader
	}
	r := Resolved{
		Plan: p, Disk: disk, Arch: f.Arch, Virt: f.Virt, SecureBoot: f.SecureBoot, TPM2: f.TPM2,
		Root: opt.Root, Work: opt.Work, MediaDir: opt.MediaDir, Installer: opt.InstallerVersion,
	}
	r.Release = p.Release
	if r.Release == 0 {
		r.Release = f.Release
	}
	if r.Release == 0 {
		return Resolved{}, fmt.Errorf("the Fedora release is unknown: set release in the plan")
	}
	r.Profile, r.ProfileReason = p.Profile, "chosen in the plan"
	if p.Profile == "auto" {
		if f.Virt != "" {
			r.Profile, r.ProfileReason = "minimal", "auto, virtual machine: "+f.Virt
		} else {
			r.Profile, r.ProfileReason = "standard", "auto, bare metal"
		}
	}
	r.EnrollTPM = p.Encrypted() && strings.Contains(p.Encryption.Unlock, "tpm2") && f.TPM2
	r.ESPDev, r.BootDev, r.SystemDev = PartitionPath(disk.Path, 1), PartitionPath(disk.Path, 2), PartitionPath(disk.Path, 3)

	var err error
	vol := make([]byte, 4)
	if _, err = io.ReadFull(opt.Random, vol); err != nil {
		return Resolved{}, err
	}
	r.ESPVolID = strings.ToUpper(hex.EncodeToString(vol))
	if r.BootUUID, err = uuid4(opt.Random); err != nil {
		return Resolved{}, err
	}
	if p.Encrypted() {
		if r.LUKSUUID, err = uuid4(opt.Random); err != nil {
			return Resolved{}, err
		}
	}
	if r.BtrfsUUID, err = uuid4(opt.Random); err != nil {
		return Resolved{}, err
	}
	mid := make([]byte, 16)
	if _, err = io.ReadFull(opt.Random, mid); err != nil {
		return Resolved{}, err
	}
	r.MachineID = hex.EncodeToString(mid)
	return r, nil
}

// PartitionPath names partition n of a disk: /dev/vda -> /dev/vda1,
// /dev/nvme0n1 -> /dev/nvme0n1p1.
func PartitionPath(disk string, n int) string {
	if disk != "" && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

func uuid4(r io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
