// Package plan defines the Basalt OS install plan: a declarative description
// of one installation (target disk, layout, encryption, profile, accounts,
// network, repositories). Every frontend (TUI, GUI, a plan file) produces a
// Plan; the engine only ever executes a validated and resolved Plan.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// APIVersion identifies the plan schema. Plans with another value are rejected.
const APIVersion = "basalt-install-plan/v1"

// Default package repositories of an installed system, on obpkg.org (the
// same defaults basalt-release ships in /etc/dnf/vars). Layout:
// <URL>/<releasever>/<arch>/. A lab or mirror sets repos.basalt.installed_url.
const (
	DefaultRepoURL    = "https://obpkg.org/basalt"
	DefaultToolsURL   = "https://obpkg.org/basalt-tools"
	DefaultTestingURL = "https://obpkg.org/basalt-testing"
)

// Plan is one installation, as written by a person or a frontend.
type Plan struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Edition    string     `json:"edition" yaml:"edition"`
	Release    int        `json:"release,omitempty" yaml:"release,omitempty"`
	Target     Target     `json:"target" yaml:"target"`
	Layout     Layout     `json:"layout" yaml:"layout"`
	Encryption Encryption `json:"encryption" yaml:"encryption"`
	Profile    string     `json:"profile" yaml:"profile"`
	Hostname   string     `json:"hostname" yaml:"hostname"`
	Timezone   string     `json:"timezone" yaml:"timezone"`
	Locale     string     `json:"locale" yaml:"locale"`
	Keymap     string     `json:"keymap" yaml:"keymap"`
	Lockdown   *bool      `json:"lockdown,omitempty" yaml:"lockdown,omitempty"`
	Accounts   Accounts   `json:"accounts" yaml:"accounts"`
	SSH        SSH        `json:"ssh" yaml:"ssh"`
	Network    Network    `json:"network" yaml:"network"`
	Repos      Repos      `json:"repos" yaml:"repos"`
	Assistant  *bool      `json:"assistant,omitempty" yaml:"assistant,omitempty"`
	Packages   Packages   `json:"packages" yaml:"packages"`
	Finish     string     `json:"finish" yaml:"finish"`
}

// Target is the disk the system is installed on. The whole disk is wiped.
type Target struct {
	Disk string `json:"disk" yaml:"disk"`
	// Wipe must be true: it records that the author knows the disk is erased.
	Wipe bool `json:"wipe" yaml:"wipe"`
}

// Layout describes the partitions and btrfs subvolumes.
type Layout struct {
	// Mode is "automatic" (the Basalt layout, whole disk) or "manual"
	// (the same layout with sizes and the optional subvolumes chosen).
	Mode    string `json:"mode" yaml:"mode"`
	ESPMiB  int    `json:"esp_mib,omitempty" yaml:"esp_mib,omitempty"`
	BootMiB int    `json:"boot_mib,omitempty" yaml:"boot_mib,omitempty"`
	// RootGiB is the size of the btrfs (LUKS2) partition; 0 uses the rest
	// of the disk. Manual mode only.
	RootGiB int `json:"root_gib,omitempty" yaml:"root_gib,omitempty"`
	// Subvolumes is the list of btrfs subvolumes (names from Subvolumes()).
	// Empty means the full default set. Manual mode only.
	Subvolumes []string `json:"subvolumes,omitempty" yaml:"subvolumes,omitempty"`
}

// Encryption is LUKS2 under btrfs (ADR 0002).
type Encryption struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Unlock is how the disk opens at boot: tpm2 (sealed to PCR 7), tang,
	// tpm2+tang, or recovery-only (the recovery key is typed at every boot).
	Unlock string `json:"unlock" yaml:"unlock"`
	// TPM2PCRs is fixed to [7] (Secure Boot state) in this version.
	TPM2PCRs []int `json:"tpm2_pcrs,omitempty" yaml:"tpm2_pcrs,omitempty"`
	Tang     Tang  `json:"tang,omitempty" yaml:"tang,omitempty"`
	// Passphrase adds a key slot opened by a passphrase typed at boot
	// (desktops). Never written to logs or saved plans.
	Passphrase string `json:"passphrase,omitempty" yaml:"passphrase,omitempty"`
	// StoreRecoveryKey also leaves the recovery key in
	// /root/basalt-recovery-key.txt on the installed system (what the
	// kickstart does). Default: false, the key is only shown once.
	StoreRecoveryKey bool `json:"store_recovery_key,omitempty" yaml:"store_recovery_key,omitempty"`
	// RecoveryKeyMedia is the label of a file system on removable media (a
	// USB stick) that receives a copy of the recovery key when the
	// installation has succeeded. Writing it there counts as the
	// acknowledgement, so an unattended installation does not wait for a
	// person. Default: none, the key is only shown.
	RecoveryKeyMedia string `json:"recovery_key_media,omitempty" yaml:"recovery_key_media,omitempty"`
}

