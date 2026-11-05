package probe

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// What is on a disk, in words a person checks before choosing it: other
// operating systems, encrypted volumes, file systems with data. Probing
// only reads the partition table, lsblk's signatures and the firmware's
// boot entries; no file system is mounted.

// BootEntry is one UEFI boot entry (efibootmgr): its name and the GPT
// partition (PARTUUID) of the loader it starts.
type BootEntry struct {
	Num      string `json:"num"`
	Name     string `json:"name"`
	PartUUID string `json:"partuuid,omitempty"`
	Loader   string `json:"loader,omitempty"`
}

// reBootEntry matches efibootmgr lines such as
// "Boot0001* Windows Boot Manager	HD(1,GPT,0f3c...,0x800,0x32000)/\EFI\Microsoft\Boot\bootmgfw.efi".
var reBootEntry = regexp.MustCompile(`^Boot([0-9A-Fa-f]{4})\*?\s+(.*?)\s+HD\(\d+,GPT,([0-9A-Fa-f-]{36}),[^)]*\)(?:/(?:File\()?([^)\s]*)\)?)?`)

// ParseBootEntries reads `efibootmgr` (or `efibootmgr --verbose`) output.
// Entries without a GPT disk path (network boot, the firmware's own) are
// left out.
func ParseBootEntries(out string) []BootEntry {
	var list []BootEntry
	for _, l := range strings.Split(out, "\n") {
		m := reBootEntry.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil {
			continue
		}
		list = append(list, BootEntry{Num: strings.ToUpper(m[1]), Name: strings.TrimSpace(m[2]),
			PartUUID: strings.ToLower(m[3]), Loader: m[4]})
	}
	return list
}

// SecureBootDB says which Microsoft certificates the firmware's db holds:
// Windows' boot manager is signed through the Windows Production PCA 2011
// (and the Windows UEFI CA 2023), other distributions' shim through the
// Microsoft UEFI CA 2011 (and 2023). A db without them (custom db mode
// without those certificates) refuses Windows and other Linux systems.
type SecureBootDB struct {
	Read      bool `json:"read"`
	WindowsCA bool `json:"windows_ca"`
	UEFICA    bool `json:"uefi_ca"`
}

// dbVar is the db variable (EFI_IMAGE_SECURITY_DATABASE_GUID).
const dbVar = "db-d719b2cb-3d3a-4596-a3bc-dad00e67656f"

// ReadSecureBootDB looks for the Microsoft certificates' common names in
// the raw db variable (the certificates are DER; their subject names are
// plain ASCII inside).
func ReadSecureBootDB(efivars string) SecureBootDB {
	data, err := os.ReadFile(filepath.Join(efivars, dbVar))
	if err != nil || len(data) < 4 {
		return SecureBootDB{}
	}
	has := func(s string) bool { return bytes.Contains(data, []byte(s)) }
	return SecureBootDB{Read: true,
		WindowsCA: has("Microsoft Windows Production PCA 2011") || has("Windows UEFI CA 2023"),
		UEFICA:    has("Microsoft Corporation UEFI CA 2011") || has("Microsoft UEFI CA 2023")}
}

// linuxRoot are the GPT root partition types of the Discoverable
// Partitions Specification (x86-64, aarch64, x86).
var linuxRoot = map[string]bool{"4f68bce3-e8cd-4db1-96e7-fbcaf984b709": true, "b921b045-1df0-41c3-af44-4c6f280d3fae": true,
	"44479540-f297-41b2-9af7-d131d5f0458a": true}

// Linux file systems that may hold another installed system.
var linuxFS = map[string]bool{"ext4": true, "ext3": true, "ext2": true, "xfs": true, "btrfs": true, "f2fs": true}

