// Package probe reads the facts an installation depends on: firmware, Secure
// Boot, TPM, virtualization, memory and the disks. Probing only reads; it
// never changes the machine.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Facts describe the machine the installer runs on.
type Facts struct {
	UEFI bool `json:"uefi"`
	// SecureBoot is "enabled", "disabled" or "unknown".
	SecureBoot string `json:"secure_boot"`
	TPM2       bool   `json:"tpm2"`
	// Virt names the hypervisor, "" on bare metal.
	Virt       string   `json:"virt"`
	Arch       string   `json:"arch"`
	MemoryMiB  int      `json:"memory_mib"`
	Release    int      `json:"release"`
	Disks      []Disk   `json:"disks"`
	Interfaces []string `json:"interfaces"`
	// KeyMedia are the file systems on removable media (USB sticks, SD
	// cards) that can take a copy of the recovery key.
	KeyMedia []KeyMedium `json:"key_media"`
}

// KeyMedium is a writable file system on a removable disk.
type KeyMedium struct {
	Path   string `json:"path"`
	Disk   string `json:"disk"`
	Label  string `json:"label,omitempty"`
	FSType string `json:"fstype"`
	Size   int64  `json:"size_bytes"`
	Model  string `json:"model,omitempty"`
	// Mountpoint is set when the file system is mounted already.
	Mountpoint string `json:"mountpoint,omitempty"`
}

// Describe is a one-line description for pickers.
func (m KeyMedium) Describe() string {
	parts := []string{m.Path, HumanSize(m.Size), m.FSType}
	if m.Label != "" {
		parts = append(parts, "\""+m.Label+"\"")
	}
	if m.Model != "" {
		parts = append(parts, m.Model)
	}
	return strings.Join(parts, "  ")
}

// keyMediaTypes are the file systems the live system can write a key file
// to.
var keyMediaTypes = map[string]bool{"vfat": true, "exfat": true, "ext4": true, "ext3": true, "ext2": true, "btrfs": true, "xfs": true}

// InstallerMediaLabel is the volume label of the Basalt OS installer media.
const InstallerMediaLabel = "BASALT-INST"

