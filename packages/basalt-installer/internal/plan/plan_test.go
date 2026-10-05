package plan

import (
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl lab"

func facts() *probe.Facts {
	return &probe.Facts{
		UEFI: true, SecureBoot: "enabled", TPM2: true, Virt: "kvm", Arch: "x86_64", MemoryMiB: 4096, Release: 44,
		Interfaces: []string{"enp1s0"},
		Disks: []probe.Disk{
			{Name: "vda", Path: "/dev/vda", SizeBytes: 30 << 30, Contents: "empty"},
			{Name: "vdb", Path: "/dev/vdb", SizeBytes: 8 << 30, Contents: "empty"},
			{Name: "sdc", Path: "/dev/sdc", SizeBytes: 100 << 30, Contents: "1 partition: linux_raid_member", Complex: "member of a RAID array"},
			{Name: "sdd", Path: "/dev/sdd", SizeBytes: 100 << 30, Contents: "1 partition: ext4", InUse: "/dev/sdd1 is mounted on /run/media"},
		},
	}
}

func valid() Plan {
	p := Default("/dev/vda")
	p.Target.Wipe = true
	p.Accounts.Root.SSHKeys = []string{key}
	p.Repos.Basalt.URL = "http://10.0.2.2:8098"
	return p
}

func hasIssue(is Issues, sev Severity, field, substr string) bool {
	for _, i := range is {
		if i.Severity == sev && i.Field == field && strings.Contains(i.Message, substr) {
			return true
		}
	}
	return false
}

func TestDefaultPlanIsValid(t *testing.T) {
	is := Validate(valid(), facts(), "")
	if err := is.Err(); err != nil {
		t.Fatal(err)
	}
	for _, i := range is {
		t.Logf("%s", i)
	}
}

func TestDefaults(t *testing.T) {
	p := Default("/dev/vda")
	if !p.Encrypted() || p.Encryption.Unlock != "tpm2" || p.Encryption.TPM2PCRs[0] != 7 {
		t.Fatalf("encryption defaults: %+v", p.Encryption)
	}
	if p.Profile != "auto" || p.Layout.Mode != "automatic" || len(p.Layout.Subvolumes) != len(Subvolumes()) {
		t.Fatalf("layout/profile defaults: %+v %s", p.Layout, p.Profile)
	}
	if !*p.Assistant || !*p.Repos.Tools || !*p.Repos.ThirdParty.TUITools || !*p.Lockdown {
		t.Fatal("assistant, tools repo, tui-tools and lockdown are on by default")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Plan)
		field  string
		substr string
	}{
		{"keep partitions of an empty disk", func(p *Plan) { p.Target.Wipe = false }, "target.wipe", "nothing to keep"},
		{"no disk", func(p *Plan) { p.Target.Disk = "" }, "target.disk", "no target disk"},
		{"partition", func(p *Plan) { p.Target.Disk = "/dev/vda2" }, "target.disk", "partition"},
		{"nvme partition", func(p *Plan) { p.Target.Disk = "/dev/nvme0n1p3" }, "target.disk", "partition"},
		{"md device", func(p *Plan) { p.Target.Disk = "/dev/md0" }, "target.disk", "kickstart"},
		{"multipath", func(p *Plan) { p.Target.Disk = "/dev/mapper/mpatha" }, "target.disk", "kickstart"},
		{"raid member", func(p *Plan) { p.Target.Disk = "/dev/sdc" }, "target.disk", "RAID"},
		{"in use", func(p *Plan) { p.Target.Disk = "/dev/sdd" }, "target.disk", "in use"},
		{"too small", func(p *Plan) { p.Target.Disk = "/dev/vdb" }, "target.disk", "at least"},
		{"unknown disk", func(p *Plan) { p.Target.Disk = "/dev/sdz" }, "target.disk", "not a disk"},
		{"api version", func(p *Plan) { p.APIVersion = "v0" }, "apiVersion", "must be"},
		{"edition", func(p *Plan) { p.Edition = "workstation" }, "edition", "server or desktop"},
		{"layout mode", func(p *Plan) { p.Layout.Mode = "lvm" }, "layout.mode", "automatic or manual"},
		{"automatic sizes", func(p *Plan) { p.Layout.BootMiB = 2048 }, "layout", "manual mode"},
		{"automatic root size", func(p *Plan) { p.Layout.RootGiB = 20 }, "layout.root_gib", "manual"},
		{"required subvolume", func(p *Plan) { p.Layout.Mode = "manual"; p.Layout.Subvolumes = []string{"root", "var_log"} }, "layout.subvolumes", "\"home\""},
		{"unknown subvolume", func(p *Plan) {
			p.Layout.Mode = "manual"
			p.Layout.Subvolumes = append(p.Layout.Subvolumes, "nix")
		}, "layout.subvolumes", "unknown"},
		{"root too small", func(p *Plan) { p.Layout.Mode = "manual"; p.Layout.RootGiB = 4 }, "layout.root_gib", "at least 8"},
		{"does not fit", func(p *Plan) { p.Layout.Mode = "manual"; p.Layout.RootGiB = 40 }, "layout", "need"},
		{"esp size", func(p *Plan) { p.Layout.Mode = "manual"; p.Layout.ESPMiB = 50 }, "layout.esp_mib", "200 to 4096"},
		{"unlock method", func(p *Plan) { p.Encryption.Unlock = "fido2" }, "encryption.unlock", "must be"},
		{"pcrs", func(p *Plan) { p.Encryption.TPM2PCRs = []int{7, 11} }, "encryption.tpm2_pcrs", "only [7]"},
		{"tang url", func(p *Plan) { p.Encryption.Unlock = "tang" }, "encryption.tang.url", "required"},
		{"tang without tang", func(p *Plan) { p.Encryption.Tang.URL = "http://tang" }, "encryption.tang", "only for unlock"},
		{"short passphrase", func(p *Plan) { p.Encryption.Passphrase = "short" }, "encryption.passphrase", "12"},
		{"settings without encryption", func(p *Plan) { p.Encryption.Enabled = Bool(false); p.Encryption.StoreRecoveryKey = true }, "encryption", "encryption is off"},
		{"profile", func(p *Plan) { p.Profile = "huge" }, "profile", "must be"},
		{"hostname", func(p *Plan) { p.Hostname = "Bad_Host" }, "hostname", "invalid"},
		{"timezone traversal", func(p *Plan) { p.Timezone = "../../etc/passwd" }, "timezone", "invalid"},
		{"ssh key", func(p *Plan) { p.Accounts.Root.SSHKeys = []string{"not-a-key"} }, "accounts.root.ssh_keys", "OpenSSH"},
		{"no admin", func(p *Plan) { p.Accounts.Root.SSHKeys = nil }, "accounts", "no administrator"},
		{"bad hash", func(p *Plan) { p.Accounts.Root.PasswordHash = "plaintext" }, "accounts.root.password_hash", "crypt"},
		{"password and hash", func(p *Plan) {
			p.Accounts.Root.Password = "longenough"
			p.Accounts.Root.PasswordHash = "$6$abc$def"
		}, "accounts.root.password", "not both"},
		{"reserved user", func(p *Plan) { p.Accounts.User = &User{Name: "daemon", Password: "longenough"} }, "accounts.user.name", "system account"},
		{"user without credentials", func(p *Plan) { p.Accounts.User = &User{Name: "ana"} }, "accounts.user", "neither"},
		{"static without address", func(p *Plan) { p.Network.Mode = "static"; p.Network.Interface = "enp1s0" }, "network.address", "prefix"},
		{"dhcp with address", func(p *Plan) { p.Network.Address = "192.0.2.10/24" }, "network", "only for mode static"},
		{"repo url", func(p *Plan) { p.Repos.Basalt.URL = "ftp://x" }, "repos.basalt.url", "media"},
		{"package name", func(p *Plan) { p.Packages.Extra = []string{"vim; rm -rf /"} }, "packages.extra", "invalid"},
		{"finish", func(p *Plan) { p.Finish = "halt-and-catch-fire" }, "finish", "must be"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := valid()
			c.change(&p)
			is := Validate(p, facts(), "")
			if !hasIssue(is, Error, c.field, c.substr) {
				t.Fatalf("expected an error on %s containing %q, got:\n%v", c.field, c.substr, is)
			}
		})
	}
}

