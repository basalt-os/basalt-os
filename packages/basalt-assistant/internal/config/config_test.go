package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	p := t.TempDir() + "/assistant.conf"
	_ = os.WriteFile(p, []byte("[decision]\nbackend = rules\ndefault_threshold = 0.8\n[thresholds]\navc.class = 0.9\n[disk]\nwarn_pct = 80\nsnapshots_large = 2G\n[events]\ndedup_window = 30m\n"), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultThreshold != 0.8 || c.Thresholds["avc.class"] != 0.9 || c.Disk.WarnPct != 80 ||
		c.Disk.SnapshotsLargeBytes != 2<<30 || c.DedupWindow != 30*time.Minute || c.Disk.CritPct != 95 {
		t.Fatalf("%+v", c)
	}
	_ = os.WriteFile(p, []byte("[thresholds]\nx = 2\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Error("threshold above 1 accepted")
	}
	_ = os.WriteFile(p, []byte("[decision]\nbackend = cloud\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Error("unknown backend accepted")
	}
	if c, err := Load(p + ".missing"); err != nil || c.Backend != "rules" {
		t.Error("missing file must give defaults")
	}
}