// DescribeHolds fills Holds, Systems and Caution of a disk from its
// partitions and the firmware's boot entries.
func DescribeHolds(d *Disk, entries []BootEntry) {
	var holds, systems []string
	add := func(list *[]string, s string) {
		for _, x := range *list {
			if x == s {
				return
			}
		}
		*list = append(*list, s)
	}
	windows, data := false, false
	for _, p := range d.Partitions {
		fs := strings.ToLower(p.FSType)
		switch {
		case p.Type == TypeESP:
			add(&holds, "EFI system partition "+p.Path)
		case fs == "bitlocker":
			windows, data = true, true
			add(&holds, "BitLocker encrypted volume "+p.Path+" (Windows)")
		case p.Type == TypeMSR || p.Type == TypeWinRecov:
			windows = true
		case fs == "ntfs":
			data = true
			add(&holds, "NTFS "+p.Path+labelSuffix(p.Label)+" (Windows or data)")
			if p.Type == TypeWindows {
				windows = true
			}
		case fs == "crypto_luks":
			data = true
			add(&holds, "LUKS encrypted volume "+p.Path+labelSuffix(p.Label))
		case linuxFS[fs]:
			data = true
			add(&holds, "Linux file system "+p.Path+" ("+fs+labelSuffix(p.Label)+")")
			// The Discoverable Partitions root types name an installed
			// Linux system even without a firmware boot entry.
			if linuxRoot[p.Type] {
				name := "Linux"
				if p.Label != "" {
					name += " (" + p.Label + ")"
				}
				add(&systems, name)
			}
		case fs == "swap":
			add(&holds, "swap "+p.Path)
		case fs != "":
			data = true
			add(&holds, fs+" "+p.Path+labelSuffix(p.Label))
		}
		for _, e := range entries {
			if e.PartUUID != "" && strings.EqualFold(e.PartUUID, p.PartUUID) {
				add(&systems, e.Name)
			}
		}
	}
	if len(d.Partitions) == 0 && d.Contents != "empty" {
		data = true
		switch {
		case strings.HasSuffix(d.Contents, "crypto_LUKS"):
			add(&holds, "LUKS encrypted volume on the whole disk")
		case strings.HasSuffix(strings.ToLower(d.Contents), "bitlocker"):
			add(&holds, "BitLocker encrypted volume on the whole disk")
		default:
			add(&holds, d.Contents)
		}
	}
	if windows {
		found := false
		for _, s := range systems {
			found = found || strings.Contains(strings.ToLower(s), "windows")
		}
		if !found {
			add(&systems, "Windows")
		}
	}
	d.Holds, d.Systems = holds, systems
	var why []string
	if len(systems) > 0 {
		why = append(why, "another operating system ("+strings.Join(systems, ", ")+")")
	}
	for _, h := range holds {
		if strings.HasPrefix(h, "LUKS") || strings.HasPrefix(h, "BitLocker") {
			why = append(why, "an encrypted volume")
			break
		}
	}
	if len(why) == 0 && data {
		why = append(why, "file systems with data")
	}
	if len(why) > 0 {
		d.Caution = "holds " + strings.Join(why, " and ")
	}
}

func labelSuffix(l string) string {
	if l == "" {
		return ""
	}
	return " \"" + l + "\""
}

// LiveSettings are the language, keyboard and time zone of the running
// (live) system: the installer's defaults for the installed one.
type LiveSettings struct {
	Locale   string `json:"locale,omitempty"`
	Keymap   string `json:"keymap,omitempty"`
	Layout   string `json:"xkb_layout,omitempty"`
	Variant  string `json:"xkb_variant,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// readLive reads /etc/locale.conf, /etc/vconsole.conf, the X11 keyboard
// configuration localectl writes, and the /etc/localtime link.
func (p Prober) readLive() LiveSettings {
	var l LiveSettings
	kv := func(path string) map[string]string {
		out := map[string]string{}
		data, err := os.ReadFile(p.path(path))
		if err != nil {
			return out
		}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				out[k] = strings.Trim(v, `"'`)
			}
		}
		return out
	}
	l.Locale = kv("/etc/locale.conf")["LANG"]
	vc := kv("/etc/vconsole.conf")
	l.Keymap, l.Layout, l.Variant = vc["KEYMAP"], vc["XKBLAYOUT"], vc["XKBVARIANT"]
	if data, err := os.ReadFile(p.path("/etc/X11/xorg.conf.d/00-keyboard.conf")); err == nil {
		re := regexp.MustCompile(`(?m)^\s*Option\s+"(XkbLayout|XkbVariant)"\s+"([^"]*)"`)
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			if m[1] == "XkbLayout" {
				l.Layout = m[2]
			} else {
				l.Variant = m[2]
			}
		}
	}
	if t, err := os.Readlink(p.path("/etc/localtime")); err == nil {
		if _, zone, ok := strings.Cut(t, "zoneinfo/"); ok {
			l.Timezone = zone
		}
	}
	return l
}