func TestValidationWarnings(t *testing.T) {
	f := facts()
	f.TPM2 = false
	is := Validate(valid(), f, "")
	if is.Err() != nil {
		t.Fatalf("no TPM is not an error for unlock tpm2: %v", is.Err())
	}
	if !hasIssue(is, Warning, "encryption.unlock", "recovery key is the only way") {
		t.Fatalf("expected the no-TPM warning, got %v", is)
	}
	p := valid()
	p.Encryption.Unlock = "tpm2+tang"
	p.Encryption.Tang.URL = "http://tang.example:7500"
	if !hasIssue(Validate(p, f, ""), Error, "encryption.unlock", "needs a TPM") {
		t.Fatal("tpm2+tang without a TPM must be an error")
	}
	p = valid()
	p.Encryption.Enabled = Bool(false)
	if !hasIssue(Validate(p, facts(), ""), Warning, "encryption.enabled", "not encrypted") {
		t.Fatal("expected a warning for an unencrypted disk")
	}
}

func TestMachineChecks(t *testing.T) {
	f := facts()
	f.UEFI = false
	if !hasIssue(Validate(valid(), f, ""), Error, "firmware", "UEFI") {
		t.Fatal("BIOS boot must be rejected")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte("apiVersion: basalt-install-plan/v1\ntarget:\n  disk: /dev/vda\n  wipe: true\nencrypton: {}\n"), ".yaml"); err == nil {
		t.Fatal("a typo in a field name must be an error")
	}
	if _, err := Parse([]byte(`{"apiVersion":"basalt-install-plan/v1","target":{"disk":"/dev/vda"},"hostnme":"x"}`), ".json"); err == nil {
		t.Fatal("a typo in a JSON field name must be an error")
	}
}

func TestParseYAMLAndJSONAgree(t *testing.T) {
	y, err := Parse([]byte(`apiVersion: basalt-install-plan/v1
target: {disk: /dev/vda, wipe: true}
encryption: {unlock: tpm2}
accounts:
  root:
    ssh_keys: ["`+key+`"]
repos:
  basalt: {url: "http://10.0.2.2:8098"}
`), ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	j, err := Parse([]byte(y.JSON()), ".json")
	if err != nil {
		t.Fatal(err)
	}
	if y.JSON() != j.JSON() {
		t.Fatalf("round trip differs:\n%s\n%s", y.JSON(), j.JSON())
	}
	if err := Validate(y, facts(), "").Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRedacted(t *testing.T) {
	p := valid()
	p.Accounts.Root.Password = "rootsecret1"
	p.Encryption.Passphrase = "a very long passphrase"
	p.Accounts.User = &User{Name: "ana", PasswordHash: "$6$salt$hash"}
	out := p.Redacted().JSON()
	for _, s := range []string{"rootsecret1", "a very long passphrase", "$6$salt$hash"} {
		if strings.Contains(out, s) {
			t.Fatalf("redacted plan still contains %q", s)
		}
	}
	if p.Accounts.Root.Password != "rootsecret1" || p.Accounts.User.PasswordHash != "$6$salt$hash" {
		t.Fatal("Redacted must not change the original plan")
	}
}
