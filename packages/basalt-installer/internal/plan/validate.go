package plan

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// Severity of a validation issue. Errors block the installation; warnings
// are shown in the preview and must be read, but do not block.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Issue is one finding of Validate.
type Issue struct {
	Severity Severity `json:"severity"`
	Field    string   `json:"field"`
	Message  string   `json:"message"`
}

func (i Issue) String() string { return fmt.Sprintf("%s: %s: %s", i.Severity, i.Field, i.Message) }

// Issues is the result of Validate.
type Issues []Issue

// Errors returns only the blocking issues.
func (is Issues) Errors() Issues {
	var out Issues
	for _, i := range is {
		if i.Severity == Error {
			out = append(out, i)
		}
	}
	return out
}

// Err returns an error listing the blocking issues, or nil.
func (is Issues) Err() error {
	errs := is.Errors()
	if len(errs) == 0 {
		return nil
	}
	lines := make([]string, len(errs))
	for i, e := range errs {
		lines[i] = e.String()
	}
	return fmt.Errorf("the plan is not valid:\n  %s", strings.Join(lines, "\n  "))
}

// KickstartHint points complex storage at the path that handles it.
const KickstartHint = "this installer handles one local disk; use the kickstart installer " +
	"(kickstart/basalt-server.ks, see docs/installer.md, \"Complex storage\")"

// MinDiskGiB is the smallest disk the installer accepts.
const MinDiskGiB = 16

var (
	hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	userName      = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	cryptHash     = regexp.MustCompile(`^\$(y|gy|7|2b|2y|6|5)\$[./A-Za-z0-9$=,]+$`)
	keymapName    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	localeName    = regexp.MustCompile(`^[A-Za-z]{2,3}(_[A-Z]{2})?(\.[A-Za-z0-9-]+)?(@[a-z]+)?$`)
	pkgName       = regexp.MustCompile(`^@?[A-Za-z0-9._+-]+$`)
	ifaceName     = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)
	sshKeyTypes   = []string{"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384",
		"ecdsa-sha2-nistp521", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com"}
	reservedUsers = map[string]bool{"root": true, "bin": true, "daemon": true, "adm": true, "lp": true,
		"sync": true, "shutdown": true, "halt": true, "mail": true, "operator": true, "games": true,
		"ftp": true, "nobody": true, "sshd": true, "systemd-network": true, "dbus": true, "polkitd": true,
		"chrony": true, "tss": true}
)

