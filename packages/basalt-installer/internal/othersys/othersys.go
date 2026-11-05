// Package othersys finds the boot loaders of the other systems on the
// machine (Windows' boot manager, other distributions' shim or GRUB) on
// their EFI system partitions and writes GRUB menu entries that chainload
// them. It only reads: each partition is mounted read-only (FAT has no
// journal to replay), nothing on it is written, and other file systems
// are never mounted. This replaces os-prober, which mounts btrfs volumes
// read-write.
package othersys

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Loader is one boot loader found on an EFI system partition.
type Loader struct {
	Name   string `json:"name"`
	Dev    string `json:"dev"`
	FSUUID string `json:"fs_uuid"` // the FAT volume ID (XXXX-XXXX)
	Path   string `json:"path"`    // /EFI/Microsoft/Boot/bootmgfw.efi
	Class  string `json:"class"`   // windows, os
}

// names of well-known EFI vendor directories.
var names = map[string]string{
	"microsoft": "Windows Boot Manager", "fedora": "Fedora", "ubuntu": "Ubuntu", "debian": "Debian",
	"opensuse": "openSUSE", "sles": "SUSE Linux Enterprise", "redhat": "Red Hat Enterprise Linux",
	"centos": "CentOS", "rocky": "Rocky Linux", "almalinux": "AlmaLinux", "arch": "Arch Linux",
	"manjaro": "Manjaro", "pop": "Pop!_OS", "linuxmint": "Linux Mint", "systemd": "Linux (systemd-boot)",
	"nixos": "NixOS", "elementary": "elementary OS", "kali": "Kali Linux", "gentoo": "Gentoo",
}

// safeName is what a directory or file name on an ESP may be to reach the
// GRUB script: it is written unquoted after chainloader and inside the
// entry title, so anything else (spaces, quotes, $, ;, newlines) is
// skipped, never escaped.
var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// loaders in order of preference inside a vendor directory: shim first, so
// a Secure Boot chain stays the vendor's.
var loaders = []string{"shimx64.efi", "grubx64.efi", "systemd-bootx64.efi"}

// Scan lists the boot loaders under root (a mounted EFI system partition).
// skip names vendor directories that belong to Basalt OS itself (fedora on
// a shared ESP). The fallback directory EFI/BOOT is never listed: it only
// repeats one of the others.
func Scan(root, dev, fsUUID string, skip []string) []Loader {
	efi := findDir(root, "EFI")
	if efi == "" {
		return nil
	}
	entries, err := os.ReadDir(efi)
	if err != nil {
		return nil
	}
	var out []Loader
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		vendor := strings.ToLower(e.Name())
		if !safeName.MatchString(e.Name()) || vendor == "boot" || contains(skip, vendor) {
			continue
		}
		dir := filepath.Join(efi, e.Name())
		if vendor == "microsoft" {
			if b := findDir(dir, "Boot"); b != "" && safeName.MatchString(filepath.Base(b)) {
				if f := findFile(b, "bootmgfw.efi"); f != "" {
					out = append(out, Loader{Name: names[vendor], Dev: dev, FSUUID: fsUUID, Class: "windows",
						Path: "/EFI/" + e.Name() + "/" + filepath.Base(b) + "/" + filepath.Base(f)})
				}
			}
			continue
		}
		for _, l := range loaders {
			if f := findFile(dir, l); f != "" {
				name := names[vendor]
				if name == "" {
					name = e.Name()
				}
				out = append(out, Loader{Name: name, Dev: dev, FSUUID: fsUUID, Class: "os", Path: "/EFI/" + e.Name() + "/" + filepath.Base(f)})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Class == "windows" && out[j].Class != "windows" })
	return out
}

// FAT is case-insensitive; the names on it are not always the case we
// expect.
func findDir(parent, name string) string {
	return findEntry(parent, name, true)
}

func findFile(parent, name string) string {
	return findEntry(parent, name, false)
}

