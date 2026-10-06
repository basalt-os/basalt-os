package steps

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/pgpkey"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// TUIToolsKey is the tui-tools repository signing key (ADR 0005: official
// third-party repository, key fingerprint pinned).
//
//go:embed RPM-GPG-KEY-tui-tools
var TUIToolsKey []byte

// TUIToolsFingerprint is the pinned fingerprint of TUIToolsKey.
const TUIToolsFingerprint = "767CFB337B01F32FFC073F3F389120B277E4FB44"

// OpenBasaltReleaseFingerprint is the primary key of the OpenBasalt release
// key (https://obpkg.org/keys/openbasalt-release-key.asc); its packages
// subkey signs the Basalt OS repository. basalt-release ships the key; the
// installer only reports whether the key on the media is this one.
const OpenBasaltReleaseFingerprint = "3601734842BD4E482D19DE4AE4EED5ECA395B302"

// TUIToolsBaseURL is the upstream tui-tools RPM repository.
const TUIToolsBaseURL = "https://pkgs.tui.tools/rpm/$basearch"

// Services enabled on the installed system (the same list as the kickstart).
var baseServices = []string{"sshd", "firewalld", "auditd", "basalt-snapshot-boot", "basalt-initial-snapshot",
	"basalt-grub-theme", "basalt-module-keys", "basalt-resolver", "basalt-ledger"}
var assistantServices = []string{"basalt-assistantd", "basalt-notify", "basalt-audit-rotate.timer"}

// DesktopPackages are what the desktop edition installs on top of the
// server system, whatever the plan's packages.extra says: the session,
// the shell's SELinux module, voice (whisper.cpp), the consented model
// downloads and the local model service. No model is installed: the
// desktop asks the person before it downloads one (basalt-models).
var DesktopPackages = []string{"basalt-desktop", "basalt-shell-selinux", "basalt-greeter-selinux", "basalt-voice", "basalt-models",
	"basalt-llm", "basalt-llm-selinux"}

// SplashTheme is the boot splash theme for the system's locale. Plymouth
// cannot know the language in the initramfs, so each language is its own
// theme (plymouth-theme-basalt: basalt in English, basalt-pt_BR).
func SplashTheme(locale string) string {
	if strings.HasPrefix(locale, "pt") {
		return "basalt-pt_BR"
	}
	return "basalt"
}

// Packages returns what dnf installs and excludes for the resolved plan
// (the kickstart's %packages, plus what Anaconda adds by itself: the kernel
// and the UEFI boot loader).
func Packages(r Resolved) (install, exclude []string) {
	install = []string{"@core", "kernel", "shim-x64", "grub2-efi-x64", "grub2-tools", "grubby",
		"basalt-release", "basalt-release-server", "basalt-logos", "basalt-logos-httpd", "basalt-grub2-theme",
		"plymouth-theme-basalt", "basalt-snapshots", "basalt-security",
		// Per-session default-deny egress and the audit ledger (docs/network.md, docs/ledger.md).
		"basalt-resolver", "basalt-resolver-selinux", "basalt-ledger", "basalt-ledger-selinux",
		// The shell prompt (interactive bash; server and desktop editions).
		"basalt-prompt",
		"btrfs-progs", "cryptsetup", "tpm2-tss", "tpm2-tools", "mokutil", "keyutils", "efibootmgr", "audit",
		"policycoreutils-python-utils", "setools-console", "compsize", "glibc-langpack-en", "snapper",
		// dnf5-plugins: `dnf config-manager` and the other dnf commands
		// the documentation (obpkg.org) uses.
		"libdnf5-plugin-actions", "dnf5-plugins", "selinux-policy-targeted",
		// Swap in compressed memory only, nothing swapped to a disk (ADR 0002).
		"zram-generator", "zram-generator-defaults"}
	if p := r.Plan.Assistant; p == nil || *p {
		install = append(install, "basalt-assistant", "basalt-assistant-selinux")
	}
	if r.UsesTang() {
		install = append(install, "clevis", "clevis-luks", "clevis-dracut")
	}
	if r.Plan.Edition == "desktop" {
		install = append(install, DesktopPackages...)
	}
	for _, p := range r.Plan.Packages.Extra {
		if !slices.Contains(install, p) {
			install = append(install, p)
		}
	}
	exclude = []string{"fedora-release", "fedora-release-common", "fedora-release-identity-basic",
		"fedora-logos", "fedora-logos-httpd", "generic-release", "generic-logos"}
	if r.Profile == "minimal" {
		exclude = append(exclude, "linux-firmware", "linux-firmware-whence", "*-firmware", "microcode_ctl", "fwupd*", "flashrom")
	}
	return install, exclude
}

// InstalledRepoURLs returns the base URLs of the Basalt and basalt-tools
// repositories the installed system uses.
func InstalledRepoURLs(r Resolved) (basalt, tools string) {
	g := &gen{r: r}
	basalt = g.installedRepoURL()
	return basalt, g.installedToolsURL(basalt)
}

// Generate returns the steps that carry out the resolved plan, in order.
func Generate(r Resolved) ([]Step, error) {
	g := &gen{r: r, T: r.Root}
	if err := g.all(); err != nil {
		return nil, err
	}
	for i := range g.out {
		if g.out[i].Weight == 0 {
			g.out[i].Weight = 1
		}
	}
	return g.out, nil
}

type gen struct {
	r     Resolved
	T     string // target root
	phase string
	out   []Step
	n     int
}

func (g *gen) add(s Step) *Step {
	g.n++
	if s.ID == "" {
		s.ID = fmt.Sprintf("%s-%02d", g.phase, g.n)
	}
	s.Phase = g.phase
	g.out = append(g.out, s)
	return &g.out[len(g.out)-1]
}

func (g *gen) run(title string, argv ...string) *Step { return g.add(Step{Title: title, Argv: argv}) }

// chroot runs a command inside the installed system.
func (g *gen) chroot(title string, argv ...string) *Step {
	return g.add(Step{Title: title, Argv: append([]string{"chroot", g.T}, argv...)})
}

