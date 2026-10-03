package journal

import (
	"os"
	"testing"
)

func load(t *testing.T, name string) []Entry {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return ParseAll(string(b))
}

func TestFailedUnitFromConfigError(t *testing.T) {
	es := load(t, "nginx-config-error.journal.json")
	var units []string
	for _, e := range es {
		if u, ok := e.FailedUnit(); ok {
			units = append(units, u)
		}
	}
	// "Failed with result" and "Failed to start" both name nginx.service;
	// "Deactivated successfully" (same catalog family) must not count.
	if len(units) != 2 || units[0] != "nginx.service" || units[1] != "nginx.service" {
		t.Fatalf("failed units = %v", units)
	}
	if es[0].Time.IsZero() || es[0].Cursor == "" {
		t.Errorf("time or cursor not parsed: %+v", es[0])
	}
}

func TestAVCAndSoftwareUpdate(t *testing.T) {
	avc := load(t, "avc-port-8181.audit.json")
	if len(avc) != 1 || !avc[0].IsAVC() {
		t.Fatalf("AVC record not recognized: %+v", avc)
	}
	failed := load(t, "rpm-install-failed.audit.json")
	sw, ok := failed[0].SoftwareUpdateFailed()
	if !ok || sw != "basalt-lab-broken-1-1.noarch" {
		t.Fatalf("SoftwareUpdateFailed = %q %v", sw, ok)
	}
	for _, e := range load(t, "rpm-update-ok.audit.json") {
		if _, ok := e.SoftwareUpdateFailed(); ok {
			t.Errorf("successful update reported as failed: %s", e.Message)
		}
	}
}

func TestBinaryMessage(t *testing.T) {
	e, ok := ParseLine([]byte(`{"MESSAGE":[104,105,10],"PRIORITY":"3","_PID":"1","UNIT":"x.service","MESSAGE_ID":"d9b373ed55a64feb8242e02dbe79a49c"}`))
	if !ok || e.Message != "hi\n" || e.Priority != 3 {
		t.Fatalf("binary MESSAGE: %+v", e)
	}
	if u, ok := e.FailedUnit(); !ok || u != "x.service" {
		t.Fatalf("FailedUnit = %q %v", u, ok)
	}
}

func TestIsNoSpaceCaseInsensitive(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{`postgres: FATAL:  could not write to file "pg_wal/xlogtemp.12": No space left on device`, true},
		// Go programs print syscall.ENOSPC in lowercase.
		{"redis: error: cannot save snapshot: write /var/lib/redis/dump.rdb: no space left on device", true},
		{"myapp: WRITE FAILED: NO SPACE LEFT ON DEVICE", true},
		{"nginx: [emerg] bind() to 0.0.0.0:80 failed (98: Address already in use)", false},
		{"df: space left on device is 12G", false},
	}
	for _, c := range cases {
		if got := (Entry{Message: c.msg}).IsNoSpace(); got != c.want {
			t.Errorf("IsNoSpace(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