func findEntry(parent, name string, dir bool) string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) && e.IsDir() == dir && safeName.MatchString(e.Name()) {
			return filepath.Join(parent, e.Name())
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// Script is the /etc/grub.d script that adds a chainload entry for each
// loader (grub2-mkconfig runs it and copies its output into grub.cfg).
func Script(list []Loader) string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/sh\n# Written by the Basalt OS installer: the other systems found on this machine's\n" +
		"# EFI system partitions when Basalt OS was installed (read-only search).\n# Remove this file to drop them from the boot menu.\nset -e\ncat <<'EOF'\n")
	for _, l := range list {
		if !regexp.MustCompile(`^[A-Za-z0-9 ._()!-]{1,64}$`).MatchString(l.Name) ||
			!regexp.MustCompile(`^/EFI(/[A-Za-z0-9._-]{1,64}){1,3}$`).MatchString(l.Path) ||
			!regexp.MustCompile(`^[0-9A-Fa-f-]{4,40}$`).MatchString(l.FSUUID) || !regexp.MustCompile(`^/dev/[A-Za-z0-9/_-]+$`).MatchString(l.Dev) {
			continue // written unquoted into a shell heredoc and a GRUB script
		}
		title := fmt.Sprintf("%s (%s)", l.Name, l.Dev)
		class := "--class os"
		if l.Class != "os" {
			class = "--class " + l.Class + " " + class
		}
		fmt.Fprintf(&b, "menuentry '%s' %s {\n\tinsmod part_gpt\n\tinsmod fat\n\tinsmod chain\n"+
			"\tsearch --no-floppy --fs-uuid --set=root %s\n\tchainloader %s\n}\n", strings.ReplaceAll(title, "'", ""), class, l.FSUUID, l.Path)
	}
	b.WriteString("EOF\n")
	return b.String()
}

// Partition is an EFI system partition to search: device, FAT volume ID
// and the vendor directories to skip on it.
type Partition struct {
	Dev, FSUUID string
	Skip        []string
}

// ParseArg reads DEV=UUID[:skip1,skip2].
func ParseArg(s string) (Partition, error) {
	dev, rest, ok := strings.Cut(s, "=")
	if !ok || !strings.HasPrefix(dev, "/dev/") {
		return Partition{}, fmt.Errorf("want DEV=FSUUID[:skip,...], got %q", s)
	}
	uuid, skip, _ := strings.Cut(rest, ":")
	p := Partition{Dev: dev, FSUUID: uuid}
	if skip != "" {
		p.Skip = strings.Split(skip, ",")
	}
	return p, nil
}

// Find mounts each partition read-only under work, scans it and unmounts
// it. A partition that cannot be mounted is reported and skipped.
func Find(ctx context.Context, work string, parts []Partition, logf func(string, ...any)) []Loader {
	var out []Loader
	for i, p := range parts {
		dir := filepath.Join(work, fmt.Sprintf("other-esp-%d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logf("%s: %v", p.Dev, err)
			continue
		}
		if b, err := exec.CommandContext(ctx, "mount", "-t", "vfat", "-o", "ro,nosuid,nodev,noexec", p.Dev, dir).CombinedOutput(); err != nil {
			logf("%s: not searched: %v: %s", p.Dev, err, strings.TrimSpace(string(b)))
			_ = os.Remove(dir)
			continue
		}
		found := Scan(dir, p.Dev, p.FSUUID, p.Skip)
		for _, l := range found {
			logf("found %s: %s on %s", l.Name, l.Path, l.Dev)
		}
		out = append(out, found...)
		if b, err := exec.CommandContext(ctx, "umount", dir).CombinedOutput(); err != nil {
			logf("%s: unmount: %v: %s", p.Dev, err, strings.TrimSpace(string(b)))
			continue
		}
		_ = os.Remove(dir)
	}
	return out
}