func (g *gen) write(title, path string, mode uint32, content string) *Step {
	return g.add(Step{Title: title, Write: &FileWrite{Path: path, Mode: mode, Content: content, Shown: content}})
}

func (g *gen) t(path string) string { return filepath.Join(g.T, path) }

func (g *gen) all() error {
	r, p := g.r, g.r.Plan
	dev := r.BtrfsDev()
	opts := "compress=zstd:1"

	// --- preflight -------------------------------------------------------------------------
	g.phase = "preflight"
	g.run("Wait for device events to settle", "udevadm", "settle", "--timeout=30")
	g.run("Create the installer's work directory (memory only)", "install", "-d", "-m", "0700", r.Work)
	g.run("Create the target mount point", "install", "-d", "-m", "0755", g.T)
	if e := r.Existing; e != nil && e.LUKS {
		g.add(Step{Title: "Check the passphrase of the existing /home " + e.Device + " (nothing is written to it)",
			Argv:  []string{"cryptsetup", "open", "--test-passphrase", "--key-file=-", e.Device},
			Stdin: p.Home.Existing.Passphrase, StdinShown: "<passphrase>"})
	}

	// --- disk ------------------------------------------------------------------------------
	g.phase = "disk"
	disk := r.Disk.Path
	if p.Target.Wipe {
		g.run("Erase the partition table of "+disk, "sgdisk", "--zap-all", disk).Note =
			"from here on the previous contents of " + disk + " are gone"
		g.run("Erase remaining file system signatures on "+disk, "wipefs", "--all", "--force", disk)
		argv := []string{"sgdisk", "--clear"}
		var roles []string
		for _, np := range r.NewParts {
			argv = append(argv, fmt.Sprintf("--new=%d:0:%s", np.Number, np.Size), fmt.Sprintf("--typecode=%d:%s", np.Number, np.TypeCode),
				fmt.Sprintf("--change-name=%d:%s", np.Number, np.Name))
			roles = append(roles, partRole(np.Role))
		}
		g.run("Partition "+disk+": "+strings.Join(roles, ", "), append(argv, disk)...)
	} else {
		// Other systems keep their partitions: only new ones are added, in
		// free space, by number and exact sectors.
		var kept []string
		for _, k := range r.Kept {
			d := fmt.Sprintf("%d %s", k.Number, probe.HumanSize(k.Bytes))
			if n := probe.TypeName(k.Type); n != "" {
				d += " " + n
			} else if k.FSType != "" {
				d += " " + k.FSType
			}
			kept = append(kept, d)
		}
		argv := []string{"sgdisk"}
		undo := []string{"sgdisk"}
		for _, np := range r.NewParts {
			argv = append(argv, fmt.Sprintf("--new=%d:%d:%d", np.Number, np.Start, np.End), fmt.Sprintf("--typecode=%d:%s", np.Number, np.TypeCode),
				fmt.Sprintf("--change-name=%d:%s", np.Number, np.Name))
			undo = append(undo, fmt.Sprintf("--delete=%d", np.Number))
		}
		s := g.run("Add the Basalt OS partitions to the free space of "+disk, append(argv, disk)...)
		s.ID, s.Undo = "new-partitions", append(undo, disk)
		var made []string
		for _, np := range r.NewParts {
			made = append(made, fmt.Sprintf("%s %s (%s, sectors %d-%d)", np.Dev, probe.HumanSize(np.Bytes), partRole(np.Role), np.Start, np.End))
		}
		s.Note = "kept as they are: partitions " + strings.Join(kept, "; ") + ". New: " + strings.Join(made, "; ")
		if r.ESPShared {
			s.Note += ". The EFI system partition " + r.ESPDev + " is shared, not formatted"
		}
	}
	g.run("Wait for the new partitions", "udevadm", "settle", "--timeout=30")
	var newDevs []string
	for _, np := range r.NewParts {
		newDevs = append(newDevs, np.Dev)
	}
	g.run("Erase old signatures at the start of each new partition", append([]string{"wipefs", "--all", "--force"}, newDevs...)...)
	if !r.ESPShared {
		g.run("Format the EFI system partition (FAT32)", "mkfs.vfat", "-F", "32", "-n", "EFI", "-i", r.ESPVolID, r.ESPDev)
	}
	g.run("Format /boot (ext4, readable by GRUB)", "mkfs.ext4", "-q", "-F", "-L", "boot", "-U", r.BootUUID, r.BootDev)

	// --- encryption ------------------------------------------------------------------------
	if p.Encrypted() {
		g.phase = "encryption"
		g.add(Step{Title: "Create a temporary key for the new LUKS2 volume",
			Write: &FileWrite{Path: r.KeyFile(), Mode: 0o600, RandomChars: 64},
			Undo:  []string{"rm", "-f", r.KeyFile()},
			Note:  "it lives in memory only and is removed once the unlock method and the recovery key are enrolled"}).ID = "luks-key"
		what, label := "the system partition", "basalt"
		if p.EncryptsHome() {
			what, label = "the /home partition", "basalt-home"
		}
		g.run("Encrypt "+what+" (LUKS2)", "cryptsetup", "luksFormat", "--batch-mode", "--type", "luks2",
			"--uuid", r.LUKSUUID, "--label", label, "--key-file", r.KeyFile(), r.LUKSDev()).Weight = 3
		s := g.run("Open the encrypted volume", "cryptsetup", "open", "--key-file", r.KeyFile(), r.LUKSDev(), r.CryptName())
		s.ID, s.Undo = "luks-open", []string{"cryptsetup", "close", r.CryptName()}
	}

	// --- file systems ----------------------------------------------------------------------
	g.phase = "filesystems"
	g.run("Create the btrfs file system", "mkfs.btrfs", "--force", "--label", "basalt", "--uuid", r.BtrfsUUID, dev)
	g.run("Create a mount point for the btrfs top level", "install", "-d", "-m", "0700", r.TopMount())
	s := g.run("Mount the btrfs top level", "mount", "-o", opts, dev, r.TopMount())
	s.ID, s.Undo = "top-mount", []string{"umount", r.TopMount()}
	subvols := g.subvolumes()
	for _, sv := range subvols {
		g.run("Create subvolume "+sv.Name+" ("+sv.Mountpoint+")", "btrfs", "subvolume", "create", filepath.Join(r.TopMount(), sv.Name))
	}
	g.run("Unmount the btrfs top level", "umount", r.TopMount()).Release = "top-mount"
	s = g.run("Mount the system subvolume on "+g.T, "mount", "-o", "subvol=root,"+opts, dev, g.T)
	s.ID, s.Undo = "root-mount", []string{"umount", "--recursive", g.T}
	s.Note = "compress=zstd:1 is active while the system is written, so installed files are compressed"
	for _, sv := range subvols {
		if sv.Name == "root" {
			continue
		}
		g.run("Create "+sv.Mountpoint, "mkdir", "-p", g.t(sv.Mountpoint))
		g.run("Mount subvolume "+sv.Name+" on "+sv.Mountpoint, "mount", "-o", "subvol="+sv.Name+","+opts, dev, g.t(sv.Mountpoint))
	}
	switch {
	case p.EncryptsHome():
		g.run("Create the /home file system (btrfs on LUKS2)", "mkfs.btrfs", "--force", "--label", "basalt-home", "--uuid", r.HomeBtrfsUUID, "/dev/mapper/"+r.CryptName())
		g.run("Create /home", "mkdir", "-p", g.t("/home"))
		g.run("Mount /home", "mount", "-o", opts, "/dev/mapper/"+r.CryptName(), g.t("/home"))
	case r.Existing != nil:
		e := r.Existing
		g.run("Create /home", "mkdir", "-p", g.t("/home"))
		src := e.Device
		if e.LUKS {
			s := g.add(Step{Title: "Open the existing /home " + e.Device, Argv: []string{"cryptsetup", "open", "--key-file=-", e.Device, e.Mapper()},
				Stdin: p.Home.Existing.Passphrase, StdinShown: "<passphrase>"})
			s.ID, s.Undo = "home-open", []string{"cryptsetup", "close", e.Mapper()}
			src = "/dev/mapper/" + e.Mapper()
		}
		g.run("Mount the existing /home (never formatted)", "mount", src, g.t("/home")).Note =
			"its files are not changed: only the new user's directory is created and labeled"
	}
	g.run("Create /boot", "mkdir", "-p", g.t("/boot"))
	g.run("Mount /boot", "mount", r.BootDev, g.t("/boot"))
	g.run("Create /boot/efi", "mkdir", "-p", g.t("/boot/efi"))
	g.run("Mount the EFI system partition", "mount", "-o", "umask=0077,shortname=winnt", r.ESPDev, g.t("/boot/efi"))
	if g.hasSubvol("var_tmp") {
		g.run("Set the sticky, world-writable mode of /var/tmp", "chmod", "1777", g.t("/var/tmp"))
	}

	// --- configuration read by the package scripts ------------------------------------------
	g.phase = "configure"
	g.run("Create configuration directories", "mkdir", "-p", g.t("/etc/kernel"), g.t("/etc/default"), g.t("/etc/dnf/vars"))
	g.write("Write /etc/fstab", g.t("/etc/fstab"), 0o644, g.fstab(subvols)).Note =
		"basalt-snapshots-setup later drops subvol=root from / so the default subvolume (what snapper rollback switches) is booted"
	if ct := g.crypttab(); ct != "" {
		g.write("Write /etc/crypttab", g.t("/etc/crypttab"), 0o600, ct)
	}
	g.write("Write the kernel command line for new kernels", g.t("/etc/kernel/cmdline"), 0o644, g.kernelCmdline(true)+"\n").Note =
		"kernel-install reads it when the kernel package is installed; without it, it would copy the live system's command line"
	g.write("Write the GRUB settings", g.t("/etc/default/grub"), 0o644, g.defaultGrub())
	g.write("Write the machine ID", g.t("/etc/machine-id"), 0o444, r.MachineID+"\n").Note =
		"boot loader entries are named after it"
	g.write("Set the host name", g.t("/etc/hostname"), 0o644, p.Hostname+"\n")
	g.write("Set the locale", g.t("/etc/locale.conf"), 0o644, "LANG="+p.Locale+"\n")
	g.write("Set the console keymap", g.t("/etc/vconsole.conf"), 0o644, "KEYMAP="+p.Keymap+"\n")
	g.add(Step{Title: "Set the time zone", Write: &FileWrite{Path: g.t("/etc/localtime"), Symlink: "../usr/share/zoneinfo/" + p.Timezone}})
	if u := g.installedRepoURL(); u != "" {
		g.write("Point the Basalt repository at its URL", g.t("/etc/dnf/vars/basalt_repo_url"), 0o644, u+"\n")
		g.write("Point the basalt-tools repository at its URL", g.t("/etc/dnf/vars/basalt_tools_url"), 0o644, g.installedToolsURL(u)+"\n")
		if g.testing() {
			g.write("Point the basalt-testing repository at its URL", g.t("/etc/dnf/vars/basalt_testing_url"), 0o644, g.installedTestingURL(u)+"\n")
		}
	}
	g.run("Create the temporary repository directory", "install", "-d", "-m", "0700", r.ReposDir())
	g.write("Write the install-time repositories (Fedora, Basalt)", filepath.Join(r.ReposDir(), "basalt-install.repo"), 0o600, g.installRepos())

	// --- packages --------------------------------------------------------------------------
	g.phase = "packages"
	g.run("Create /dev, /proc, /sys and /run in the target", "mkdir", "-p", g.t("/dev"), g.t("/proc"), g.t("/sys"), g.t("/run"))
	g.run("Bind the device tree into the target", "mount", "--rbind", "/dev", g.t("/dev"))
	g.run("Keep unmounts in the target from reaching the live /dev", "mount", "--make-rslave", g.t("/dev"))
	g.run("Mount /proc in the target", "mount", "-t", "proc", "proc", g.t("/proc"))
	g.run("Mount /sys in the target", "mount", "-t", "sysfs", "sysfs", g.t("/sys"))
	g.run("Mount a /run for package scripts", "mount", "-t", "tmpfs", "-o", "mode=0755", "tmpfs", g.t("/run"))
	install, exclude := Packages(r)
	argv := []string{"dnf", "--assumeyes", "--installroot=" + g.T, fmt.Sprintf("--releasever=%d", r.Release),
		"--use-host-config", "--no-plugins", "--setopt=reposdir=" + r.ReposDir(), "--setopt=install_weak_deps=True"}
	for _, e := range exclude {
		argv = append(argv, "--exclude="+e)
	}
	argv = append(argv, "install")
	argv = append(argv, install...)
	s = g.run(fmt.Sprintf("Install the %s profile (Fedora %d packages and the Basalt packages)", r.Profile, r.Release), argv...)
	s.Weight, s.Progress = 60, "dnf"
	s.Note = "packages and repository metadata are signature checked (gpgcheck, repo_gpgcheck for Basalt)"

	// --- system configuration -----------------------------------------------------------------
	g.phase = "system"
	g.write("SELinux enforcing, targeted policy", g.t("/etc/selinux/config"), 0o644,
		"# Written by the Basalt OS installer. Basalt OS runs SELinux enforcing.\nSELINUX=enforcing\nSELINUXTYPE=targeted\n")
	g.accounts()
	if p.SSH.PasswordAuth {
		g.write("Allow password logins over SSH (plan: ssh.password_auth)", g.t("/etc/ssh/sshd_config.d/05-basalt-installer-passwords.conf"), 0o600,
			"# Written by the Basalt OS installer (ssh.password_auth: true). Read before\n# 10-basalt-hardening.conf, so these values win.\nPasswordAuthentication yes\nAuthenticationMethods any\n")
	}
	if p.Network.Mode == "static" {
		name := "basalt-" + p.Network.Interface
		g.write("Static network connection "+name, g.t("/etc/NetworkManager/system-connections/"+name+".nmconnection"), 0o600, g.nmConnection(name))
	}
	if err := g.repos(); err != nil {
		return err
	}
	g.chroot("Use the Basalt boot splash", "plymouth-set-default-theme", SplashTheme(p.Locale)).Optional = true
	svcs := append([]string{}, baseServices...)
	if a := p.Assistant; a == nil || *a {
		svcs = append(svcs, assistantServices...)
	}
	target := "multi-user.target"
	if p.Edition == "desktop" {
		target = "graphical.target"
	}
	g.chroot("Boot to "+target+" (the "+p.Edition+" edition)", "systemctl", "set-default", target).Note =
		"systemd's own default is graphical.target; Anaconda sets multi-user.target for a server (skipx), and so does this"
	g.chroot("Enable the Basalt services", append([]string{"systemctl", "enable"}, svcs...)...).Note =
		"the assistant's daemon only diagnoses and proposes; applying a change always needs a confirmation"

	// --- boot loader -------------------------------------------------------------------------
	g.phase = "bootloader"
	g.run("Create the boot loader directory on the EFI system partition", "mkdir", "-p", g.t("/boot/efi/EFI/fedora"))
	g.write("Point the signed GRUB on the ESP at /boot", g.t("/boot/efi/EFI/fedora/grub.cfg"), 0o600, g.espGrubStub()).Note =
		"the boot chain is Fedora's signed shim, GRUB and kernel, so Secure Boot stays on"
	g.chroot("Show the boot menu", "grub2-editenv", "-", "unset", "menu_auto_hide").Optional = true
	g.chroot("Generate the GRUB configuration", "grub2-mkconfig", "-o", "/boot/grub2/grub.cfg")
	g.run("Add a firmware boot entry", "efibootmgr", "--create", "--disk", disk, "--part", fmt.Sprint(r.ESPNumber),
		"--label", "Basalt OS", "--loader", `\EFI\fedora\shimx64.efi`).Optional = true
	g.out[len(g.out)-1].Note = "the firmware's fallback path (\\EFI\\BOOT\\BOOTX64.EFI, also Fedora's shim) boots the disk without it"

	// --- disk unlock -------------------------------------------------------------------------
	if p.Encrypted() {
		g.phase = "unlock"
		g.unlock()
	}
	if e := r.Existing; e != nil && e.LUKS && p.Home.Existing.TPM2 && r.TPM2 {
		g.phase = "unlock"
		g.add(Step{Title: "Let this machine's TPM open the existing /home too (PCR 7; the passphrase keeps working)",
			Argv: []string{"systemd-cryptenroll", "--tpm2-device=auto", "--tpm2-pcrs=7", e.Device},
			Env:  []EnvVar{{Name: "PASSWORD", Value: p.Home.Existing.Passphrase, Shown: "<passphrase>"}}})
	}

	// --- initramfs and snapshots ----------------------------------------------------------------
	g.phase = "initramfs"
	g.chroot("Build the initramfs with the unlock support and the crypttab", "dracut", "--force", "--regenerate-all").Weight = 6
	g.phase = "snapshots"
	g.chroot("Set up snapper, the default subvolume and the snapshot boot menu", "basalt-snapshots-setup", "--no-initial-snapshot").Note =
		"no snapshot inside the installer: the first one is taken on the first boot (basalt-initial-snapshot.service)"
	g.out[len(g.out)-1].Weight = 2

	// --- finish ------------------------------------------------------------------------------
	g.phase = "finish"
	g.chroot("Remove the package cache", "dnf", "clean", "all").Optional = true
	g.run("Create the install record directory", "mkdir", "-p", g.t("/var/log/basalt-installer"))
	g.write("Record the plan (without secrets)", g.t("/var/log/basalt-installer/plan.json"), 0o600, p.Redacted().JSON())
	g.write("Record the install summary", g.t("/var/log/basalt-installer/summary.log"), 0o644, g.summary()+"\n")
	// Every subvolume and /boot is named as a starting point: setfiles does
	// not descend into another file system (each btrfs subvolume counts as
	// one) unless the kernel marks it "seclabel", which the live system,
	// running without SELinux, does not.
	relabel := []string{"setfiles", "-F", "-e", "/proc", "-e", "/sys", "-e", "/dev", "-e", "/run",
		"-e", "/boot/efi", "/etc/selinux/targeted/contexts/files/file_contexts", "/", "/boot", "/.snapshots"}
	for _, sv := range subvols {
		if sv.Name != "root" {
			relabel = append(relabel, sv.Mountpoint)
		}
	}
	if p.EncryptsHome() {
		relabel = append(relabel, "/home")
	}
	s = g.chroot("Label every file for SELinux (target policy)", relabel...)
	s.Weight = 6
	s.Note = "every subvolume, /.snapshots and /boot by name: setfiles does not cross into another file system on its own; " +
		"append-only and immutable files (the assistant's audit log) lose that attribute for the relabel and get it back"
	s.KeepAttrsUnder = g.T
	if u := p.Accounts.User; u != nil || r.Existing != nil {
		// The relabel above runs without the kernel's SELinux interface in
		// the target (it labels types only the installed policy knows), and
		// libselinux then leaves out the home directory contexts: home
		// directories would be home_root_t. restorecon with the interface
		// labels them as the installed system does (user_home_dir_t).
		s := g.run("Show the kernel's SELinux interface to the target", "mount", "-t", "selinuxfs", "selinuxfs", g.t("/sys/fs/selinux"))
		s.ID, s.Undo = "selinuxfs", []string{"umount", g.t("/sys/fs/selinux")}
		if r.Existing != nil {
			g.chroot("Label the /home mount point (the existing files keep their labels)", "restorecon", "-F", "/home")
		}
		if u != nil {
			home := u.HomeDir
			if home == "" {
				home = "/home/" + u.Name
			}
			s := g.chroot("Label "+home+" for SELinux", "restorecon", "-R", "-F", home)
			if r.Existing != nil {
				s.Note = "only the new user's directory: nothing else on the existing /home is changed"
			}
		}
		g.run("Hide the SELinux interface from the target again", "umount", g.t("/sys/fs/selinux")).Release = "selinuxfs"
	}
	g.add(Step{Title: "Copy the install log into the installed system",
		Write: &FileWrite{Path: g.t("/var/log/basalt-installer/install.jsonl"), Mode: 0o600, FromLog: true}})
	g.chroot("Label the install record", "restorecon", "-R", "/var/log/basalt-installer")
	g.run("Unmount the installed system", "umount", "--recursive", g.T).Release = "root-mount"
	g.run("Flush writes to disk", "sync")
	if p.Encrypted() {
		g.run("Wait for device events to settle", "udevadm", "settle", "--timeout=30")
		g.run("Show what still uses the file system (diagnostics)", "lsblk", "--output", "NAME,TYPE,MOUNTPOINTS", r.LUKSDev()).Optional = true
		s := g.run("Close the encrypted volume", "cryptsetup", "close", r.CryptName())
		s.Release, s.Retries = "luks-open", 10
	}
	if e := r.Existing; e != nil && e.LUKS {
		s := g.run("Close the existing /home", "cryptsetup", "close", e.Mapper())
		s.Release, s.Retries = "home-open", 10
	}
	return nil
}

