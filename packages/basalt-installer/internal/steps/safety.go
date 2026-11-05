package steps

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// CheckSafety refuses a step list that would write to anything the plan
// did not choose. It looks at every command, whatever generated it:
//
//   - the partition table: only the target disk's, rewritten only when the
//     plan erases that disk (wipe); otherwise only new entries are added
//     (sgdisk --new, with their type and name), and the undo deletes only
//     those new entries;
//   - file systems, signatures and LUKS headers: only on the partitions the
//     installation creates, the existing partitions the person chose to
//     reuse (target.boot, target.system), and the volumes opened on them;
//   - LUKS key slots: the new volume, and the existing /home only when the
//     plan asks to enroll this machine's TPM there;
//   - firmware boot entries: only added, never deleted or reordered;
//   - files (rm, chmod, chown, install -d and the steps that write a
//     file): only in the target root or the installer's work directory,
//     never a device path, never on an adopted /home except the new
//     user's ~/.ssh; btrfs only creates subvolumes on the new file system;
//   - only the commands of the allow-list below run at all (anything else,
//     dd, blkdiscard, sfdisk, parted, mkswap included, is refused).
//
// Every other partition, on every disk, is left exactly as it is: an NTFS
// or BitLocker Windows, another Linux, an encrypted volume, the shared EFI
// system partition (files are added under EFI/fedora, it is not
// formatted).
func CheckSafety(r Resolved, list []Step) error {
	p := r.Plan
	disk := r.Disk.Path
	writable := map[string]bool{}
	newNums := map[string]bool{}
	for _, np := range r.NewParts {
		writable[np.Dev] = true
		newNums[fmt.Sprint(np.Number)] = true
	}
	for _, rp := range r.Reused {
		writable[rp.Path] = true
	}
	if p.Target.Wipe {
		writable[disk] = true
	}
	mapped := map[string]bool{}
	if p.Encrypted() {
		mapped["/dev/mapper/"+r.CryptName()] = true
	}
	keyslots := map[string]bool{}
	if p.Encrypted() {
		keyslots[r.LUKSDev()] = true
	}
	if e := r.Existing; e != nil && e.LUKS && p.Home.Existing.TPM2 && r.TPM2 {
		keyslots[e.Device] = true
	}
	mountable := map[string]bool{}
	if e := r.Existing; e != nil {
		mountable[e.Device] = true
		mountable["/dev/mapper/"+e.Mapper()] = true
	}

	var errs []string
	bad := func(s Step, why string, a ...any) {
		errs = append(errs, fmt.Sprintf("step %q: %s", s.Title, fmt.Sprintf(why, a...)))
	}
	devArgs := func(argv []string) []string {
		var out []string
		for _, a := range argv {
			if strings.HasPrefix(a, "/dev/") && !strings.HasPrefix(a, "/dev/mapper/") {
				out = append(out, a)
			} else if strings.HasPrefix(a, "/dev/mapper/") {
				out = append(out, a)
			}
		}
		return out
	}
	paths := pathRules(r)
	check := func(s Step, argv []string, undo bool) {
		inTarget := false
		if len(argv) >= 2 && argv[0] == "chroot" {
			if filepath.Clean(argv[1]) != filepath.Clean(r.Root) {
				bad(s, "chroot into %s, not the target %s", argv[1], r.Root)
			}
			argv = argv[2:]
			inTarget = true
		}
		if len(argv) == 0 {
			return
		}
		cmd := filepath.Base(argv[0])
		if !allowed[cmd] {
			bad(s, "%s is not a command the installer uses", cmd)
			return
		}
		switch {
		case cmd == "rm" || cmd == "chmod" || cmd == "chown" || cmd == "install":
			ops, err := fileOperands(cmd, argv[1:])
			if err != "" {
				bad(s, "%s", err)
			}
			for _, a := range ops {
				// install -d creates the target and work directories themselves.
				if why := paths.refuse(a, inTarget, cmd == "install"); why != "" {
					bad(s, "%s %s: %s", cmd, a, why)
				}
			}
		case cmd == "btrfs":
			// The generator only creates the subvolumes, under the
			// top level it mounted in the installer's work directory.
			if len(argv) != 4 || argv[1] != "subvolume" || argv[2] != "create" {
				bad(s, "btrfs %s: only \"btrfs subvolume create PATH\" is used", strings.Join(argv[1:], " "))
			} else if inTarget || !within(argv[3], r.TopMount()) || filepath.Clean(argv[3]) == filepath.Clean(r.TopMount()) {
				bad(s, "btrfs subvolume create %s: not under the new file system's top level %s", argv[3], r.TopMount())
			}
		case cmd == "sgdisk":
			whole := false
			for _, a := range argv[1:] {
				switch {
				case a == "--zap-all" || a == "--clear" || a == "-Z" || a == "-o" || strings.HasPrefix(a, "--load-backup") || a == "--mbrtogpt":
					whole = true
				case strings.HasPrefix(a, "--new="), strings.HasPrefix(a, "--typecode="), strings.HasPrefix(a, "--change-name="), strings.HasPrefix(a, "--delete="):
					num, _, _ := strings.Cut(strings.SplitN(a, "=", 2)[1], ":")
					if !p.Target.Wipe && !newNums[num] {
						bad(s, "changes partition %s of %s, which the installation does not create", num, disk)
					}
				case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--print") && a != "--info":
					bad(s, "sgdisk option %s is not one the installer uses", a)
				}
			}
			for _, d := range devArgs(argv[1:]) {
				if d != disk {
					bad(s, "writes the partition table of %s, not the target disk %s", d, disk)
				}
			}
			if whole && !p.Target.Wipe {
				bad(s, "rewrites the whole partition table of %s, but the plan keeps its partitions", disk)
			}
		case cmd == "wipefs" || strings.HasPrefix(cmd, "mkfs") || cmd == "mke2fs" || cmd == "mkdosfs":
			for _, d := range devArgs(argv[1:]) {
				if !writable[d] && !mapped[d] {
					bad(s, "%s %s: not a partition the installation creates or the person chose to reuse", cmd, d)
				}
			}
		case cmd == "cryptsetup":
			if len(argv) > 1 && (argv[1] == "luksFormat" || argv[1] == "erase" || argv[1] == "luksErase" || argv[1] == "reencrypt" ||
				argv[1] == "luksAddKey" || argv[1] == "luksKillSlot" || argv[1] == "luksRemoveKey" || argv[1] == "luksHeaderRestore") {
				for _, d := range devArgs(argv[2:]) {
					if !writable[d] {
						bad(s, "cryptsetup %s on %s, which is not a new or chosen partition", argv[1], d)
					}
				}
			}
		case cmd == "systemd-cryptenroll" || (cmd == "clevis" && len(argv) > 2 && argv[1] == "luks"):
			for _, d := range devArgs(argv[1:]) {
				if !keyslots[d] {
					bad(s, "%s changes the key slots of %s", cmd, d)
				}
			}
		case cmd == "efibootmgr":
			for _, a := range argv[1:] {
				switch a {
				case "-B", "--delete-bootnum", "-o", "--bootorder", "-O", "--delete-bootorder", "-N", "--delete-bootnext":
					bad(s, "efibootmgr %s changes the other boot entries", a)
				}
			}
		case cmd == "mount":
			// Mounted read-write: the new and reused partitions, the
			// shared ESP (files go into EFI/fedora) and the adopted /home.
			for _, d := range devArgs(argv[1:]) {
				if !writable[d] && !mapped[d] && d != r.ESPDev && !mountable[d] {
					bad(s, "mounts %s, which the plan does not use", d)
				}
			}
		}
		if undo && cmd == "sgdisk" {
			for _, a := range argv[1:] {
				if !strings.HasPrefix(a, "--delete=") && strings.HasPrefix(a, "-") {
					bad(s, "the undo may only delete the new partitions, not run %s", a)
				}
			}
		}
	}
	for _, s := range list {
		check(s, s.Argv, false)
		check(s, s.Undo, true)
		if s.Write != nil && reDevPath.MatchString(s.Write.Path) {
			bad(s, "writes a file to a device path %s", s.Write.Path)
		} else if s.Write != nil {
			if why := paths.refuse(s.Write.Path, false, false); why != "" {
				bad(s, "writes %s: %s", s.Write.Path, why)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("the generated steps would touch what the plan did not choose:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// allowed are the commands the generator uses (inside the target with
// chroot or on the live system). The write-capable ones are checked
// above; a command not listed here never runs.
var allowed = map[string]bool{
	"udevadm": true, "install": true, "mkdir": true, "chmod": true, "chown": true, "rm": true, "sync": true, "lsblk": true,
	"sgdisk": true, "wipefs": true, "mkfs.vfat": true, "mkfs.ext4": true, "mkfs.btrfs": true, "btrfs": true,
	"cryptsetup": true, "systemd-cryptenroll": true, "clevis": true, "mount": true, "umount": true,
	"dnf": true, "rpm": true, "dracut": true, "setfiles": true, "restorecon": true, "systemctl": true,
	"plymouth-set-default-theme": true, "grub2-editenv": true, "grub2-mkconfig": true, "efibootmgr": true,
	"basalt-snapshots-setup": true, "basalt-installer": true,
	"usermod": true, "useradd": true, "groupadd": true, "chpasswd": true,
}

var reDevPath = regexp.MustCompile(`^/dev/`)

// fileOperands returns the paths rm, chmod, chown and install act on,
// accepting only the options the generator uses: rm -f FILE, chmod MODE
// PATH, chown OWNER PATH and install -d [-m MODE] [-o USER] [-g GROUP]
// DIR. Anything else (rm -r, chmod -R, install copying a file) is refused.
func fileOperands(cmd string, args []string) ([]string, string) {
	var ops []string
	switch cmd {
	case "rm":
		for _, a := range args {
			switch {
			case a == "-f":
			case strings.HasPrefix(a, "-"):
				return nil, "rm option " + a + " is not one the installer uses"
			default:
				ops = append(ops, a)
			}
		}
	case "chmod", "chown":
		if len(args) < 2 {
			return nil, cmd + " needs a mode or owner and a path"
		}
		if strings.HasPrefix(args[0], "-") {
			return nil, cmd + " option " + args[0] + " is not one the installer uses"
		}
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				return nil, cmd + " option " + a + " is not one the installer uses"
			}
			ops = append(ops, a)
		}
	case "install":
		dir := false
		for i := 0; i < len(args); i++ {
			switch a := args[i]; {
			case a == "-d":
				dir = true
			case a == "-m" || a == "-o" || a == "-g":
				i++ // the option's value
			case strings.HasPrefix(a, "-"):
				return nil, "install option " + a + " is not one the installer uses"
			default:
				ops = append(ops, a)
			}
		}
		if !dir {
			return nil, "install is used only to create directories (install -d)"
		}
	}
	if len(ops) == 0 {
		return nil, cmd + " without a path"
	}
	return ops, ""
}

// paths says where rm, chmod, chown, install and file writes may act: in
// the installer's work directory (memory only) or in the target root, never
// on a device node, and never inside an adopted /home, which is mounted at
// the target's /home and keeps its files as they are. The one exception on
// an adopted /home is what the generator creates for the new user: the
// user's ~/.ssh and ~/.ssh/authorized_keys.
type paths struct {
	root, work string
	home       string          // the adopted /home's mount point in the target, "" without one
	userSSH    map[string]bool // on the adopted /home: the new user's ~/.ssh entries
}

func pathRules(r Resolved) paths {
	ps := paths{root: filepath.Clean(r.Root), work: filepath.Clean(r.Work), userSSH: map[string]bool{}}
	if r.Existing != nil {
		ps.home = filepath.Clean(r.Target("/home"))
		if u := r.Plan.Accounts.User; u != nil {
			home := u.HomeDir
			if home == "" {
				home = "/home/" + u.Name
			}
			ssh := r.Target(filepath.Join(home, ".ssh"))
			ps.userSSH[ssh] = true
			ps.userSSH[filepath.Join(ssh, "authorized_keys")] = true
		}
	}
	return ps
}

// refuse returns why a path may not be changed, or "". inTarget is true for
// a command run with chroot into the target: its paths are inside the
// installed system. self allows the target root and the work directory
// themselves (install -d creates them).
func (ps paths) refuse(path string, inTarget, self bool) string {
	if !filepath.IsAbs(path) {
		return "not an absolute path"
	}
	for _, el := range strings.Split(path, "/") {
		if el == ".." {
			return "the path climbs with .."
		}
	}
	host := filepath.Clean(path)
	if inTarget {
		host = filepath.Join(ps.root, path)
	}
	if strings.HasPrefix(path, "/dev/") || path == "/dev" || within(host, filepath.Join(ps.root, "dev")) {
		return "a device path"
	}
	if !within(host, ps.root) && !within(host, ps.work) {
		return "outside the target " + ps.root + " and the installer's work directory " + ps.work
	}
	if !self && (host == ps.root || host == ps.work) {
		return "the target or work directory itself"
	}
	if ps.home != "" && within(host, ps.home) && !ps.userSSH[host] {
		return "on the adopted /home, which the installation does not change"
	}
	return ""
}

// within reports whether path is dir or below it (both cleaned).
func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+"/")
}
