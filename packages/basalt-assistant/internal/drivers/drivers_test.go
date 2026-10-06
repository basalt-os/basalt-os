package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

type dev struct {
	slot, class, vendor, device, driver string
	boot                                bool
}

// fakeSys builds a sysfs tree with the given PCI devices.
func fakeSys(t *testing.T, devs []dev, files map[string]string, answers map[string]runner.Result) Sys {
	t.Helper()
	root := t.TempDir()
	for _, d := range devs {
		p := filepath.Join(root, "sys/bus/pci/devices", d.slot)
		must(t, os.MkdirAll(p, 0o755))
		must(t, os.WriteFile(filepath.Join(p, "class"), []byte(d.class+"\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(p, "vendor"), []byte("0x"+d.vendor+"\n"), 0o644))
		must(t, os.WriteFile(filepath.Join(p, "device"), []byte("0x"+d.device+"\n"), 0o644))
		b := "0"
		if d.boot {
			b = "1"
		}
		must(t, os.WriteFile(filepath.Join(p, "boot_vga"), []byte(b+"\n"), 0o644))
		if d.driver != "" {
			must(t, os.MkdirAll(filepath.Join(root, "sys/bus/pci/drivers", d.driver), 0o755))
			must(t, os.Symlink("../../../bus/pci/drivers/"+d.driver, filepath.Join(p, "driver")))
		}
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o644))
	}
	ids := "8086  Intel Corporation\n\ta7a0  Raptor Lake-P [Iris Xe Graphics]\n1af4  Red Hat, Inc.\n\t1050  Virtio 1.0 GPU\n"
	must(t, os.MkdirAll(filepath.Join(root, "usr/share/hwdata"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, PCIIDs), []byte(ids), 0o644))
	return Sys{Root: root, R: &runner.Fake{Answers: answers, Default: &runner.Result{Code: 1}}, ReadFile: os.ReadFile,
		Kernel: func() string { return "7.2.8-200.fc44.x86_64" }, Graphical: func() bool { return true }}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var (
	rtx4060Laptop = dev{"0000:01:00.0", "0x030200", "10de", "28a0", "nouveau", false}
	intelIGPU     = dev{"0000:00:02.0", "0x030000", "8086", "a7a0", "i915", true}
	gtx1070       = dev{"0000:03:00.0", "0x030000", "10de", "1b81", "nouveau", true}
	gt710         = dev{"0000:03:00.0", "0x030000", "10de", "128b", "nouveau", true}
	rtx4090       = dev{"0000:01:00.0", "0x030000", "10de", "2684", "nouveau", true}
	virtioGPU     = dev{"0000:00:01.0", "0x030000", "1af4", "1050", "virtio-pci", true}
)

func TestLicenseAndTable(t *testing.T) {
	if LicenseSHA256() != action.NvidiaLicenseSHA256 {
		t.Fatalf("the embedded Agreement %s differs from the pinned one %s", LicenseSHA256(), action.NvidiaLicenseSHA256)
	}
	if !strings.Contains(NvidiaLicense, "NVIDIA") || !strings.Contains(NvidiaLicense, "2.8") {
		t.Error("the embedded license text is not the Agreement")
	}
	if DriverVersion() != "595.104.02" {
		t.Errorf("table for driver %s", DriverVersion())
	}
	for dev, want := range map[string]string{"28a0": Supported, "2684": Supported, "1b81": Legacy580, "1d81": Legacy580, "128b": Unsupported, "ffff": Unknown} {
		if got, _, _ := classify(dev); got != want {
			t.Errorf("%s: %s, want %s", dev, got, want)
		}
	}
}

func TestHybridLaptop(t *testing.T) {
	s := fakeSys(t, []dev{intelIGPU, rtx4060Laptop}, nil, map[string]runner.Result{
		"rpm -q --qf '%{NAME} %{VERSION}\n' nvidia-driver nvidia-driver-compute basalt-nonfree-release": {Code: 1, Out: "package nvidia-driver is not installed\n"},
	})
	r := Build(context.Background(), s, true)
	if len(r.GPUs) != 2 {
		t.Fatalf("gpus: %+v", r.GPUs)
	}
	nv := r.GPUs[1]
	if nv.Name != "NVIDIA GeForce RTX 4060 Laptop GPU" || nv.Support != Supported || !nv.GeForce || nv.Driver != "nouveau" || nv.Class != "3d" {
		t.Errorf("nvidia: %+v", nv)
	}
	if r.GPUs[0].Name != "Intel Corporation Raptor Lake-P [Iris Xe Graphics]" {
		t.Errorf("intel name %q", r.GPUs[0].Name)
	}
	rec := r.Recommendation
	if rec.Action != "install" || rec.Variant != "display" || !rec.Hybrid || !strings.Contains(rec.Primary, "Iris Xe") {
		t.Fatalf("recommendation: %+v", rec)
	}
	if strings.Join(rec.Packages, " ") != "basalt-nonfree-release nvidia-driver kmod-nvidia-open-7.2.8-200.fc44.x86_64" {
		t.Errorf("packages %v", rec.Packages)
	}
	if !strings.Contains(strings.Join(rec.Changes, " "), "PRIME offload") {
		t.Error("hybrid change not explained")
	}
	if len(r.Notices) != 2 || !strings.Contains(r.Notices[1], "datacenter") || !strings.Contains(r.Notices[0], Trademark) {
		t.Errorf("notices %v", r.Notices)
	}
	if r.License.Text == "" || r.License.SHA256 != action.NvidiaLicenseSHA256 {
		t.Error("license missing")
	}

	p, err := InstallProposal(r, "cli", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	lines, _ := p.Commands()
	want := []string{"dnf -y install basalt-nonfree-release", "dnf config-manager setopt basalt-nonfree.enabled=1",
		"dnf -y install --skip-unavailable nvidia-driver kmod-nvidia-open-7.2.8-200.fc44.x86_64", "basalt-nvidia arm"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands:\n%s", strings.Join(lines, "\n"))
	}
	text := strings.Join(strings.Fields(report.Render(p)), " ")
	for _, w := range []string{"NVIDIA GeForce RTX 4060 Laptop GPU can use the NVIDIA driver 595.104.02 instead of nouveau",
		"stays the display GPU", "Risk: high", "NVIDIA Driver License Agreement", "basalt apply " + p.ID} {
		if !strings.Contains(text, w) {
			t.Errorf("report lacks %q:\n%s", w, text)
		}
	}
	if p.NeedsReview {
		t.Error("a proposal the person asked for at the command line needs no review")
	}
}

func TestDesktopNvidiaOnlyAndServer(t *testing.T) {
	s := fakeSys(t, []dev{rtx4090}, nil, nil)
	r := Build(context.Background(), s, false)
	if r.Recommendation.Hybrid || r.Recommendation.Variant != "display" || r.License.Text != "" {
		t.Errorf("%+v", r.Recommendation)
	}
	s.Graphical = func() bool { return false }
	r = Build(context.Background(), s, false)
	if r.Recommendation.Variant != "compute" || r.Recommendation.Packages[1] != "nvidia-driver-compute" {
		t.Errorf("server: %+v", r.Recommendation)
	}
	p, err := InstallProposal(r, "mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.NeedsReview || p.Actions[0].Params["variant"] != "compute" {
		t.Errorf("mcp proposal: %+v", p)
	}
}

func TestLegacyAndOther(t *testing.T) {
	for _, c := range []struct {
		d      []dev
		action string
		note   string
	}{
		{[]dev{gtx1070}, "guided", "580 legacy driver"},
		{[]dev{gt710}, "unsupported", "470"},
		{[]dev{virtioGPU}, "none", ""},
		{[]dev{{"0000:01:00.0", "0x030000", "10de", "ffff", "", true}}, "unknown", "not in the list"},
	} {
		r := Build(context.Background(), fakeSys(t, c.d, nil, nil), false)
		if r.Recommendation.Action != c.action || !strings.Contains(strings.Join(r.Recommendation.Notes, " "), c.note) {
			t.Errorf("%v: %+v", c.d, r.Recommendation)
		}
		if _, err := InstallProposal(r, "cli", ""); err == nil {
			t.Errorf("%v: install proposed", c.d)
		}
	}
}

func TestSecureBootBlocksWithoutCA(t *testing.T) {
	files := map[string]string{ModuleCA: "x", "sys/firmware/efi/.keep": ""}
	answers := map[string]runner.Result{
		"mokutil --sb-state":                       {Out: "SecureBoot enabled\n"},
		"mokutil --test-key " + ModuleCA:           {Code: 0, Out: ModuleCA + " is not enrolled\n"},
		"openssl x509 -inform DER -in " + ModuleCA: {},
	}
	answers["openssl x509 -inform DER -in "+ModuleCA+" -noout -subject"] = runner.Result{Out: "subject=CN=OpenBasalt Kernel Module CA\n"}
	r := Build(context.Background(), fakeSys(t, []dev{intelIGPU, rtx4060Laptop}, files, answers), false)
	if !r.SecureBoot.Enabled || r.SecureBoot.CAEnrolled || len(r.Recommendation.Blockers) != 1 {
		t.Fatalf("%+v %+v", r.SecureBoot, r.Recommendation)
	}
	if _, err := InstallProposal(r, "cli", ""); err == nil || !strings.Contains(err.Error(), "enroll-mok") {
		t.Errorf("install not blocked: %v", err)
	}
	answers["mokutil --list-new"] = runner.Result{Out: "  Subject: CN=OpenBasalt Kernel Module CA\n"}
	r = Build(context.Background(), fakeSys(t, []dev{intelIGPU, rtx4060Laptop}, files, answers), false)
	if !r.SecureBoot.CAPending || len(r.Recommendation.Blockers) != 0 {
		t.Errorf("pending enrollment should not block: %+v %+v", r.SecureBoot, r.Recommendation)
	}
	answers["mokutil --test-key "+ModuleCA] = runner.Result{Code: 1, Out: ModuleCA + " is already enrolled\n"}
	r = Build(context.Background(), fakeSys(t, []dev{intelIGPU, rtx4060Laptop}, files, answers), false)
	if !r.SecureBoot.CAEnrolled || len(r.Recommendation.Blockers) != 0 {
		t.Errorf("%+v", r.SecureBoot)
	}
}

func TestStateFallback(t *testing.T) {
	files := map[string]string{
		StateFile:                                "mode=fallback\nsince=2026-10-05T10:00:00Z\nreason=nvidia-smi does not answer\nsnapshot=42\nproposal=p-1a2b3c\n",
		BootFile:                                 "driver=nouveau\nreason=persistent fallback\n",
		RepoFile:                                 "[basalt-nonfree]\nenabled=0\n",
		RepoOverride + "/99-config_manager.repo": "[basalt-nonfree]\nenabled=1\n",
	}
	answers := map[string]runner.Result{
		"rpm -q --qf '%{NAME} %{VERSION}\n' nvidia-driver nvidia-driver-compute basalt-nonfree-release": {Out: "nvidia-driver 595.104.02\npackage nvidia-driver-compute is not installed\nbasalt-nonfree-release 1\n"},
	}
	r := Build(context.Background(), fakeSys(t, []dev{intelIGPU, rtx4060Laptop}, files, answers), false)
	st := r.State
	if st.Installed != "nvidia-driver" || st.Mode != "fallback" || st.Snapshot != "42" || st.Proposal != "p-1a2b3c" || !st.RepoEnabled || !st.ReleasePackage || st.ThisBoot != "nouveau" {
		t.Errorf("state %+v", st)
	}
	if r.Recommendation.Action != "fallback" {
		t.Errorf("%+v", r.Recommendation)
	}
	if _, err := InstallProposal(r, "cli", ""); err == nil {
		t.Error("installed twice")
	}
}

func TestActionValidation(t *testing.T) {
	ok := map[string]string{"driver": "nvidia", "variant": "display", "kernel": "7.2.8-200.fc44.x86_64", "license": action.NvidiaLicenseSHA256}
	if err := (action.Action{Kind: action.DriverInstall, Params: ok}).Validate(); err != nil {
		t.Fatal(err)
	}
	for k, bad := range map[string]string{"driver": "amdgpu", "variant": "all", "kernel": "7.2; reboot", "license": strings.Repeat("0", 64)} {
		p := map[string]string{}
		for a, b := range ok {
			p[a] = b
		}
		p[k] = bad
		if err := (action.Action{Kind: action.DriverInstall, Params: p}).Validate(); err == nil {
			t.Errorf("%s=%q accepted", k, bad)
		}
	}
	checks := (action.Action{Kind: action.DriverInstall, Params: ok}).Verify(time.Time{})
	if len(checks) != 5 {
		t.Errorf("%d checks", len(checks))
	}
}