func (g *gen) subvolumes() []plan.Subvolume {
	var out []plan.Subvolume
	for _, s := range plan.Subvolumes() {
		if g.hasSubvol(s.Name) {
			out = append(out, s)
		}
	}
	// Mount parents before children.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out
}

func (g *gen) hasSubvol(name string) bool {
	for _, s := range g.r.Plan.Layout.Subvolumes {
		if s == name {
			return true
		}
	}
	return false
}

func (g *gen) installedRepoURL() string {
	b := g.r.Plan.Repos.Basalt
	if b.InstalledURL != "" {
		return strings.TrimRight(b.InstalledURL, "/")
	}
	if strings.HasPrefix(b.URL, "http") {
		return strings.TrimRight(b.URL, "/")
	}
	// Installed from the media: the published repositories.
	return plan.DefaultRepoURL
}

// installedToolsURL is the basalt-tools base URL that goes with the
// installed Basalt repository u.
func (g *gen) installedToolsURL(u string) string {
	if t := g.r.Plan.Repos.Basalt.InstalledToolsURL; t != "" {
		return strings.TrimRight(t, "/")
	}
	if u == plan.DefaultRepoURL {
		return plan.DefaultToolsURL
	}
	return u + "/tools"
}

// testing reports whether the plan turns basalt-testing on.
func (g *gen) testing() bool {
	t := g.r.Plan.Repos.Testing
	return t != nil && *t
}