// Validate checks a plan. Facts may be nil (offline validation of a plan
// file); then checks against the machine are skipped. ZoneinfoDir is where
// time zones are looked up ("" skips that check).
func Validate(p Plan, f *probe.Facts, zoneinfoDir string) Issues {
	var is Issues
	add := func(sev Severity, field, format string, a ...any) {
		is = append(is, Issue{sev, field, fmt.Sprintf(format, a...)})
	}

	if p.APIVersion != APIVersion {
		add(Error, "apiVersion", "must be %q, got %q", APIVersion, p.APIVersion)
	}
	switch p.Edition {
	case "server":
	case "desktop":
		add(Warning, "edition", "the desktop edition is experimental in this installer version: it installs the server system plus packages.extra")
	default:
		add(Error, "edition", "must be server or desktop, got %q", p.Edition)
	}
	if p.Release != 0 && (p.Release < 40 || p.Release > 99) {
		add(Error, "release", "unexpected Fedora release %d", p.Release)
	}

	// --- target disk ---------------------------------------------------------------
	if p.Target.Disk == "" {
		add(Error, "target.disk", "no target disk")
	} else if !strings.HasPrefix(p.Target.Disk, "/dev/") {
		add(Error, "target.disk", "must be a device path such as /dev/vda, got %q", p.Target.Disk)
	} else if isPartitionPath(p.Target.Disk) {
		add(Error, "target.disk", "%s looks like a partition; give the whole disk (%s)", p.Target.Disk, KickstartHint)
	} else if strings.HasPrefix(p.Target.Disk, "/dev/md") || strings.HasPrefix(p.Target.Disk, "/dev/mapper/") || strings.HasPrefix(p.Target.Disk, "/dev/dm-") {
		add(Error, "target.disk", "%s is a RAID, multipath or device-mapper device: %s", p.Target.Disk, KickstartHint)
	}
	if p.Target.ESP != "" && p.Target.Wipe {
		add(Error, "target.esp", "%s", i18n.T("an existing EFI system partition can only be shared when the disk is not erased (wipe: false)"))
	}
	var disk probe.Disk
	haveDisk := false
	if f != nil && p.Target.Disk != "" {
		disk, haveDisk = f.Disk(p.Target.Disk)
		switch {
		case !haveDisk:
			add(Error, "target.disk", "%s is not a disk on this machine", p.Target.Disk)
		case disk.Complex != "":
			add(Error, "target.disk", "%s is a %s: %s", disk.Path, disk.Complex, KickstartHint)
		case disk.ReadOnly:
			add(Error, "target.disk", "%s is read-only", disk.Path)
		case disk.InUse != "":
			add(Error, "target.disk", "%s is in use: %s", disk.Path, disk.InUse)
		case disk.SizeGiB() < MinDiskGiB:
			add(Error, "target.disk", "%s has %s; at least %d GiB are needed", disk.Path, probe.HumanSize(disk.SizeBytes), MinDiskGiB)
		}
		if haveDisk && disk.Removable {
			add(Warning, "target.disk", "%s is a removable disk", disk.Path)
		}
		if haveDisk && disk.Contents != "empty" && p.Target.Wipe {
			add(Warning, "target.disk", "%s is not empty (%s); everything on it is erased", disk.Path, disk.Contents)
		}
		if haveDisk && !p.Target.Wipe {
			validateFreeSpace(p, disk, add)
		}
	}

	// --- layout ----------------------------------------------------------------------
	switch p.Layout.Mode {
	case "automatic", "manual":
	default:
		add(Error, "layout.mode", "must be automatic or manual, got %q", p.Layout.Mode)
	}
	if p.Layout.ESPMiB < 200 || p.Layout.ESPMiB > 4096 {
		add(Error, "layout.esp_mib", "EFI system partition must be 200 to 4096 MiB, got %d", p.Layout.ESPMiB)
	}
	if p.Layout.BootMiB < 512 || p.Layout.BootMiB > 8192 {
		add(Error, "layout.boot_mib", "/boot must be 512 to 8192 MiB, got %d", p.Layout.BootMiB)
	}
	if p.Layout.Mode == "automatic" {
		if p.Layout.RootGiB != 0 {
			add(Error, "layout.root_gib", "only in manual mode (automatic uses the whole disk)")
		}
		if p.Layout.ESPMiB != DefaultESPMiB || p.Layout.BootMiB != DefaultBootMiB {
			add(Error, "layout", "sizes can only be changed in manual mode")
		}
		if !sameSet(p.Layout.Subvolumes, DefaultSubvolumeNames(p)) {
			add(Error, "layout.subvolumes", "the subvolume list can only be changed in manual mode")
		}
	}
	if p.Layout.RootGiB < 0 || (p.Layout.RootGiB > 0 && p.Layout.RootGiB < 8) {
		add(Error, "layout.root_gib", "the system partition needs at least 8 GiB, got %d", p.Layout.RootGiB)
	}
	if haveDisk && p.Target.Wipe {
		need := NeededBytes(p)
		if need > disk.SizeBytes {
			add(Error, "layout", "the partitions need %s, the disk has %s", probe.HumanSize(need), probe.HumanSize(disk.SizeBytes))
		}
	}
	seen := map[string]bool{}
	for _, s := range p.Layout.Subvolumes {
		if _, ok := SubvolumeByName(s); !ok {
			add(Error, "layout.subvolumes", "unknown subvolume %q", s)
		}
		if seen[s] {
			add(Error, "layout.subvolumes", "subvolume %q listed twice", s)
		}
		seen[s] = true
	}
	for _, s := range Subvolumes() {
		if s.Name == "home" && p.HomeElsewhere() {
			if seen["home"] {
				add(Error, "layout.subvolumes", "%s", i18n.T("/home is on a partition of its own (encryption scope home, or an existing /home): leave the home subvolume out"))
			}
			continue
		}
		if s.Required && !seen[s.Name] {
			add(Error, "layout.subvolumes", "subvolume %q (%s) is required", s.Name, s.Mountpoint)
		}
	}

	// --- encryption scope and /home ------------------------------------------------------
	switch p.Encryption.Scope {
	case "", "system", "home", "none":
	default:
		add(Error, "encryption.scope", i18n.T("must be system, home or none, got %q"), p.Encryption.Scope)
	}
	if p.Encryption.Scope == "home" && p.Home.Existing != nil {
		add(Error, "encryption.scope", "%s", i18n.T("scope home creates a new encrypted /home; with an existing /home use system or none"))
	}
	if p.Encryption.Scope == "home" {
		add(Warning, "encryption.scope", "%s", i18n.T("only /home is encrypted: the system, its settings in /etc, the logs and /var (containers, virtual machines, databases) are not"))
	}
	if e := p.Home.Existing; e != nil {
		validateExistingHome(p, *e, f, add)
	}

	// --- encryption ------------------------------------------------------------------
	if p.Encrypted() {
		switch p.Encryption.Unlock {
		case "tpm2", "tang", "tpm2+tang", "recovery-only":
		default:
			add(Error, "encryption.unlock", "must be tpm2, tang, tpm2+tang or recovery-only, got %q", p.Encryption.Unlock)
		}
		if len(p.Encryption.TPM2PCRs) != 1 || p.Encryption.TPM2PCRs[0] != 7 {
			add(Error, "encryption.tpm2_pcrs", "only [7] (the Secure Boot state, ADR 0002) is supported, got %v", p.Encryption.TPM2PCRs)
		}
		usesTang := strings.Contains(p.Encryption.Unlock, "tang")
		usesTPM := strings.Contains(p.Encryption.Unlock, "tpm2")
		if usesTang {
			u, err := url.Parse(p.Encryption.Tang.URL)
			if p.Encryption.Tang.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add(Error, "encryption.tang.url", "a Tang server URL (http or https) is required for unlock %s", p.Encryption.Unlock)
			}
			if p.Encryption.Tang.Thumbprint == "" {
				add(Warning, "encryption.tang.thumbprint", "no thumbprint: the first advertisement is trusted (compare it later with clevis luks list)")
			}
		} else if p.Encryption.Tang != (Tang{}) {
			add(Error, "encryption.tang", "set only for unlock tang or tpm2+tang")
		}
		if f != nil && usesTPM && !f.TPM2 {
			if p.Encryption.Unlock == "tpm2+tang" {
				add(Error, "encryption.unlock", "tpm2+tang needs a TPM 2.0 and none was found")
			} else {
				add(Warning, "encryption.unlock", "no TPM 2.0 found: the recovery key is the only way to unlock the disk, at every boot")
			}
		}
		if f != nil && usesTPM && f.TPM2 && f.SecureBoot != "enabled" {
			add(Warning, "encryption.unlock", "Secure Boot is %s: the TPM key is sealed to this state and stops working when Secure Boot is turned on", f.SecureBoot)
		}
		if p.Encryption.Unlock == "recovery-only" && p.Encryption.Passphrase == "" {
			add(Warning, "encryption.unlock", "recovery-only: the recovery key must be typed at every boot (no TPM, Tang or passphrase)")
		}
		if pw := p.Encryption.Passphrase; pw != "" && pw != Redacted && len(pw) < 12 {
			add(Error, "encryption.passphrase", "use at least 12 characters")
		}
	} else {
		add(Warning, "encryption.enabled", "the disk is not encrypted (Basalt OS encrypts by default)")
		if p.Encryption.Passphrase != "" || p.Encryption.StoreRecoveryKey || p.Encryption.RecoveryKeyMedia != "" || p.Encryption.Tang != (Tang{}) {
			add(Error, "encryption", "encryption settings given but encryption is off")
		}
	}
	if p.Encryption.StoreRecoveryKey {
		add(Warning, "encryption.store_recovery_key", "the recovery key stays in /root/basalt-recovery-key.txt on the disk it protects: move it off the machine")
	}
	if l := p.Encryption.RecoveryKeyMedia; l != "" && p.Encrypted() {
		if l == probe.InstallerMediaLabel {
			add(Error, "encryption.recovery_key_media", "%s", i18n.T("the installer media is read-only: name the label of a USB stick or another removable file system"))
		} else if f != nil && findKeyMedium(f.KeyMedia, l) == nil {
			add(Error, "encryption.recovery_key_media", i18n.T("no removable file system labeled %q was found (plug in the USB stick that should take the recovery key)"), l)
		}
	}

	// --- system ----------------------------------------------------------------------
	switch p.Profile {
	case "auto", "minimal", "standard":
	default:
		add(Error, "profile", "must be auto, minimal or standard, got %q", p.Profile)
	}
	if len(p.Hostname) > 253 || p.Hostname == "" {
		add(Error, "hostname", "invalid host name %q", p.Hostname)
	} else {
		for _, l := range strings.Split(p.Hostname, ".") {
			if !hostnameLabel.MatchString(l) {
				add(Error, "hostname", "invalid host name %q (lower-case letters, digits and hyphens)", p.Hostname)
				break
			}
		}
	}
	if strings.Contains(p.Timezone, "..") || strings.HasPrefix(p.Timezone, "/") || p.Timezone == "" {
		add(Error, "timezone", "invalid time zone %q", p.Timezone)
	} else if zoneinfoDir != "" {
		if _, err := os.Stat(filepath.Join(zoneinfoDir, p.Timezone)); err != nil {
			add(Error, "timezone", "unknown time zone %q", p.Timezone)
		}
	}
	if !localeName.MatchString(p.Locale) {
		add(Error, "locale", "invalid locale %q", p.Locale)
	}
	if !keymapName.MatchString(p.Keymap) {
		add(Error, "keymap", "invalid keymap %q", p.Keymap)
	}
	if p.Lockdown != nil && !*p.Lockdown {
		add(Warning, "lockdown", "kernel lockdown and module signature enforcement are left off the command line")
	}

	// --- accounts --------------------------------------------------------------------
	checkKeys := func(field string, keys []string) {
		for _, k := range keys {
			fields := strings.Fields(k)
			if len(fields) < 2 || !contains(sshKeyTypes, fields[0]) || strings.ContainsAny(k, "\n\r") {
				add(Error, field, "not an OpenSSH public key: %q", shorten(k))
			}
		}
	}
	checkPassword := func(field, pw, hash string) {
		if pw != "" && hash != "" {
			add(Error, field, "give a password or a password hash, not both")
		}
		if hash != "" && hash != Redacted && !cryptHash.MatchString(hash) {
			add(Error, field+"_hash", "not a crypt(3) hash (yescrypt $y$ or SHA-512 $6$ expected)")
		}
		if pw != "" && pw != Redacted && len(pw) < 8 {
			add(Error, field, "use at least 8 characters")
		}
		if strings.ContainsAny(pw, "\n\r:") || strings.ContainsAny(hash, "\n\r:") {
			add(Error, field, "must not contain line breaks or colons")
		}
	}
	r := p.Accounts.Root
	checkKeys("accounts.root.ssh_keys", r.SSHKeys)
	checkPassword("accounts.root.password", r.Password, r.PasswordHash)
	rootAccess := len(r.SSHKeys) > 0 || r.Password != "" || r.PasswordHash != ""
	adminAccess := len(r.SSHKeys) > 0 || r.Password != "" || r.PasswordHash != ""
	if u := p.Accounts.User; u != nil {
		if !userName.MatchString(u.Name) {
			add(Error, "accounts.user.name", "invalid user name %q", u.Name)
		} else if reservedUsers[u.Name] {
			add(Error, "accounts.user.name", "%q is a system account", u.Name)
		}
		if strings.ContainsAny(u.FullName, ":\n\r") {
			add(Error, "accounts.user.full_name", "must not contain colons or line breaks")
		}
		checkKeys("accounts.user.ssh_keys", u.SSHKeys)
		if u.HomeDir != "" && (!strings.HasPrefix(u.HomeDir, "/home/") || strings.Contains(u.HomeDir, "..") || len(strings.TrimPrefix(u.HomeDir, "/home/")) == 0) {
			add(Error, "accounts.user.home_dir", i18n.T("must be a directory under /home, got %q"), u.HomeDir)
		}
		if u.UID != 0 && (u.UID < 1000 || u.UID > 60000) {
			add(Error, "accounts.user.uid", i18n.T("must be from 1000 to 60000, got %d"), u.UID)
		}
		if u.GID != 0 && (u.GID < 1000 || u.GID > 60000) {
			add(Error, "accounts.user.gid", i18n.T("must be from 1000 to 60000, got %d"), u.GID)
		}
		if p.Home.Existing != nil && u.UID == 0 {
			add(Warning, "accounts.user.uid", "%s", i18n.T("no uid given: the files on the existing /home may belong to another number than the new user"))
		}
		checkPassword("accounts.user.password", u.Password, u.PasswordHash)
		userAccess := len(u.SSHKeys) > 0 || u.Password != "" || u.PasswordHash != ""
		if !userAccess {
			add(Error, "accounts.user", "the user %q has neither a password nor an SSH key", u.Name)
		}
		if p.IsAdminUser() && userAccess {
			adminAccess = true
		}
		if p.IsAdminUser() && u.Password == "" && u.PasswordHash == "" {
			add(Warning, "accounts.user", "the administrator %q has no password: sudo will not work for it", u.Name)
		}
	}
	if !adminAccess {
		add(Error, "accounts", "no administrator access: give root an SSH key or password, or add an administrator user")
	}
	if !rootAccess && p.Accounts.User == nil {
		add(Error, "accounts", "nobody can log in")
	}
	remote := len(r.SSHKeys) > 0 || (p.Accounts.User != nil && len(p.Accounts.User.SSHKeys) > 0)
	if p.SSH.PasswordAuth {
		add(Warning, "ssh.password_auth", "password logins over SSH are allowed (Basalt OS default: public keys only)")
		remote = remote || p.Accounts.User != nil
	}
	if !remote && p.Edition == "server" {
		add(Warning, "accounts", "no SSH key: this server can only be reached on its console")
	}

	// --- network ---------------------------------------------------------------------
	switch p.Network.Mode {
	case "dhcp":
		if p.Network.Address != "" || p.Network.Gateway != "" || len(p.Network.DNS) > 0 {
			add(Error, "network", "address, gateway and dns are only for mode static")
		}
		if p.Network.Interface != "" && !ifaceName.MatchString(p.Network.Interface) {
			add(Error, "network.interface", "invalid interface name %q", p.Network.Interface)
		}
	case "static":
		if !ifaceName.MatchString(p.Network.Interface) {
			add(Error, "network.interface", "static needs an interface name, got %q", p.Network.Interface)
		} else if f != nil && len(f.Interfaces) > 0 && !contains(f.Interfaces, p.Network.Interface) {
			add(Warning, "network.interface", "%s is not an interface of this machine (%s)", p.Network.Interface, strings.Join(f.Interfaces, ", "))
		}
		ip, ipnet, err := net.ParseCIDR(p.Network.Address)
		if err != nil {
			add(Error, "network.address", "must be an address with prefix length, such as 192.0.2.10/24")
		}
		if p.Network.Gateway != "" {
			gw := net.ParseIP(p.Network.Gateway)
			if gw == nil {
				add(Error, "network.gateway", "invalid address %q", p.Network.Gateway)
			} else if err == nil && !ipnet.Contains(gw) {
				add(Warning, "network.gateway", "%s is outside %s", gw, ipnet)
			} else if err == nil && gw.Equal(ip) {
				add(Error, "network.gateway", "the gateway is the host's own address")
			}
		}
		for _, d := range p.Network.DNS {
			if net.ParseIP(d) == nil {
				add(Error, "network.dns", "invalid address %q", d)
			}
		}
	default:
		add(Error, "network.mode", "must be dhcp or static, got %q", p.Network.Mode)
	}

	// --- repositories ----------------------------------------------------------------
	b := p.Repos.Basalt
	if b.URL != "media" && !validRepoURL(b.URL) {
		add(Error, "repos.basalt.url", "must be \"media\" or an http(s) or file URL, got %q", b.URL)
	}
	if b.InstalledURL != "" && !validRepoURL(b.InstalledURL) {
		add(Error, "repos.basalt.installed_url", "must be an http(s) or file URL, got %q", b.InstalledURL)
	}
	if b.InstalledToolsURL != "" && !validRepoURL(b.InstalledToolsURL) {
		add(Error, "repos.basalt.installed_tools_url", "must be an http(s) or file URL, got %q", b.InstalledToolsURL)
	}
	if b.GPGKey != "" && !filepath.IsAbs(b.GPGKey) {
		add(Error, "repos.basalt.gpg_key", "must be an absolute path")
	}
	for field, u := range map[string]string{"repos.fedora.baseurl": p.Repos.Fedora.BaseURL, "repos.fedora.updates_baseurl": p.Repos.Fedora.UpdatesBaseURL} {
		if u != "" && !validRepoURL(u) {
			add(Error, field, "must be an http(s) or file URL, got %q", u)
		}
	}
	if p.Assistant != nil && !*p.Assistant {
		add(Warning, "assistant", "the system assistant is not installed (Basalt OS installs it by default)")
	}
	for _, pkg := range p.Packages.Extra {
		if !pkgName.MatchString(pkg) {
			add(Error, "packages.extra", "invalid package name %q", pkg)
		}
	}
	switch p.Finish {
	case "reboot", "poweroff", "none":
	default:
		add(Error, "finish", "must be reboot, poweroff or none, got %q", p.Finish)
	}

	// --- machine ---------------------------------------------------------------------
	if f != nil {
		if !f.UEFI {
			add(Error, "firmware", "the machine did not boot in UEFI mode; Basalt OS installs for UEFI only")
		}
		if f.Arch != "x86_64" {
			add(Error, "firmware", "only x86_64 is supported, this is %s", f.Arch)
		}
		if f.MemoryMiB > 0 && f.MemoryMiB < 1800 {
			add(Warning, "memory", "%d MiB of memory: the installer needs about 2 GiB", f.MemoryMiB)
		}
	}
	return is
}

func validRepoURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return u.Host != ""
	case "file":
		return strings.HasPrefix(u.Path, "/")
	}
	return false
}

// isPartitionPath recognizes /dev/sda1, /dev/vda2, /dev/nvme0n1p1, /dev/mmcblk0p1.
var partitionPath = regexp.MustCompile(`^/dev/((sd|vd|hd|xvd)[a-z]+[0-9]+|(nvme[0-9]+n[0-9]+|mmcblk[0-9]+|loop[0-9]+)p[0-9]+)$`)

func isPartitionPath(s string) bool { return partitionPath.MatchString(s) }

// DefaultSubvolumeNames is the automatic layout's subvolume list for a
// plan (without home when /home is a partition of its own).
func DefaultSubvolumeNames(p Plan) []string {
	var out []string
	for _, s := range Subvolumes() {
		if s.Name == "home" && p.HomeElsewhere() {
			continue
		}
		out = append(out, s.Name)
	}
	return out
}

// Sizes of the parts a plan creates.
const (
	// DefaultSystemGiBWithHome is the system partition's size when a new
	// /home partition takes the rest of the space.
	DefaultSystemGiBWithHome = 64
	MinHomeGiB               = 8
)

// NeededBytes is the space the new partitions need at least.
func NeededBytes(p Plan) int64 {
	esp := int64(p.Layout.ESPMiB) << 20
	if p.Target.ESP != "" {
		esp = 0
	}
	sys := int64(8) << 30
	if p.Layout.RootGiB > 0 {
		sys = int64(p.Layout.RootGiB) << 30
	}
	need := esp + int64(p.Layout.BootMiB)<<20 + sys + 2<<20
	if p.EncryptsHome() {
		need += int64(MinHomeGiB) << 30
	}
	return need
}

