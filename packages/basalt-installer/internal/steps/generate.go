package steps

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/pgpkey"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
)

// TUIToolsKey is the tui-tools repository signing key (ADR 0005: official
// third-party repository, key fingerprint pinned).
//
//go:embed RPM-GPG-KEY-tui-tools
var TUIToolsKey []byte

// TUIToolsFingerprint is the pinned fingerprint of TUIToolsKey.
const TUIToolsFingerprint = "767CFB337B01F32FFC073F3F389120B277E4FB44"

// TUIToolsBaseURL is the upstream tui-tools RPM repository.
const TUIToolsBaseURL = "https://pkgs.tui.tools/rpm/$basearch"

// Services enabled on the installed system (the same list as the kickstart).
var baseServices = []string{"sshd", "firewalld", "auditd", "basalt-snapshot-boot", "basalt-initial-snapshot",
	"basalt-grub-theme", "basalt-module-keys"}
var assistantServices = []string{"basalt-assistantd", "basalt-notify", "basalt-audit-rotate.timer"}

// Packages returns what dnf installs and excludes for the resolved plan
// (the kickstart's %packages, plus what Anaconda adds by itself: the kernel
// and the UEFI boot loader).
func Packages(r Resolved) (install, exclude []string) {
	install = []string{"@core", "kernel", "shim-x64", "grub2-efi-x64", "grub2-tools", "grubby",
		"basalt-release", "basalt-release-server", "basalt-logos", "basalt-logos-httpd", "basalt-grub2-theme",
		"plymouth-theme-basalt", "basalt-snapshots", "basalt-security",
		"btrfs-progs", "cryptsetup", "tpm2-tss", "tpm2-tools", "mokutil", "keyutils", "efibootmgr", "audit",
		"policycoreutils-python-utils", "setools-console", "compsize", "glibc-langpack-en", "snapper",
		"libdnf5-plugin-actions", "selinux-policy-targeted"}
	if p := r.Plan.Assistant; p == nil || *p {
		install = append(install, "basalt-assistant", "basalt-assistant-selinux")
	}
	if r.UsesTang() {
		install = append(install, "clevis", "clevis-luks", "clevis-dracut")
	}
	install = append(install, r.Plan.Packages.Extra...)
	exclude = []string{"fedora-release", "fedora-release-common", "fedora-release-identity-basic",
		"fedora-logos", "fedora-logos-httpd", "generic-release", "generic-logos"}
	if r.Profile == "minimal" {
		exclude = append(exclude, "linux-firmware", "linux-firmware-whence", "*-firmware", "microcode_ctl", "fwupd*", "flashrom")
	}
	return install, exclude
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

	// --- disk ------------------------------------------------------------------------------
	g.phase = "disk"
	disk := r.Disk.Path
	g.run("Erase the partition table of "+disk, "sgdisk", "--zap-all", disk).Note =
		"from here on the previous contents of " + disk + " are gone"
	g.run("Erase remaining file system signatures on "+disk, "wipefs", "--all", "--force", disk)
	sysType, sysName := "8300", "basalt"
	if p.Encrypted() {
		sysType = "8309"
	}
	sysSize := "0"
	if p.Layout.RootGiB > 0 {
		sysSize = fmt.Sprintf("+%dG", p.Layout.RootGiB)
	}
	g.run("Partition "+disk+": EFI system, /boot, system",
		"sgdisk", "--clear",
		fmt.Sprintf("--new=1:0:+%dM", p.Layout.ESPMiB), "--typecode=1:EF00", "--change-name=1:EFI System Partition",
		fmt.Sprintf("--new=2:0:+%dM", p.Layout.BootMiB), "--typecode=2:8300", "--change-name=2:boot",
		"--new=3:0:"+sysSize, "--typecode=3:"+sysType, "--change-name=3:"+sysName,
		disk)
	g.run("Wait for the new partitions", "udevadm", "settle", "--timeout=30")
	g.run("Erase old signatures at the start of each partition", "wipefs", "--all", "--force", r.ESPDev, r.BootDev, r.SystemDev)
	g.run("Format the EFI system partition (FAT32)", "mkfs.vfat", "-F", "32", "-n", "EFI", "-i", r.ESPVolID, r.ESPDev)
	g.run("Format /boot (ext4, readable by GRUB)", "mkfs.ext4", "-q", "-F", "-L", "boot", "-U", r.BootUUID, r.BootDev)

	// --- encryption ------------------------------------------------------------------------
	if p.Encrypted() {
		g.phase = "encryption"
		g.add(Step{Title: "Create a temporary key for the new LUKS2 volume",
			Write: &FileWrite{Path: r.KeyFile(), Mode: 0o600, RandomChars: 64},
			Undo:  []string{"rm", "-f", r.KeyFile()},
			Note:  "it lives in memory only and is removed once the unlock method and the recovery key are enrolled"}).ID = "luks-key"
		g.run("Encrypt the system partition (LUKS2)", "cryptsetup", "luksFormat", "--batch-mode", "--type", "luks2",
			"--uuid", r.LUKSUUID, "--label", "basalt", "--key-file", r.KeyFile(), r.SystemDev).Weight = 3
		s := g.run("Open the encrypted volume", "cryptsetup", "open", "--key-file", r.KeyFile(), r.SystemDev, r.CryptName())
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
	if p.Encrypted() {
		g.write("Write /etc/crypttab", g.t("/etc/crypttab"), 0o600, g.crypttab())
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
	g.chroot("Use the Basalt boot splash", "plymouth-set-default-theme", "basalt").Optional = true
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
	g.run("Add a firmware boot entry", "efibootmgr", "--create", "--disk", disk, "--part", "1",
		"--label", "Basalt OS", "--loader", `\EFI\fedora\shimx64.efi`).Optional = true
	g.out[len(g.out)-1].Note = "the firmware's fallback path (\\EFI\\BOOT\\BOOTX64.EFI, also Fedora's shim) boots the disk without it"

	// --- disk unlock -------------------------------------------------------------------------
	if p.Encrypted() {
		g.phase = "unlock"
		g.unlock()
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
	s = g.chroot("Label every file for SELinux (target policy)", relabel...)
	s.Weight = 6
	s.Note = "every subvolume, /.snapshots and /boot by name: setfiles does not cross into another file system on its own; " +
		"append-only and immutable files (the assistant's audit log) lose that attribute for the relabel and get it back"
	s.KeepAttrsUnder = g.T
	g.add(Step{Title: "Copy the install log into the installed system",
		Write: &FileWrite{Path: g.t("/var/log/basalt-installer/install.jsonl"), Mode: 0o600, FromLog: true}})
	g.chroot("Label the install record", "restorecon", "-R", "/var/log/basalt-installer")
	g.run("Unmount the installed system", "umount", "--recursive", g.T).Release = "root-mount"
	g.run("Flush writes to disk", "sync")
	if p.Encrypted() {
		g.run("Wait for device events to settle", "udevadm", "settle", "--timeout=30")
		g.run("Show what still uses the file system (diagnostics)", "findmnt", "--source", dev).Optional = true
		s := g.run("Close the encrypted volume", "cryptsetup", "close", r.CryptName())
		s.Release, s.Retries = "luks-open", 10
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
	return ""
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
	argv := []string{"useradd", "--create-home"}
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
		home := "/home/" + u.Name
		g.chroot("Create "+home+"/.ssh", "install", "-d", "-m", "0700", "-o", u.Name, "-g", u.Name, home+"/.ssh")
		g.write("Authorize SSH keys for "+u.Name, g.t(home+"/.ssh/authorized_keys"), 0o600, strings.Join(u.SSHKeys, "\n")+"\n")
		g.chroot("Give "+u.Name+" its authorized_keys", "chown", u.Name+":"+u.Name, home+"/.ssh/authorized_keys")
	}
}

func (g *gen) repos() error {
	p := g.r.Plan
	enabled := func(b *bool) string {
		if b == nil || *b {
			return "1"
		}
		return "0"
	}
	g.write("Configure the basalt-tools repository (ADR 0005)", g.t("/etc/yum.repos.d/basalt-tools.repo"), 0o644,
		`# OpenBasalt tools (Samba Conductor, future tools): metadata only, nothing is
# installed unless chosen. Disable with: dnf config-manager setopt basalt-tools.enabled=0
# Written by the Basalt OS installer; it moves to basalt-release once the
# repository is published.
[basalt-tools]
name=Basalt OS tools $releasever - $basearch
baseurl=$basalt_repo_url/tools/$releasever/$basearch/
enabled=`+enabled(p.Repos.Tools)+`
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-basalt
skip_if_unavailable=True
metadata_expire=6h
`)
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
	key, dev := r.KeyFile(), r.SystemDev
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
	return b.String()
}

func (g *gen) crypttab() string {
	opts := "discard"
	if g.r.EnrollTPM && g.r.Plan.Encryption.Unlock == "tpm2" {
		// No headless=true: when the TPM refuses, the boot asks for the
		// recovery key on the console and the serial port.
		opts += ",tpm2-device=auto"
	}
	return fmt.Sprintf("%s UUID=%s none %s\n", g.r.CryptName(), g.r.LUKSUUID, opts)
}

// kernelCmdline is the command line of the installed system. withRoot adds
// root= and rootflags= (for /etc/kernel/cmdline; GRUB adds them itself).
func (g *gen) kernelCmdline(withRoot bool) string {
	r, p := g.r, g.r.Plan
	var args []string
	if withRoot {
		args = append(args, "root=UUID="+r.BtrfsUUID, "ro", "rootflags=subvol=root")
	}
	if p.Encrypted() {
		args = append(args, "rd.luks.uuid="+r.CryptName())
	}
	args = append(args, "console=tty0", "console=ttyS0,115200n8")
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
	return fmt.Sprintf(`# Install-time repositories of the Basalt OS installer (live system only).
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
`, fedora, fkey, updates, fkey, g.basaltInstallURL(), g.basaltKey())
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