// installedTestingURL is the basalt-testing base URL that goes with the
// installed Basalt repository u.
func (g *gen) installedTestingURL(u string) string {
	if u == plan.DefaultRepoURL {
		return plan.DefaultTestingURL
	}
	return u + "/testing"
}

// testingInstallURL is where the installer reads basalt-testing: the
// media's basalt/testing tree when the Basalt repository is the media and
// carries one, else the installed system's basalt-testing URL.
func (g *gen) testingInstallURL() string {
	if g.r.Plan.Repos.Basalt.URL == "media" {
		dir := filepath.Join(g.r.MediaDir, "basalt", "testing")
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return "file://" + dir
		}
	}
	return g.installedTestingURL(g.installedRepoURL())
}

func (g *gen) basaltKey() string {
	if k := g.r.Plan.Repos.Basalt.GPGKey; k != "" {
		return k
	}
	return filepath.Join(g.r.MediaDir, "basalt", "RPM-GPG-KEY-basalt")
}

func (g *gen) basaltInstallURL() string {
	u := g.r.Plan.Repos.Basalt.URL
	if u == "media" {
		u = "file://" + filepath.Join(g.r.MediaDir, "basalt", "repo")
	}
	return strings.TrimRight(u, "/")
}

func (g *gen) accounts() {
	p := g.r.Plan
	root := p.Accounts.Root
	switch {
	case root.Password != "":
		g.add(Step{Title: "Set the root password", Argv: []string{"chroot", g.T, "chpasswd"},
			Stdin: "root:" + root.Password + "\n", StdinShown: "root:<password>"})
	case root.PasswordHash != "":
		g.add(Step{Title: "Set the root password (hash)", Argv: []string{"chroot", g.T, "chpasswd", "--encrypted"},
			Stdin: "root:" + root.PasswordHash + "\n", StdinShown: "root:<password hash>"})
	default:
		g.chroot("Lock the root password (root logs in with an SSH key or not at all)", "usermod", "--lock", "root")
	}
	if len(root.SSHKeys) > 0 {
		g.run("Create /root/.ssh", "install", "-d", "-m", "0700", g.t("/root/.ssh"))
		g.write("Authorize SSH keys for root", g.t("/root/.ssh/authorized_keys"), 0o600, strings.Join(root.SSHKeys, "\n")+"\n")
	}
	u := p.Accounts.User
	if u == nil {
		return
	}
	home := u.HomeDir
	if home == "" {
		home = "/home/" + u.Name
	}
	if u.GID != 0 {
		g.chroot(fmt.Sprintf("Create the group %s (gid %d, as on the existing /home)", u.Name, u.GID), "groupadd", "--gid", fmt.Sprint(u.GID), u.Name)
	}
	argv := []string{"useradd", "--create-home"}
	if u.HomeDir != "" {
		argv = append(argv, "--home-dir", u.HomeDir)
	}
	if u.UID != 0 {
		argv = append(argv, "--uid", fmt.Sprint(u.UID))
	}
	if u.GID != 0 {
		argv = append(argv, "--gid", fmt.Sprint(u.GID))
	}
	if p.IsAdminUser() {
		argv = append(argv, "--groups", "wheel")
	}
	if u.FullName != "" {
		argv = append(argv, "--comment", u.FullName)
	}
	argv = append(argv, u.Name)
	title := "Create the user " + u.Name
	if p.IsAdminUser() {
		title += " (administrator, group wheel)"
	}
	g.chroot(title, argv...)
	switch {
	case u.Password != "":
		g.add(Step{Title: "Set the password of " + u.Name, Argv: []string{"chroot", g.T, "chpasswd"},
			Stdin: u.Name + ":" + u.Password + "\n", StdinShown: u.Name + ":<password>"})
	case u.PasswordHash != "":
		g.add(Step{Title: "Set the password of " + u.Name + " (hash)", Argv: []string{"chroot", g.T, "chpasswd", "--encrypted"},
			Stdin: u.Name + ":" + u.PasswordHash + "\n", StdinShown: u.Name + ":<password hash>"})
	}
	if len(u.SSHKeys) > 0 {
		g.chroot("Create "+home+"/.ssh", "install", "-d", "-m", "0700", "-o", u.Name, "-g", u.Name, home+"/.ssh")
		g.write("Authorize SSH keys for "+u.Name, g.t(home+"/.ssh/authorized_keys"), 0o600, strings.Join(u.SSHKeys, "\n")+"\n")
		g.chroot("Give "+u.Name+" its authorized_keys", "chown", u.Name+":"+u.Name, home+"/.ssh/authorized_keys")
	}
}