func validateFreeSpace(p Plan, disk probe.Disk, add func(Severity, string, string, ...any)) {
	if disk.Table != "gpt" {
		if len(disk.Partitions) == 0 {
			add(Error, "target.wipe", "%s", i18n.T("the disk has no partition table: there is nothing to keep, so erase it (wipe: true)"))
		} else {
			add(Error, "target.wipe", i18n.T("the disk has a %s partition table: installing next to other systems needs GPT (UEFI)"), disk.Table)
		}
		return
	}
	free, ok := disk.LargestFree()
	need := NeededBytes(p)
	if !ok || free.Bytes < need {
		add(Error, "target.wipe", i18n.T("the largest free space on %s is %s; the new partitions need %s (shrink another system's partition first)"),
			disk.Path, probe.HumanSize(free.Bytes), probe.HumanSize(need))
	}
	if len(disk.Partitions) >= 124 {
		add(Error, "target.wipe", "%s", i18n.T("the partition table has no free entries"))
	}
	for _, part := range disk.Partitions {
		if part.Mountpoint != "" {
			add(Error, "target.disk", i18n.T("%s is mounted on %s"), part.Path, part.Mountpoint)
		}
	}
	if p.Target.ESP != "" {
		var esp *probe.Partition
		for i := range disk.Partitions {
			if disk.Partitions[i].Path == p.Target.ESP {
				esp = &disk.Partitions[i]
			}
		}
		switch {
		case esp == nil:
			add(Error, "target.esp", i18n.T("%s is not a partition of %s"), p.Target.ESP, disk.Path)
		case esp.Type != probe.TypeESP || esp.FSType != "vfat":
			add(Error, "target.esp", i18n.T("%s is not an EFI system partition (FAT)"), p.Target.ESP)
		case esp.Bytes < 100<<20:
			add(Error, "target.esp", i18n.T("%s is too small to share (%s)"), p.Target.ESP, probe.HumanSize(esp.Bytes))
		default:
			add(Warning, "target.esp", "%s", i18n.T("the shared EFI system partition gets Fedora's boot files in EFI/fedora (shim, GRUB): another Fedora on it would be replaced"))
		}
	}
}