// Tang is a network unlock server (Clevis).
type Tang struct {
	URL        string `json:"url,omitempty" yaml:"url,omitempty"`
	Thumbprint string `json:"thumbprint,omitempty" yaml:"thumbprint,omitempty"`
}

// Accounts are the ways to log in to the installed system.
type Accounts struct {
	Root Root  `json:"root" yaml:"root"`
	User *User `json:"user,omitempty" yaml:"user,omitempty"`
}

// Root is the root account. Without a password it stays locked.
type Root struct {
	SSHKeys      []string `json:"ssh_keys,omitempty" yaml:"ssh_keys,omitempty"`
	PasswordHash string   `json:"password_hash,omitempty" yaml:"password_hash,omitempty"`
	Password     string   `json:"password,omitempty" yaml:"password,omitempty"`
}

// User is one regular account, an administrator (wheel) by default.
type User struct {
	Name         string   `json:"name" yaml:"name"`
	FullName     string   `json:"full_name,omitempty" yaml:"full_name,omitempty"`
	PasswordHash string   `json:"password_hash,omitempty" yaml:"password_hash,omitempty"`
	Password     string   `json:"password,omitempty" yaml:"password,omitempty"`
	SSHKeys      []string `json:"ssh_keys,omitempty" yaml:"ssh_keys,omitempty"`
	Admin        *bool    `json:"admin,omitempty" yaml:"admin,omitempty"`
}

// SSH is the SSH server policy. Basalt OS allows public keys only.
type SSH struct {
	// PasswordAuth allows password logins over SSH (not recommended).
	PasswordAuth bool `json:"password_auth,omitempty" yaml:"password_auth,omitempty"`
}

// Network is the first network connection of the installed system.
type Network struct {
	Mode      string   `json:"mode" yaml:"mode"` // dhcp | static
	Interface string   `json:"interface,omitempty" yaml:"interface,omitempty"`
	Address   string   `json:"address,omitempty" yaml:"address,omitempty"` // CIDR
	Gateway   string   `json:"gateway,omitempty" yaml:"gateway,omitempty"`
	DNS       []string `json:"dns,omitempty" yaml:"dns,omitempty"`
}

// Repos are the package repositories used at install time and configured
// on the installed system (ADR 0005).
type Repos struct {
	Basalt     BasaltRepo `json:"basalt" yaml:"basalt"`
	Fedora     FedoraRepo `json:"fedora,omitempty" yaml:"fedora,omitempty"`
	Tools      *bool      `json:"tools,omitempty" yaml:"tools,omitempty"`
	ThirdParty ThirdParty `json:"third_party,omitempty" yaml:"third_party,omitempty"`
}

// BasaltRepo is the signed Basalt OS repository.
type BasaltRepo struct {
	// URL is where the installer reads Basalt packages: "media" (the
	// repository on the installer image) or an http(s) or file URL. The tree
	// is <URL>/<release>/<arch>/.
	URL string `json:"url" yaml:"url"`
	// InstalledURL is the base URL the installed system uses
	// (/etc/dnf/vars/basalt_repo_url). Default: URL when it is http(s),
	// else DefaultRepoURL.
	InstalledURL string `json:"installed_url,omitempty" yaml:"installed_url,omitempty"`
	// InstalledToolsURL is the base URL of the basalt-tools repository on
	// the installed system (/etc/dnf/vars/basalt_tools_url). Default:
	// DefaultToolsURL with the default repository, else <InstalledURL>/tools
	// (the layout of a lab or mirror repository).
	InstalledToolsURL string `json:"installed_tools_url,omitempty" yaml:"installed_tools_url,omitempty"`
	// GPGKey is the repository key file (default: the one on the media).
	GPGKey string `json:"gpg_key,omitempty" yaml:"gpg_key,omitempty"`
}