func (g *gen) repos() error {
	p := g.r.Plan
	// basalt-release ships [basalt-tools] (enabled, ADR 0005); its URL is
	// the basalt_tools_url variable written above. A plan without tools
	// turns it off with a dnf repository override, as
	// `dnf config-manager setopt basalt-tools.enabled=0` would.
	var override []string
	if p.Repos.Tools != nil && !*p.Repos.Tools {
		override = append(override, `# The plan leaves basalt-tools off.
# Turn it on with: dnf config-manager setopt basalt-tools.enabled=1
[basalt-tools]
enabled=0
`)
	}
	if g.testing() {
		override = append(override, `# The plan turns basalt-testing on (pre-release packages).
# Turn it off with: dnf config-manager setopt basalt-testing.enabled=0
[basalt-testing]
enabled=1
`)
	}
	if len(override) > 0 {
		title := "Turn the basalt-tools repository off (ADR 0005)"
		switch {
		case len(override) == 2:
			title = "Turn basalt-tools off and basalt-testing on"
		case g.testing():
			title = "Turn the basalt-testing repository on (pre-release packages)"
		}
		g.write(title, g.t("/etc/dnf/repos.override.d/80-basalt-installer.repo"), 0o644,
			"# Written by the Basalt OS installer.\n"+strings.Join(override, "\n"))
	}
	if t := p.Repos.ThirdParty.TUITools; t == nil || *t {
		fpr, err := pgpkey.Fingerprint(TUIToolsKey)
		if err != nil {
			return fmt.Errorf("tui-tools key: %w", err)
		}
		if fpr != TUIToolsFingerprint {
			return fmt.Errorf("tui-tools key fingerprint %s does not match the pinned %s", fpr, TUIToolsFingerprint)
		}
		g.write("Install the tui-tools signing key (fingerprint "+TUIToolsFingerprint+", pinned)",
			g.t("/etc/pki/rpm-gpg/RPM-GPG-KEY-tui-tools"), 0o644, string(TUIToolsKey))
		g.write("Configure the tui-tools repository (third party, ADR 0005)", g.t("/etc/yum.repos.d/tui-tools.repo"), 0o644,
			`# tui-tools: terminal tools with a command preview before every change.
# Official third-party repository: packages come straight from upstream.
# Signing key fingerprint `+TUIToolsFingerprint+` (pinned by the installer).
# Written by the Basalt OS installer; it moves to basalt-third-party.
[tui-tools]
name=tui-tools (third party)
baseurl=`+TUIToolsBaseURL+`
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-tui-tools
skip_if_unavailable=True
metadata_expire=6h
`)
		g.chroot("Trust the tui-tools key in the package database", "rpm", "--import", "/etc/pki/rpm-gpg/RPM-GPG-KEY-tui-tools")
	}
	return nil
}

