package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

// The demo mode runs every frontend against a made-up machine and runs no
// command at all, so the wizard, the preview and the progress can be tried
// (and screenshotted) anywhere.

const demoLsblk = `{"blockdevices":[
 {"name":"nvme0n1","path":"/dev/nvme0n1","size":512110190592,"type":"disk","rm":false,"ro":false,"rota":false,"model":"Example NVMe SSD 512GB","serial":"D1","tran":"nvme","fstype":null,"mountpoints":[null],"label":null},
 {"name":"sda","path":"/dev/sda","size":2000398934016,"type":"disk","rm":false,"ro":false,"rota":true,"model":"Example HDD 2TB","serial":"D2","tran":"sata","fstype":null,"mountpoints":[null],"label":null,
  "children":[{"name":"sda1","path":"/dev/sda1","size":2000397885440,"type":"part","rm":false,"ro":false,"rota":true,"model":null,"serial":null,"tran":"sata","fstype":"linux_raid_member","mountpoints":[null],"label":"old:0"}]},
 {"name":"sdb","path":"/dev/sdb","size":15376318464,"type":"disk","rm":true,"ro":false,"rota":false,"model":"USB stick","serial":"D3","tran":"usb","fstype":"iso9660","mountpoints":["/run/basalt/media"],"label":"BASALT-INST"},
 {"name":"sdc","path":"/dev/sdc","size":8012345344,"type":"disk","rm":true,"ro":false,"rota":false,"model":"Key stick","serial":"D4","tran":"usb","fstype":null,"mountpoints":[null],"label":null,
  "children":[{"name":"sdc1","path":"/dev/sdc1","size":8011296768,"type":"part","rm":true,"ro":false,"rota":false,"model":null,"serial":null,"tran":"usb","fstype":"vfat","mountpoints":[null],"label":"KEYS"}]}]}`

func demoProber() probe.Prober {
	root, _ := os.MkdirTemp("", "basalt-installer-demo-machine-")
	files := map[string]string{
		"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c": "\x06\x00\x00\x00\x01",
		"sys/class/tpm/tpm0/tpm_version_major": "2\n",
		"etc/os-release":                       "VERSION_ID=44\n",
		"proc/meminfo":                         "MemTotal: 16316412 kB\n",
		"sys/class/net/enp3s0/type":            "1\n",
	}
	for name, content := range files {
		_ = os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755)
		_ = os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
	}
	return probe.Prober{Root: root, Read: func(_ context.Context, argv ...string) (string, error) {
		if argv[0] == "lsblk" {
			return demoLsblk, nil
		}
		return "none\n", nil
	}}
}

func demoDNF() []string {
	pkgs := []string{"filesystem", "glibc", "systemd", "kernel-core", "selinux-policy-targeted", "shim-x64", "grub2-efi-x64",
		"basalt-release", "basalt-logos", "basalt-snapshots", "basalt-security", "basalt-resolver", "basalt-ledger", "basalt-prompt", "basalt-assistant", "snapper", "cryptsetup"}
	var out []string
	for i, p := range pkgs {
		out = append(out, fmt.Sprintf("[%2d/%d] %s 100%%", i+1, len(pkgs), p))
	}
	for i, p := range pkgs {
		out = append(out, fmt.Sprintf("[%2d/%d] Installing %s", i+1, len(pkgs), p))
	}
	return out
}

// demoRunner creates the directories the real commands would create (all
// inside the demo's temporary root), so the file writes succeed; everything
// else is recorded and not run.
type demoRunner struct {
	root string
	fake engine.FakeRunner
}

func (d *demoRunner) Run(ctx context.Context, s steps.Step, out func(string)) (string, error) {
	if len(s.Argv) > 0 && (s.Argv[0] == "mkdir" || (s.Argv[0] == "install" && len(s.Argv) > 1 && s.Argv[1] == "-d")) {
		for _, a := range s.Argv[1:] {
			if strings.HasPrefix(a, d.root) {
				_ = os.MkdirAll(a, 0o755)
			}
		}
	}
	// Directories created by packages in a real install.
	if len(s.Argv) > 0 && s.Argv[0] == "dnf" {
		for _, dir := range []string{"etc/selinux", "etc/yum.repos.d", "etc/pki/rpm-gpg", "etc/ssh/sshd_config.d", "etc/NetworkManager/system-connections", "root"} {
			_ = os.MkdirAll(filepath.Join(d.root, "sysroot", dir), 0o755)
		}
	}
	return d.fake.Run(ctx, s, out)
}
