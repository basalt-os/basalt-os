package plan

import (
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// Minimum sizes of reused partitions.
const (
	MinBootMiB   = 500
	MinSystemGiB = 8
)

// windowsPart reports whether a partition belongs to Windows or holds NTFS
// or BitLocker data: never formatted by this installer, even when chosen
// (delete or shrink it from Windows first).
func windowsPart(part probe.Partition) bool {
	fs := strings.ToLower(part.FSType)
	switch part.Type {
	case probe.TypeMSR, probe.TypeWinRecov:
		return true
	case probe.TypeWindows:
		// "Basic data" is also used by Linux tools for plain data
		// partitions: a Linux file system on it is not Windows'.
		if fs != "ext4" && fs != "ext3" && fs != "ext2" && fs != "xfs" && fs != "btrfs" && fs != "crypto_luks" {
			return true
		}
	}
	return fs == "ntfs" || fs == "bitlocker" || fs == "exfat" || fs == "refs"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// validateReuse checks the existing partitions a custom layout formats
// (target.boot, target.system): each must be a partition of the target
// disk the person chose, not in use, not the ESP, not the adopted /home,
// not Windows data, and large enough.
func validateReuse(p Plan, disk probe.Disk, add func(Severity, string, string, ...any)) {
	check := func(field, dev string, min int64) {
		if dev == "" {
			return
		}
		var part *probe.Partition
		for i := range disk.Partitions {
			if disk.Partitions[i].Path == dev {
				part = &disk.Partitions[i]
			}
		}
		switch {
		case part == nil:
			add(Error, field, i18n.T("%s is not a partition of %s"), dev, disk.Path)
			return
		case part.Mountpoint != "":
			add(Error, field, i18n.T("%s is mounted on %s"), dev, part.Mountpoint)
		case part.Type == probe.TypeESP || dev == p.Target.ESP:
			add(Error, field, i18n.T("%s is an EFI system partition: it is never formatted"), dev)
		case p.Home.Existing != nil && p.Home.Existing.Device == dev:
			add(Error, field, i18n.T("%s is the existing /home, which is kept"), dev)
		case windowsPart(*part):
			add(Error, field, i18n.T("%s holds Windows or NTFS/BitLocker data (%s): this installer never formats it; delete or shrink it from Windows first"),
				dev, orDash(part.FSType))
		case part.Bytes < min:
			add(Error, field, i18n.T("%s is too small (%s, at least %s)"), dev, probe.HumanSize(part.Bytes), probe.HumanSize(min))
		case part.FSType != "":
			add(Warning, field, i18n.T("%s (%s) is formatted: everything on it is lost"), dev, part.Describe())
		}
	}
	check("target.boot", p.Target.Boot, MinBootMiB<<20)
	check("target.system", p.Target.System, int64(MinSystemGiB)<<30)
	if p.Target.Boot != "" && p.Target.Boot == p.Target.System {
		add(Error, "target.system", "%s", i18n.T("/boot and the system need two different partitions"))
	}
}

// validateOtherSystems warns about what the other systems need: the
// Microsoft certificates in the firmware's db (Windows and other
// distributions' shim are signed through them), an EFI/fedora directory a
// shared ESP would replace, and the recovery key after Windows updates.
func validateOtherSystems(p Plan, disk probe.Disk, f *probe.Facts, add func(Severity, string, string, ...any)) {
	if f == nil {
		return
	}
	windows, linux := false, false
	for _, d := range f.Disks {
		if d.Path == disk.Path && p.Target.Wipe {
			continue // erased: nothing on it boots afterwards
		}
		for _, s := range d.Systems {
			if strings.Contains(strings.ToLower(s), "windows") {
				windows = true
			} else {
				linux = true
			}
		}
	}
	if p.Target.Wipe && disk.Caution != "" {
		add(Warning, "target.disk", i18n.T("%s %s: erasing the disk deletes it"), describeDisk(disk), disk.Caution)
	}
	if f.SecureBoot == "enabled" && f.SecureBootDB.Read {
		if windows && !f.SecureBootDB.WindowsCA {
			add(Warning, "secure_boot", "%s", i18n.T("the firmware's Secure Boot db has no Microsoft Windows certificate (custom keys): Windows will not start while Secure Boot is on; restore the factory keys or add Microsoft's certificates to db"))
		}
		if linux && !f.SecureBootDB.UEFICA {
			add(Warning, "secure_boot", "%s", i18n.T("the firmware's Secure Boot db has no Microsoft UEFI CA (custom keys): other Linux systems booted through their own shim will be refused while Secure Boot is on"))
		}
	}
	if p.Target.ESP != "" {
		for _, e := range f.BootEntries {
			if esp, ok := f.Partition(p.Target.ESP); ok && e.PartUUID != "" && strings.EqualFold(e.PartUUID, esp.PartUUID) &&
				strings.Contains(strings.ToLower(e.Loader), `\efi\fedora\`) {
				add(Error, "target.esp", i18n.T("%s already holds a Fedora boot loader (boot entry %q): sharing it would replace that system's; let the installer create a new EFI system partition"),
					p.Target.ESP, e.Name)
			}
		}
	}
	if windows && p.Encrypted() && strings.Contains(p.Encryption.Unlock, "tpm2") {
		add(Warning, "encryption.unlock", "%s", i18n.T("Windows is on this machine: a Windows or firmware update that changes the Secure Boot keys or revocations (PCR 7) makes the next Basalt OS start ask for the recovery key. Keep it at hand"))
	}
}

// describeDisk names a disk the way a person recognizes it: path, model,
// serial and size.
func describeDisk(d probe.Disk) string {
	s := d.Path
	if d.Model != "" {
		s += " (" + d.Model
		if d.Serial != "" {
			s += ", serial " + d.Serial
		}
		s += ", " + probe.HumanSize(d.SizeBytes) + ")"
	} else if d.Serial != "" {
		s += " (serial " + d.Serial + ", " + probe.HumanSize(d.SizeBytes) + ")"
	}
	return s
}

// DescribeDisk is describeDisk for the frontends.
func DescribeDisk(d probe.Disk) string { return describeDisk(d) }