func (g *gen) unlock() {
	r, p := g.r, g.r.Plan
	key, dev := r.KeyFile(), r.LUKSDev()
	tangCfg := func() string {
		if p.Encryption.Tang.Thumbprint != "" {
			return fmt.Sprintf(`{"url":%q,"thp":%q}`, p.Encryption.Tang.URL, p.Encryption.Tang.Thumbprint)
		}
		return fmt.Sprintf(`{"url":%q}`, p.Encryption.Tang.URL)
	}
	trust := func(argv []string) []string {
		if p.Encryption.Tang.Thumbprint == "" {
			return append(argv, "-y")
		}
		return argv
	}
	switch p.Encryption.Unlock {
	case "tpm2":
		if r.EnrollTPM {
			g.run("Seal a key in the TPM, bound to the Secure Boot state (PCR 7)",
				"systemd-cryptenroll", "--unlock-key-file="+key, "--tpm2-device=auto", "--tpm2-pcrs=7", dev).Note =
				"the disk unlocks by itself while the Secure Boot configuration is unchanged"
		}
	case "tang":
		g.run("Bind the volume to the Tang server "+p.Encryption.Tang.URL,
			append(trust([]string{"clevis", "luks", "bind"}), "-d", dev, "-k", key, "tang", tangCfg())...)
	case "tpm2+tang":
		cfg := fmt.Sprintf(`{"t":2,"pins":{"tang":[%s],"tpm2":{"pcr_bank":"sha256","pcr_ids":"7"}}}`, tangCfg())
		g.run("Bind the volume to the TPM (PCR 7) and the Tang server, both required",
			append(trust([]string{"clevis", "luks", "bind"}), "-d", dev, "-k", key, "sss", cfg)...)
	}
	if p.Encryption.Passphrase != "" {
		g.add(Step{Title: "Add the boot passphrase", Argv: []string{"systemd-cryptenroll", "--unlock-key-file=" + key, "--password", dev},
			Env: []EnvVar{{Name: "NEWPASSWORD", Value: p.Encryption.Passphrase, Shown: "<passphrase>"}}})
	}
	s := g.run("Generate and enroll the recovery key", "systemd-cryptenroll", "--unlock-key-file="+key, "--recovery-key", dev)
	s.Capture = "recovery_key"
	s.Note = "shown once on the screen; it opens the disk when the TPM refuses (Secure Boot changed, disk moved)"
	g.run("Check that key slot 0 holds the temporary key", "cryptsetup", "open", "--test-passphrase",
		"--disable-external-tokens", "--key-slot", "0", "--key-file", key, dev).Note =
		"token plugins off, so the TPM2 token cannot answer instead (milestone 1 lesson)"
	g.run("Remove the temporary key from the volume", "systemd-cryptenroll", "--unlock-key-file="+key, "--wipe-slot=0", dev)
	g.run("Delete the temporary key", "rm", "-f", key).Release = "luks-key"
	if p.Encryption.StoreRecoveryKey {
		g.add(Step{Title: "Leave the recovery key in /root (plan: store_recovery_key)",
			Write: &FileWrite{Path: g.t("/root/basalt-recovery-key.txt"), Mode: 0o400, FromSecret: "recovery_key"}})
		g.run("Create /etc/motd.d", "mkdir", "-p", g.t("/etc/motd.d"))
		g.write("Remind the administrator to move the recovery key", g.t("/etc/motd.d/basalt-recovery-key"), 0o644,
			"Basalt OS: the disk recovery key is in /root/basalt-recovery-key.txt.\n"+
				"Store it somewhere safe, off this machine, then delete the file and this\n"+
				"notice (rm /root/basalt-recovery-key.txt /etc/motd.d/basalt-recovery-key).\n")
	}
}