// FedoraRepo overrides where Fedora packages come from (a local mirror).
type FedoraRepo struct {
	BaseURL        string `json:"baseurl,omitempty" yaml:"baseurl,omitempty"`
	UpdatesBaseURL string `json:"updates_baseurl,omitempty" yaml:"updates_baseurl,omitempty"`
}

// ThirdParty lists official third-party repositories.
type ThirdParty struct {
	TUITools *bool `json:"tui_tools,omitempty" yaml:"tui_tools,omitempty"`
}

// Packages adds packages to the profile's set.
type Packages struct {
	Extra []string `json:"extra,omitempty" yaml:"extra,omitempty"`
}

// Subvolume is one btrfs subvolume of the Basalt layout.
type Subvolume struct {
	Name       string
	Mountpoint string
	Required   bool
	Purpose    string
}

// Subvolumes returns the Basalt layout (ADR 0006, kickstart/basalt-server.ks):
// the system in "root", data that must survive a rollback in its own subvolume.
func Subvolumes() []Subvolume {
	return []Subvolume{
		{"root", "/", true, "the system; snapshots and rollback apply here"},
		{"home", "/home", true, "user data, never rolled back"},
		{"srv", "/srv", false, "served data"},
		{"var_log", "/var/log", true, "logs survive a rollback"},
		{"var_cache", "/var/cache", false, "caches"},
		{"var_tmp", "/var/tmp", false, "temporary files kept across reboots"},
		{"var_spool", "/var/spool", false, "mail and print queues"},
		{"var_lib_containers", "/var/lib/containers", false, "container images and volumes"},
		{"var_lib_libvirt", "/var/lib/libvirt", false, "virtual machines"},
		{"var_lib_pgsql", "/var/lib/pgsql", false, "PostgreSQL"},
		{"var_lib_mysql", "/var/lib/mysql", false, "MariaDB / MySQL"},
	}
}

// Defaults are the values a plan gets for every field it leaves empty.
const (
	DefaultESPMiB   = 600
	DefaultBootMiB  = 1024
	DefaultHostname = "basalt"
	DefaultTimezone = "Etc/UTC"
	DefaultLocale   = "en_US.UTF-8"
	DefaultKeymap   = "us"
)

// Default returns a complete plan for disk with the Basalt defaults.
func Default(disk string) Plan {
	p := Plan{APIVersion: APIVersion, Target: Target{Disk: disk}}
	p.ApplyDefaults()
	return p
}

// Bool returns a pointer to b, for the optional fields.
func Bool(b bool) *bool { return &b }

// ApplyDefaults fills every empty field with its default. It never changes a
// value that was set.
func (p *Plan) ApplyDefaults() {
	if p.APIVersion == "" {
		p.APIVersion = APIVersion
	}
	if p.Edition == "" {
		p.Edition = "server"
	}
	if p.Layout.Mode == "" {
		p.Layout.Mode = "automatic"
	}
	if p.Layout.ESPMiB == 0 {
		p.Layout.ESPMiB = DefaultESPMiB
	}
	if p.Layout.BootMiB == 0 {
		p.Layout.BootMiB = DefaultBootMiB
	}
	if len(p.Layout.Subvolumes) == 0 {
		for _, s := range Subvolumes() {
			p.Layout.Subvolumes = append(p.Layout.Subvolumes, s.Name)
		}
	}
	if p.Encryption.Enabled == nil {
		p.Encryption.Enabled = Bool(true)
	}
	if p.Encryption.Unlock == "" {
		p.Encryption.Unlock = "tpm2"
	}
	if len(p.Encryption.TPM2PCRs) == 0 {
		p.Encryption.TPM2PCRs = []int{7}
	}
	if p.Profile == "" {
		p.Profile = "auto"
	}
	if p.Hostname == "" {
		p.Hostname = DefaultHostname
	}
	if p.Timezone == "" {
		p.Timezone = DefaultTimezone
	}
	if p.Locale == "" {
		p.Locale = DefaultLocale
	}
	if p.Keymap == "" {
		p.Keymap = DefaultKeymap
	}
	if p.Lockdown == nil {
		p.Lockdown = Bool(true)
	}
	if p.Accounts.User != nil && p.Accounts.User.Admin == nil {
		p.Accounts.User.Admin = Bool(true)
	}
	if p.Network.Mode == "" {
		p.Network.Mode = "dhcp"
	}
	if p.Repos.Basalt.URL == "" {
		p.Repos.Basalt.URL = "media"
	}
	if p.Repos.Tools == nil {
		p.Repos.Tools = Bool(true)
	}
	if p.Repos.ThirdParty.TUITools == nil {
		p.Repos.ThirdParty.TUITools = Bool(true)
	}
	if p.Assistant == nil {
		p.Assistant = Bool(true)
	}
	if p.Finish == "" {
		p.Finish = "reboot"
	}
}