// ParseKeyMedia finds the file systems on removable disks (removable flag
// or USB, MMC transport) that can hold the recovery key: a known writable
// file system, not read-only, not the installer media.
func ParseKeyMedia(data []byte) ([]KeyMedium, error) {
	var doc struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing lsblk: %w", err)
	}
	var out []KeyMedium
	for _, d := range doc.Blockdevices {
		tran := strings.ToLower(str(d.Tran))
		if d.Type != "disk" || bool(d.RO) || !(bool(d.RM) || tran == "usb" || tran == "mmc") {
			continue
		}
		cands := []lsblkDev{d}
		cands = append(cands, d.Children...)
		for _, c := range cands {
			fs := str(c.FSType)
			if !keyMediaTypes[fs] || bool(c.RO) || str(c.Label) == InstallerMediaLabel {
				continue
			}
			m := KeyMedium{Path: c.Path, Disk: d.Path, Label: str(c.Label), FSType: fs, Size: int64(c.Size), Model: str(d.Model)}
			if m.Path == "" {
				m.Path = "/dev/" + c.Name
			}
			for _, mp := range c.Mountpoints {
				if mp != nil && *mp != "" {
					m.Mountpoint = *mp
					break
				}
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// Disk is one whole disk.
type Disk struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	Model      string `json:"model,omitempty"`
	Serial     string `json:"serial,omitempty"`
	Tran       string `json:"tran,omitempty"`
	Removable  bool   `json:"removable"`
	ReadOnly   bool   `json:"read_only"`
	Rotational bool   `json:"rotational"`
	// Contents summarizes what is on the disk now ("empty", "3 partitions:
	// vfat, ext4, crypto_LUKS").
	Contents string `json:"contents"`
	// InUse says why the disk cannot be wiped now (a mounted file system,
	// an open encrypted volume, active swap), "" when it is free.
	InUse string `json:"in_use,omitempty"`
	// Complex says why this version of the installer does not handle the
	// disk (RAID member, multipath, iSCSI), "" otherwise.
	Complex string `json:"complex,omitempty"`
	// Table is the partition table type (gpt, dos), "" when there is none.
	Table string `json:"table,omitempty"`
	// SectorSize is the logical sector size in bytes.
	SectorSize int64 `json:"sector_size,omitempty"`
	// Partitions are the existing partitions, in disk order.
	Partitions []Partition `json:"partitions,omitempty"`
	// Free lists the unused regions of a GPT disk large enough to matter
	// (at least 1 MiB, aligned to 1 MiB), in disk order.
	Free []Region `json:"free,omitempty"`
}

// Partition is one existing partition.
type Partition struct {
	Path    string `json:"path"`
	Number  int    `json:"number"`
	Start   int64  `json:"start"`   // first sector
	Sectors int64  `json:"sectors"` // length in sectors
	Bytes   int64  `json:"size_bytes"`
	// Type is the GPT partition type GUID (lower case) or the MBR type.
	Type       string `json:"type,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	Label      string `json:"label,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	// Disk is the whole disk the partition is on.
	Disk string `json:"disk"`
}

// Region is a span of unused sectors.
type Region struct {
	Start   int64 `json:"start"`
	Sectors int64 `json:"sectors"`
	Bytes   int64 `json:"size_bytes"`
}

// GPT partition types the installer knows.
const (
	TypeESP       = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"
	TypeMSR       = "e3c9e316-0b5c-4db8-817d-f92df00215ae"
	TypeWindows   = "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7"
	TypeWinRecov  = "de94bba4-06d1-4d40-a16a-bfd50179d6ac"
	alignSectors  = 2048 // 1 MiB with 512 byte sectors
	gptTailBlocks = 34   // backup GPT
)

// Describe is a one-line description of a partition.
func (p Partition) Describe() string {
	parts := []string{p.Path, HumanSize(p.Bytes)}
	if p.FSType != "" {
		parts = append(parts, p.FSType)
	}
	if p.Label != "" {
		parts = append(parts, "\""+p.Label+"\"")
	}
	if n := TypeName(p.Type); n != "" {
		parts = append(parts, n)
	}
	return strings.Join(parts, "  ")
}

// TypeName names the well-known partition types.
func TypeName(t string) string {
	switch strings.ToLower(t) {
	case TypeESP:
		return "EFI system"
	case TypeMSR:
		return "Microsoft reserved"
	case TypeWindows:
		return "Windows data"
	case TypeWinRecov:
		return "Windows recovery"
	}
	return ""
}

// LargestFree returns the largest free region of the disk.
func (d Disk) LargestFree() (Region, bool) {
	var best Region
	for _, r := range d.Free {
		if r.Sectors > best.Sectors {
			best = r
		}
	}
	return best, best.Sectors > 0
}

// Partition finds an existing partition by path on any disk.
func (f Facts) Partition(path string) (Partition, bool) {
	for _, d := range f.Disks {
		for _, p := range d.Partitions {
			if p.Path == path {
				return p, true
			}
		}
	}
	return Partition{}, false
}

// SizeGiB is the size in GiB, rounded down.
func (d Disk) SizeGiB() int64 { return d.SizeBytes >> 30 }

// Describe is a one-line description for pickers.
func (d Disk) Describe() string {
	parts := []string{d.Path, HumanSize(d.SizeBytes)}
	if d.Model != "" {
		parts = append(parts, d.Model)
	}
	if d.Tran != "" {
		parts = append(parts, d.Tran)
	}
	parts = append(parts, d.Contents)
	return strings.Join(parts, "  ")
}

// HumanSize renders bytes in binary units with one decimal.
func HumanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// Disk returns the disk with the given name or path.
func (f Facts) Disk(nameOrPath string) (Disk, bool) {
	for _, d := range f.Disks {
		if d.Path == nameOrPath || d.Name == nameOrPath || "/dev/"+d.Name == nameOrPath {
			return d, true
		}
	}
	return Disk{}, false
}

// Reader runs a read-only command and returns its standard output.
type Reader func(ctx context.Context, argv ...string) (string, error)

// ExecReader runs commands with os/exec, no shell.
func ExecReader(ctx context.Context, argv ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Prober gathers Facts. Root is "/" on a real machine; tests point it at a
// fixture tree.
type Prober struct {
	Root string
	Read Reader
}

// Probe gathers the facts. A failing source leaves its fact at the zero
// value; only the disk listing is required.
func (p Prober) Probe(ctx context.Context) (Facts, error) {
	if p.Root == "" {
		p.Root = "/"
	}
	if p.Read == nil {
		p.Read = ExecReader
	}
	f := Facts{Arch: archName(), SecureBoot: "unknown"}
	f.UEFI = exists(p.path("/sys/firmware/efi"))
	f.SecureBoot = secureBootState(p.path("/sys/firmware/efi/efivars"))
	f.TPM2 = tpm2Present(p.path("/sys/class/tpm"))
	f.Virt = p.virt(ctx)
	f.MemoryMiB = memoryMiB(p.path("/proc/meminfo"))
	f.Release = osRelease(p.path("/etc/os-release"))
	f.Interfaces = interfaces(p.path("/sys/class/net"))
	out, err := p.Read(ctx, "lsblk", "--json", "--bytes", "--tree", "--output",
		"NAME,PATH,SIZE,TYPE,RM,RO,ROTA,MODEL,SERIAL,TRAN,FSTYPE,MOUNTPOINTS,LABEL,UUID,PARTTYPE,PARTN,START,LOG-SEC,PTTYPE")
	if err != nil {
		return f, fmt.Errorf("listing disks: %w", err)
	}
	disks, err := ParseLsblk([]byte(out))
	if err != nil {
		return f, err
	}
	swaps := activeSwaps(p.path("/proc/swaps"))
	for i := range disks {
		if disks[i].InUse == "" {
			for dev := range swaps {
				if strings.HasPrefix(dev, disks[i].Path) {
					disks[i].InUse = "active swap on " + dev
				}
			}
		}
	}
	f.Disks = disks
	f.KeyMedia, _ = ParseKeyMedia([]byte(out))
	return f, nil
}

func (p Prober) path(rel string) string { return filepath.Join(p.Root, rel) }

func (p Prober) virt(ctx context.Context) string {
	out, _ := p.Read(ctx, "systemd-detect-virt", "--vm")
	v := strings.TrimSpace(out)
	if v != "" && v != "none" {
		return v
	}
	if data, err := os.ReadFile(p.path("/proc/cpuinfo")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "flags") && strings.Contains(" "+line+" ", " hypervisor ") {
				return "hypervisor"
			}
		}
	}
	return ""
}

func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// secureBootState reads the SecureBoot EFI variable: 4 attribute bytes, then
// one byte that is 1 when Secure Boot is enforced.
func secureBootState(efivars string) string {
	data, err := os.ReadFile(filepath.Join(efivars, "SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"))
	if err != nil || len(data) < 5 {
		return "unknown"
	}
	if data[4] == 1 {
		return "enabled"
	}
	return "disabled"
}

func tpm2Present(class string) bool {
	entries, err := os.ReadDir(class)
	if err != nil {
		return false
	}
	for _, e := range entries {
		v, err := os.ReadFile(filepath.Join(class, e.Name(), "tpm_version_major"))
		if err == nil && strings.TrimSpace(string(v)) == "2" {
			return true
		}
	}
	return false
}

func memoryMiB(meminfo string) int {
	f, err := os.Open(meminfo)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(fields[1])
			return kb / 1024
		}
	}
	return 0
}