func (g *gen) fstab(subvols []plan.Subvolume) string {
	r := g.r
	var b strings.Builder
	b.WriteString("# /etc/fstab, written by the Basalt OS installer.\n")
	b.WriteString("# btrfs on " + map[bool]string{true: "LUKS2", false: "the system partition"}[r.Plan.Encrypted()] + ", label basalt; data subvolumes are not rolled back.\n")
	for _, sv := range subvols {
		if sv.Name == "root" {
			fmt.Fprintf(&b, "UUID=%s / btrfs subvol=root,compress=zstd:1 0 0\n", r.BtrfsUUID)
			fmt.Fprintf(&b, "UUID=%s /boot ext4 defaults 1 2\n", r.BootUUID)
			fmt.Fprintf(&b, "UUID=%s /boot/efi vfat umask=0077,shortname=winnt 0 2\n", r.ESPUUID())
			continue
		}
		fmt.Fprintf(&b, "UUID=%s %s btrfs subvol=%s,compress=zstd:1 0 0\n", r.BtrfsUUID, sv.Mountpoint, sv.Name)
	}
	switch {
	case r.Plan.EncryptsHome():
		fmt.Fprintf(&b, "UUID=%s /home btrfs compress=zstd:1 0 0\n", r.HomeBtrfsUUID)
	case r.Existing != nil && r.Existing.LUKS:
		fmt.Fprintf(&b, "/dev/mapper/%s /home auto defaults 0 2\n", r.Existing.Mapper())
	case r.Existing != nil:
		fmt.Fprintf(&b, "UUID=%s /home auto defaults 0 2\n", r.Existing.UUID)
	}
	return b.String()
}

func (g *gen) crypttab() string {
	var b strings.Builder
	if g.r.Plan.Encrypted() {
		opts := "discard"
		if g.r.EnrollTPM && g.r.Plan.Encryption.Unlock == "tpm2" {
			// No headless=true: when the TPM refuses, the boot asks for the
			// recovery key on the console and the serial port.
			opts += ",tpm2-device=auto"
		}
		fmt.Fprintf(&b, "%s UUID=%s none %s\n", g.r.CryptName(), g.r.LUKSUUID, opts)
	}
	if e := g.r.Existing; e != nil && e.LUKS {
		// The existing /home asks for its passphrase at boot (the splash or
		// the console), unless this machine's TPM was enrolled too.
		opts := "discard"
		if g.r.Plan.Home.Existing.TPM2 && g.r.TPM2 {
			opts += ",tpm2-device=auto"
		}
		fmt.Fprintf(&b, "%s UUID=%s none %s\n", e.Mapper(), e.UUID, opts)
	}
	return b.String()
}

