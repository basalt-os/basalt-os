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

	// NewParts are the partitions the installation creates, in order.
	NewParts []NewPart `json:"new_partitions"`
	// Kept are the partitions of the target disk that stay as they are
	// (target.wipe false).
	Kept []probe.Partition `json:"kept_partitions,omitempty"`
	// ESPShared is true when an existing EFI system partition is used.
	ESPShared  bool   `json:"esp_shared,omitempty"`
	ESPNumber  int    `json:"esp_number"`
	ESPFatUUID string `json:"esp_fat_uuid,omitempty"` // the shared ESP's volume ID
	// HomeDev is the new /home partition (encryption scope home).
	HomeDev       string `json:"home_dev,omitempty"`
	HomeBtrfsUUID string `json:"home_btrfs_uuid,omitempty"`
	// Existing is the adopted /home partition.
	Existing *ExistingHome `json:"existing_home,omitempty"`
}

// NewPart is one partition the installation creates.
type NewPart struct {
	Number int    `json:"number"`
	Role   string `json:"role"` // esp, boot, system, home
	Dev    string `json:"dev"`
	// Start and End are sectors (inclusive) when the partition goes into a
	// free region of a disk that keeps its other partitions; with a wiped
	// disk Start is 0 and Size is sgdisk's "+<n>M"/"+<n>G" or "0" (the rest).
	Start    int64  `json:"start,omitempty"`
	End      int64  `json:"end,omitempty"`
	Size     string `json:"size,omitempty"`
	Bytes    int64  `json:"size_bytes,omitempty"`
	TypeCode string `json:"typecode"`
	Name     string `json:"name"`
}

// ExistingHome is the adopted /home partition, as probed.
type ExistingHome struct {
	Device string `json:"device"`
	FSType string `json:"fstype"`
	UUID   string `json:"uuid"`
	LUKS   bool   `json:"luks"`
}

// Mapper is the device-mapper name of the opened existing /home.
func (e ExistingHome) Mapper() string { return "luks-" + e.UUID }

// LUKSDev is the partition the installation encrypts (the system's, or
// the new /home's).
func (r Resolved) LUKSDev() string {
	if r.Plan.EncryptsHome() {
		return r.HomeDev
	}
	return r.SystemDev
}

// CryptName is the device-mapper name of the open LUKS volume.
func (r Resolved) CryptName() string { return "luks-" + r.LUKSUUID }

// BtrfsDev is the block device holding the system's btrfs file system.
func (r Resolved) BtrfsDev() string {
	if r.Plan.EncryptsSystem() {
		return "/dev/mapper/" + r.CryptName()
	}
	return r.SystemDev
}

// ESPUUID is the FAT volume ID the way fstab and blkid write it.
func (r Resolved) ESPUUID() string {
	if r.ESPShared {
		return r.ESPFatUUID
	}
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
	if err := r.layoutPartitions(f); err != nil {
		return Resolved{}, err
	}
	if e := p.Home.Existing; e != nil {
		part, _ := f.Partition(e.Device)
		r.Existing = &ExistingHome{Device: part.Path, FSType: part.FSType, UUID: part.UUID, LUKS: part.FSType == "crypto_LUKS"}
	}

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
	if r.HomeDev != "" {
		if r.HomeBtrfsUUID, err = uuid4(opt.Random); err != nil {
			return Resolved{}, err
		}
	}
	mid := make([]byte, 16)
	if _, err = io.ReadFull(opt.Random, mid); err != nil {
		return Resolved{}, err
	}
	r.MachineID = hex.EncodeToString(mid)
	return r, nil
}

// layoutPartitions chooses the new partitions: numbers 1 to n on a wiped
// disk; on a disk that keeps its partitions, the lowest free numbers and
// explicit sectors inside its largest free region.
func (r *Resolved) layoutPartitions(f probe.Facts) error {
	p, disk := r.Plan, r.Disk
	sysType := "8300"
	if p.EncryptsSystem() {
		sysType = "8309"
	}
	type spec struct {
		role, typecode, name string
		bytes                int64 // 0: the rest
	}
	var specs []spec
	if p.Target.ESP == "" {
		specs = append(specs, spec{"esp", "EF00", "EFI System Partition", int64(p.Layout.ESPMiB) << 20})
	}
	specs = append(specs, spec{"boot", "8300", "boot", int64(p.Layout.BootMiB) << 20})
	sysBytes := int64(p.Layout.RootGiB) << 30
	space := disk.SizeBytes
	if !p.Target.Wipe {
		free, _ := disk.LargestFree()
		space = free.Bytes
	}
	if p.EncryptsHome() && sysBytes == 0 {
		sysBytes = int64(plan.DefaultSystemGiBWithHome) << 30
		if half := space / 2; half < sysBytes {
			sysBytes = half &^ (1<<20 - 1)
		}
	}
	specs = append(specs, spec{"system", sysType, "basalt", sysBytes})
	if p.EncryptsHome() {
		specs[len(specs)-1].bytes = sysBytes
		specs = append(specs, spec{"home", "8309", "basalt-home", 0})
	}
	// The last partition takes the rest when its size is 0.
	used := map[int]bool{}
	if !p.Target.Wipe {
		for _, part := range disk.Partitions {
			used[part.Number] = true
		}
		r.Kept = append(r.Kept, disk.Partitions...)
	}
	next := func() int {
		for n := 1; ; n++ {
			if !used[n] {
				used[n] = true
				return n
			}
		}
	}
	var region probe.Region
	sector := disk.SectorSize
	if sector <= 0 {
		sector = 512
	}
	if !p.Target.Wipe {
		region, _ = disk.LargestFree()
	}
	pos := region.Start
	for i, sp := range specs {
		np := NewPart{Number: next(), Role: sp.role, TypeCode: sp.typecode, Name: sp.name}
		np.Dev = PartitionPath(disk.Path, np.Number)
		last := i == len(specs)-1
		if p.Target.Wipe {
			switch {
			case sp.bytes == 0:
				np.Size = "0"
			case sp.bytes%(1<<30) == 0 && sp.role == "system":
				np.Size = fmt.Sprintf("+%dG", sp.bytes>>30)
			default:
				np.Size = fmt.Sprintf("+%dM", sp.bytes>>20)
			}
			np.Bytes = sp.bytes
		} else {
			n := sp.bytes / sector
			if sp.bytes == 0 || (last && sp.role != "home" && p.Layout.RootGiB == 0) {
				n = region.Start + region.Sectors - pos
			}
			if pos+n > region.Start+region.Sectors {
				return fmt.Errorf("the new partitions do not fit in the free space of %s", disk.Path)
			}
			np.Start, np.End, np.Bytes = pos, pos+n-1, n*sector
			pos += n
		}
		r.NewParts = append(r.NewParts, np)
		switch sp.role {
		case "esp":
			r.ESPDev, r.ESPNumber = np.Dev, np.Number
		case "boot":
			r.BootDev = np.Dev
		case "system":
			r.SystemDev = np.Dev
		case "home":
			r.HomeDev = np.Dev
		}
	}
	if p.Target.ESP != "" {
		for _, part := range disk.Partitions {
			if part.Path == p.Target.ESP {
				r.ESPShared, r.ESPDev, r.ESPNumber, r.ESPFatUUID = true, part.Path, part.Number, strings.ToUpper(part.UUID)
			}
		}
	}
	return nil
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
