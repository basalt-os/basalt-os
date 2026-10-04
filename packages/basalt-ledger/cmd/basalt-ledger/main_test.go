package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/sign"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/tpm/tpmsim"
)

func TestParseRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{"365d": 365 * 24 * time.Hour, "8760h": 8760 * time.Hour, "forever": 0, "never": 0, "0": 0} {
		if d, err := parseRetention(in); err != nil || d != want {
			t.Fatalf("%s: %v %v", in, d, err)
		}
	}
	for _, bad := range []string{"-1d", "0d", "a year", "-5h"} {
		if _, err := parseRetention(bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	c, err := loadConfig(filepath.Join(t.TempDir(), "missing.conf"))
	if err != nil || c.ExportKey != "auto" || c.Retention != 365*24*time.Hour || c.RetentionText != "365d" || c.TPMDevice != tpm.DefaultDevice {
		t.Fatalf("%+v %v", c, err)
	}
	p := filepath.Join(t.TempDir(), "ledger.conf")
	_ = os.WriteFile(p, []byte("export_key = software\nretention = forever\ntpm_device = /dev/tpmrm1\n"), 0o644)
	c, err = loadConfig(p)
	if err != nil || c.ExportKey != "software" || c.Retention != 0 || c.RetentionText != "forever" || c.TPMDevice != "/dev/tpmrm1" {
		t.Fatalf("%+v %v", c, err)
	}
	_ = os.WriteFile(p, []byte("export_key = hsm\n"), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("unknown export_key accepted")
	}
}

func TestExportKeyChoice(t *testing.T) {
	logf := func(string, ...any) {}
	old := tpm.OpenFunc
	t.Cleanup(func() { tpm.OpenFunc = old })
	sim := tpmsim.New()
	withTPM := func(string) (io.ReadWriteCloser, error) { return sim, nil }
	noTPM := func(string) (io.ReadWriteCloser, error) { return nil, os.ErrNotExist }

	// auto with a TPM: the TPM key, no software key file created.
	dir := t.TempDir()
	tpm.OpenFunc = withTPM
	k, note, err := exportKey(config{Dir: dir, ExportKey: "auto"}, logf)
	if err != nil || k == nil || k.Kind != sign.KindTPM || note != "" {
		t.Fatalf("auto+tpm: %v %q %v", k, note, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys", "export-ed25519.key")); !os.IsNotExist(err) {
		t.Fatal("software key created although the TPM works")
	}
	pub, err := sign.ReadPublic(filepath.Join(dir, "keys", "export.pub"))
	if err != nil || sign.ID(pub) != sign.ID(k.Public) {
		t.Fatal("export.pub is not the TPM key")
	}
	if _, err := os.Stat(filepath.Join(dir, "keys", "export-"+sign.ID(k.Public)+".pub")); err != nil {
		t.Fatal("per-key public file missing")
	}
	tpmID := sign.ID(k.Public)

	// auto without a TPM: the software key, labeled, and export.pub follows.
	tpm.OpenFunc = noTPM
	k, note, err = exportKey(config{Dir: dir, ExportKey: "auto"}, logf)
	if err != nil || k == nil || k.Kind != sign.KindSoftware || note == "" {
		t.Fatalf("auto without tpm: %v %q %v", k, note, err)
	}
	pub, _ = sign.ReadPublic(filepath.Join(dir, "keys", "export.pub"))
	if sign.ID(pub) != sign.ID(k.Public) {
		t.Fatal("export.pub is not the software key")
	}
	// The earlier TPM key's public file stays for old exports.
	if _, err := os.Stat(filepath.Join(dir, "keys", "export-"+tpmID+".pub")); err != nil {
		t.Fatal("old key's public file removed")
	}

	// tpm required and missing: no key, exports refused, no export.pub.
	k, note, err = exportKey(config{Dir: dir, ExportKey: "tpm"}, logf)
	if err != nil || k != nil || note == "" {
		t.Fatalf("tpm required: %v %q %v", k, note, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys", "export.pub")); !os.IsNotExist(err) {
		t.Fatal("export.pub left without a key")
	}

	// software chosen even with a TPM.
	tpm.OpenFunc = withTPM
	k, _, err = exportKey(config{Dir: dir, ExportKey: "software"}, logf)
	if err != nil || k.Kind != sign.KindSoftware {
		t.Fatalf("software: %v %v", k, err)
	}
	if sim.Err != nil {
		t.Fatal(sim.Err)
	}
}