// Encrypted reports whether the disk is encrypted.
func (p Plan) Encrypted() bool { return p.Encryption.Enabled == nil || *p.Encryption.Enabled }

// Load reads a plan from a YAML or JSON file (JSON is valid YAML) and fills
// the defaults. Unknown fields are an error, so a typo never silently falls
// back to a default.
func Load(path string) (Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	return Parse(data, filepath.Ext(path))
}

// Parse decodes a plan; ext (".json", ".yaml") picks the decoder, anything
// else tries JSON when the data starts with "{" and YAML otherwise.
func Parse(data []byte, ext string) (Plan, error) {
	var p Plan
	trimmed := bytes.TrimSpace(data)
	isJSON := ext == ".json" || (ext != ".yaml" && ext != ".yml" && len(trimmed) > 0 && trimmed[0] == '{')
	if isJSON {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return Plan{}, fmt.Errorf("plan: %w", err)
		}
	} else {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&p); err != nil {
			return Plan{}, fmt.Errorf("plan: %w", err)
		}
	}
	p.ApplyDefaults()
	return p, nil
}

// Redacted returns a copy without secrets (passwords and the encryption
// passphrase), for logs, saved plans and the audit record. Password hashes
// are replaced too: they are offline-attackable.
func (p Plan) Redacted() Plan {
	r := p
	r.Encryption.Passphrase = redact(p.Encryption.Passphrase)
	r.Accounts.Root.Password = redact(p.Accounts.Root.Password)
	r.Accounts.Root.PasswordHash = redact(p.Accounts.Root.PasswordHash)
	r.Accounts.Root.SSHKeys = append([]string(nil), p.Accounts.Root.SSHKeys...)
	if p.Accounts.User != nil {
		u := *p.Accounts.User
		u.Password = redact(u.Password)
		u.PasswordHash = redact(u.PasswordHash)
		u.SSHKeys = append([]string(nil), u.SSHKeys...)
		r.Accounts.User = &u
	}
	return r
}

// Redacted marks a secret that was set but is not shown.
const Redacted = "<redacted>"

func redact(s string) string {
	if s == "" {
		return ""
	}
	return Redacted
}

// JSON renders the plan as indented JSON.
func (p Plan) JSON() string {
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b) + "\n"
}

// YAML renders the plan as YAML.
func (p Plan) YAML() string {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(p)
	return buf.String()
}

// SubvolumeByName returns the layout entry for a subvolume name.
func SubvolumeByName(name string) (Subvolume, bool) {
	for _, s := range Subvolumes() {
		if s.Name == name {
			return s, true
		}
	}
	return Subvolume{}, false
}

// IsAdminUser reports whether the plan's user is an administrator.
func (p Plan) IsAdminUser() bool {
	return p.Accounts.User != nil && (p.Accounts.User.Admin == nil || *p.Accounts.User.Admin)
}

// Summary is a one-line description of the plan, for confirmations.
func (p Plan) Summary() string {
	enc := "plain btrfs"
	if p.Encrypted() {
		enc = "LUKS2 + " + p.Encryption.Unlock
	}
	return strings.Join([]string{p.Edition, p.Target.Disk, enc, "profile " + p.Profile, p.Hostname}, ", ")
}