// osRelease returns the Fedora release of the running system: the major
// number of VERSION_ID (Basalt OS: 44.0, Fedora: 44), else PLATFORM_ID
// (platform:f44).
func osRelease(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	major, _, _ := strings.Cut(kv["VERSION_ID"], ".")
	if n, err := strconv.Atoi(major); err == nil && n > 0 {
		return n
	}
	if v, ok := strings.CutPrefix(kv["PLATFORM_ID"], "platform:f"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func interfaces(class string) []string {
	entries, err := os.ReadDir(class)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Name() != "lo" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func activeSwaps(path string) map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		if f := strings.Fields(line); len(f) > 0 {
			out[f[0]] = true
		}
	}
	return out
}

// lsblk JSON: booleans are true/false in current util-linux and "0"/"1" in
// older releases; MOUNTPOINTS is a list that may hold nulls.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	*b = flexBool(s == "true" || s == "1")
	return nil
}

type flexInt int64

func (n *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" || s == "" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	*n = flexInt(v)
	return err
}

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Size        flexInt    `json:"size"`
	Type        string     `json:"type"`
	RM          flexBool   `json:"rm"`
	RO          flexBool   `json:"ro"`
	Rota        flexBool   `json:"rota"`
	Model       *string    `json:"model"`
	Serial      *string    `json:"serial"`
	Tran        *string    `json:"tran"`
	FSType      *string    `json:"fstype"`
	Mountpoints []*string  `json:"mountpoints"`
	Label       *string    `json:"label"`
	UUID        *string    `json:"uuid"`
	PartType    *string    `json:"parttype"`
	PartN       flexInt    `json:"partn"`
	Start       flexInt    `json:"start"`
	LogSec      flexInt    `json:"log-sec"`
	PTType      *string    `json:"pttype"`
	Children    []lsblkDev `json:"children"`
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// ParseLsblk turns `lsblk --json --bytes --tree` output into whole disks
// with their state. Loop, zram, ROM and RAM devices are left out.
func ParseLsblk(data []byte) ([]Disk, error) {
	var doc struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing lsblk: %w", err)
	}
	var disks []Disk
	for _, d := range doc.Blockdevices {
		if d.Type != "disk" || strings.HasPrefix(d.Name, "zram") || strings.HasPrefix(d.Name, "ram") {
			continue
		}
		disk := Disk{
			Name: d.Name, Path: d.Path, SizeBytes: int64(d.Size),
			Model: str(d.Model), Serial: str(d.Serial), Tran: str(d.Tran),
			Removable: bool(d.RM), ReadOnly: bool(d.RO), Rotational: bool(d.Rota),
		}
		if disk.Path == "" {
			disk.Path = "/dev/" + d.Name
		}
		disk.Contents = contents(d)
		disk.InUse = inUse(d)
		disk.Complex = complexReason(d)
		partitions(&disk, d)
		disks = append(disks, disk)
	}
	return disks, nil
}