func validateExistingHome(p Plan, e ExistingHome, f *probe.Facts, add func(Severity, string, string, ...any)) {
	if !strings.HasPrefix(e.Device, "/dev/") {
		add(Error, "home.existing.device", i18n.T("must be a partition such as /dev/nvme1n1p1, got %q"), e.Device)
		return
	}
	if f == nil {
		return
	}
	part, ok := f.Partition(e.Device)
	if !ok {
		add(Error, "home.existing.device", i18n.T("%s is not a partition on this machine"), e.Device)
		return
	}
	if part.Disk == p.Target.Disk && p.Target.Wipe {
		add(Error, "home.existing.device", i18n.T("%s is on %s, the disk that is erased"), e.Device, part.Disk)
	}
	if part.Mountpoint != "" {
		add(Error, "home.existing.device", i18n.T("%s is mounted on %s"), e.Device, part.Mountpoint)
	}
	switch part.FSType {
	case "crypto_LUKS":
		if e.Passphrase == "" {
			add(Error, "home.existing.passphrase", "%s", i18n.T("the existing /home is encrypted: its passphrase is needed to open it during the installation"))
		}
	case "ext4", "xfs", "btrfs":
		if e.Passphrase != "" || e.TPM2 {
			add(Error, "home.existing", "%s", i18n.T("the existing /home is not encrypted: no passphrase or TPM"))
		}
		add(Warning, "home.existing", "%s", i18n.T("the existing /home is not encrypted"))
	default:
		add(Error, "home.existing.device", i18n.T("%s holds %q, not a LUKS volume or a Linux file system (ext4, xfs, btrfs)"), e.Device, part.FSType)
	}
	if e.TPM2 && !f.TPM2 {
		add(Error, "home.existing.tpm2", "%s", i18n.T("no TPM 2.0 on this machine"))
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

// findKeyMedium returns the removable file system with this label.
func findKeyMedium(media []probe.KeyMedium, label string) *probe.KeyMedium {
	for i := range media {
		if media[i].Label == label {
			return &media[i]
		}
	}
	return nil
}

// FindKeyMedium returns the removable file system with this label, or nil.
func FindKeyMedium(media []probe.KeyMedium, label string) *probe.KeyMedium {
	return findKeyMedium(media, label)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func shorten(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