// kernelCmdline is the command line of the installed system. withRoot adds
// root= and rootflags= (for /etc/kernel/cmdline; GRUB adds them itself).
func (g *gen) kernelCmdline(withRoot bool) string {
	r, p := g.r, g.r.Plan
	var args []string
	if withRoot {
		args = append(args, "root=UUID="+r.BtrfsUUID, "ro", "rootflags=subvol=root")
	}
	if p.EncryptsSystem() {
		args = append(args, "rd.luks.uuid="+r.CryptName())
	}
	args = append(args, "console=tty0", "console=ttyS0,115200n8")
	if p.Edition == "desktop" {
		// The graphical boot splash and its passphrase card, also with the
		// serial console on (plymouth otherwise falls back to text there).
		// Servers keep the plain text boot.
		args = append(args, "rhgb", "quiet", "plymouth.ignore-serial-consoles")
	}
	if p.Lockdown == nil || *p.Lockdown {
		args = append(args, "lockdown=integrity", "module.sig_enforce=1")
	}
	if r.UsesTang() {
		args = append(args, "rd.neednet=1")
	}
	return strings.Join(args, " ")
}

func (g *gen) defaultGrub() string {
	return `GRUB_TIMEOUT=5
GRUB_DISTRIBUTOR="$(sed 's, release .*$,,g' /etc/system-release)"
GRUB_DEFAULT=saved
GRUB_DISABLE_SUBMENU=true
GRUB_TIMEOUT_STYLE=menu
GRUB_TERMINAL_INPUT="serial console"
GRUB_TERMINAL_OUTPUT="serial console"
GRUB_SERIAL_COMMAND="serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1"
GRUB_CMDLINE_LINUX="` + g.kernelCmdline(false) + `"
GRUB_DISABLE_RECOVERY="true"
GRUB_ENABLE_BLSCFG=true
`
}

func (g *gen) espGrubStub() string {
	return fmt.Sprintf(`search --no-floppy --root-dev-only --fs-uuid --set=dev %s
set prefix=($dev)/grub2
export $prefix
configfile $prefix/grub.cfg
`, g.r.BootUUID)
}

func (g *gen) installRepos() string {
	p := g.r.Plan
	fedora := "metalink=https://mirrors.fedoraproject.org/metalink?repo=fedora-$releasever&arch=$basearch"
	if p.Repos.Fedora.BaseURL != "" {
		fedora = "baseurl=" + p.Repos.Fedora.BaseURL
	}
	updates := "metalink=https://mirrors.fedoraproject.org/metalink?repo=updates-released-f$releasever&arch=$basearch"
	if p.Repos.Fedora.UpdatesBaseURL != "" {
		updates = "baseurl=" + p.Repos.Fedora.UpdatesBaseURL
	}
	fkey := "file:///etc/pki/rpm-gpg/RPM-GPG-KEY-fedora-$releasever-$basearch"
	testing := ""
	if g.testing() {
		// Same key as the Basalt repository: one packages subkey signs both.
		testing = fmt.Sprintf(`
[basalt-install-testing]
name=Basalt OS testing $releasever - $basearch
baseurl=%s/$releasever/$basearch/
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://%s
`, strings.TrimRight(g.testingInstallURL(), "/"), g.basaltKey())
	}
	return fmt.Sprintf(`# Install-time repositories of the Basalt OS installer (live system only).
# Basalt repository key: %s
[basalt-install-fedora]
name=Fedora $releasever - $basearch
%s
gpgcheck=1
gpgkey=%s

[basalt-install-updates]
name=Fedora $releasever - $basearch - Updates
%s
gpgcheck=1
gpgkey=%s

[basalt-install-basalt]
name=Basalt OS $releasever - $basearch
baseurl=%s/$releasever/$basearch/
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://%s
`, describeBasaltKey(g.basaltKey()), fedora, fkey, updates, fkey, g.basaltInstallURL(), g.basaltKey()) + testing
}

// describeBasaltKey says whether a repository key file holds the OpenBasalt
// release key. dnf verifies every package and the metadata against the key
// either way; this tells a release build from a development one.
func describeBasaltKey(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "not readable yet (" + path + "); dnf checks signatures against it"
	}
	fpr, err := pgpkey.Fingerprint(data)
	switch {
	case err != nil:
		return "not an OpenPGP public key (" + err.Error() + "); dnf will refuse the repository"
	case fpr == OpenBasaltReleaseFingerprint:
		return "OpenBasalt release key " + fpr
	default:
		return fpr + ", not the OpenBasalt release key " + OpenBasaltReleaseFingerprint + " (development or third-party build)"
	}
}

func (g *gen) nmConnection(name string) string {
	n := g.r.Plan.Network
	var b strings.Builder
	fmt.Fprintf(&b, "[connection]\nid=%s\ntype=ethernet\ninterface-name=%s\nautoconnect=true\n\n[ipv4]\nmethod=manual\naddress1=%s", name, n.Interface, n.Address)
	if n.Gateway != "" {
		b.WriteString("," + n.Gateway)
	}
	b.WriteString("\n")
	if len(n.DNS) > 0 {
		b.WriteString("dns=" + strings.Join(n.DNS, ";") + ";\n")
	}
	b.WriteString("\n[ipv6]\nmethod=auto\n")
	return b.String()
}

// summary is the kickstart's %pre line, so tools and tests read both
// install paths the same way.
func (g *gen) summary() string {
	r, p := g.r, g.r.Plan
	enc := 0
	if p.Encrypted() {
		enc = 1
	}
	lock := 0
	if p.Lockdown == nil || *p.Lockdown {
		lock = 1
	}
	unlock := p.Encryption.Unlock
	return fmt.Sprintf("basalt: disk=%s encrypt=%d unlock=%s profile=%s (%s) lockdown=%d finish=%s installer=basalt-installer %s",
		strings.TrimPrefix(r.Disk.Path, "/dev/"), enc, unlock, r.Profile, r.ProfileReason, lock, p.Finish, r.Installer)
}

func partRole(role string) string {
	switch role {
	case "esp":
		return "EFI system"
	case "boot":
		return "/boot"
	case "home":
		return "/home"
	}
	return "system"
}