func contents(d lsblkDev) string {
	var types []string
	parts := 0
	for _, c := range d.Children {
		if c.Type == "part" {
			parts++
			if fs := str(c.FSType); fs != "" {
				types = append(types, fs)
			}
		}
	}
	if parts == 0 {
		if fs := str(d.FSType); fs != "" {
			return "whole-disk " + fs
		}
		return "empty"
	}
	s := fmt.Sprintf("%d partition", parts)
	if parts > 1 {
		s += "s"
	}
	if len(types) > 0 {
		s += ": " + strings.Join(types, ", ")
	}
	return s
}

// inUse finds a mounted file system or an active mapping anywhere below d.
func inUse(d lsblkDev) string {
	var walk func(n lsblkDev) string
	walk = func(n lsblkDev) string {
		for _, m := range n.Mountpoints {
			if m != nil && *m != "" {
				return fmt.Sprintf("%s is mounted on %s", n.Path, *m)
			}
		}
		switch n.Type {
		case "crypt", "lvm", "dm":
			return fmt.Sprintf("%s (%s) is active", n.Path, n.Type)
		}
		for _, c := range n.Children {
			if r := walk(c); r != "" {
				return r
			}
		}
		return ""
	}
	return walk(d)
}

// complexReason detects the storage setups this installer version leaves to
// the kickstart path.
func complexReason(d lsblkDev) string {
	if strings.EqualFold(str(d.Tran), "iscsi") {
		return "iSCSI disk"
	}
	var walk func(n lsblkDev) string
	walk = func(n lsblkDev) string {
		fs := str(n.FSType)
		switch {
		case strings.HasPrefix(n.Type, "raid") || fs == "linux_raid_member" || fs == "isw_raid_member" || fs == "ddf_raid_member":
			return "member of a RAID array"
		case n.Type == "mpath" || fs == "mpath_member":
			return "multipath device"
		}
		for _, c := range n.Children {
			if r := walk(c); r != "" {
				return r
			}
		}
		return ""
	}
	return walk(d)
}

// partitions fills the partition list and the free regions of a disk.
func partitions(disk *Disk, d lsblkDev) {
	disk.Table = str(d.PTType)
	disk.SectorSize = int64(d.LogSec)
	if disk.SectorSize <= 0 {
		disk.SectorSize = 512
	}
	for _, c := range d.Children {
		if c.Type != "part" {
			continue
		}
		p := Partition{Path: c.Path, Number: int(c.PartN), Start: int64(c.Start), Bytes: int64(c.Size),
			Type: strings.ToLower(str(c.PartType)), FSType: str(c.FSType), Label: str(c.Label), UUID: str(c.UUID), Disk: disk.Path}
		if p.Path == "" {
			p.Path = "/dev/" + c.Name
		}
		p.Sectors = p.Bytes / disk.SectorSize
		for _, m := range c.Mountpoints {
			if m != nil && *m != "" {
				p.Mountpoint = *m
				break
			}
		}
		disk.Partitions = append(disk.Partitions, p)
	}
	sort.Slice(disk.Partitions, func(i, j int) bool { return disk.Partitions[i].Start < disk.Partitions[j].Start })
	if disk.Table != "gpt" {
		return
	}
	total := disk.SizeBytes / disk.SectorSize
	align := int64(alignSectors) * 512 / disk.SectorSize
	if align < 1 {
		align = 1
	}
	pos, end := align, total-gptTailBlocks
	add := func(from, to int64) { // [from, to)
		from = (from + align - 1) / align * align
		to = to / align * align
		if to-from >= align {
			disk.Free = append(disk.Free, Region{Start: from, Sectors: to - from, Bytes: (to - from) * disk.SectorSize})
		}
	}
	for _, p := range disk.Partitions {
		if p.Start > pos {
			add(pos, p.Start)
		}
		if e := p.Start + p.Sectors; e > pos {
			pos = e
		}
	}
	if end > pos {
		add(pos, end)
	}
}
