package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sumHex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// planServer serves basePlan at /plan.yaml and counts the requests.
func planServer(t *testing.T, tls bool, mux *http.ServeMux) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	if mux == nil {
		mux = http.NewServeMux()
	}
	mux.HandleFunc("/plan.yaml", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(basePlan))
	})
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(mux)
		old := planTransport
		planTransport = srv.Client().Transport
		t.Cleanup(func() { planTransport = old })
	} else {
		srv = httptest.NewServer(mux)
	}
	t.Cleanup(srv.Close)
	return srv, hits
}

func mustRefuse(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !isPlanRefused(err) || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal mentioning %q, got %v", want, err)
	}
}

func TestPlanOverPlainHTTPNeedsAChecksum(t *testing.T) {
	srv, hits := planServer(t, false, nil)
	ctx := context.Background()
	_, err := loadPlan(ctx, srv.URL+"/plan.yaml", "")
	mustRefuse(t, err, "basalt.inst.plan.sha256")
	if hits.Load() != 0 {
		t.Fatal("a refused http plan must not even be requested")
	}
	p, err := loadPlan(ctx, srv.URL+"/plan.yaml", sumHex(basePlan))
	if err != nil || p.Target.Disk != "/dev/vda" {
		t.Fatalf("http with the right checksum: %v %+v", err, p.Target)
	}
	// Upper-case hex is the same checksum.
	if _, err := loadPlan(ctx, srv.URL+"/plan.yaml", strings.ToUpper(sumHex(basePlan))); err != nil {
		t.Fatal(err)
	}
	_, err = loadPlan(ctx, srv.URL+"/plan.yaml", sumHex(basePlan+"# changed\n"))
	mustRefuse(t, err, "SHA-256")
}

func TestPlanChecksumMustBeWellFormed(t *testing.T) {
	for _, bad := range []string{"abc", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 66)} {
		_, err := loadPlan(context.Background(), "https://192.0.2.1/plan.yaml", bad)
		mustRefuse(t, err, "64 hexadecimal digits")
	}
}

func TestPlanOverHTTPS(t *testing.T) {
	srv, _ := planServer(t, true, nil)
	p, err := loadPlan(context.Background(), srv.URL+"/plan.yaml", "")
	if err != nil || p.Target.Disk != "/dev/vda" {
		t.Fatalf("https without a checksum: %v", err)
	}
	// A checksum given with https is checked too.
	_, err = loadPlan(context.Background(), srv.URL+"/plan.yaml", sumHex("other"))
	mustRefuse(t, err, "SHA-256")
}

func TestPlanRedirects(t *testing.T) {
	plain, _ := planServer(t, false, nil)
	other, _ := planServer(t, true, nil) // a second https host (another port)
	mux := http.NewServeMux()
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/plan.yaml", http.StatusFound)
	})
	mux.HandleFunc("/to-other", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/plan.yaml", http.StatusFound)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/plan.yaml", http.StatusFound)
	})
	srv, _ := planServer(t, true, mux)
	ctx := context.Background()
	if _, err := loadPlan(ctx, srv.URL+"/same", ""); err != nil {
		t.Fatalf("a redirect on the same https host: %v", err)
	}
	_, err := loadPlan(ctx, srv.URL+"/to-http", "")
	mustRefuse(t, err, "not https")
	_, err = loadPlan(ctx, srv.URL+"/to-other", "")
	mustRefuse(t, err, "another host")
	// With the checksum pinned, the content decides.
	for _, path := range []string{"/to-http", "/to-other"} {
		if _, err := loadPlan(ctx, srv.URL+path, sumHex(basePlan)); err != nil {
			t.Fatalf("%s with the checksum: %v", path, err)
		}
	}
}

func TestPlanSizeLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big.yaml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(basePlan + "#" + strings.Repeat("x", planMaxBytes) + "\n"))
	})
	srv, _ := planServer(t, true, mux)
	_, err := loadPlan(context.Background(), srv.URL+"/big.yaml", "")
	mustRefuse(t, err, "1 MiB")
}

func TestPlanFileChecksum(t *testing.T) {
	f := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(f, []byte(basePlan), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlan(context.Background(), f, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlan(context.Background(), f, sumHex(basePlan)); err != nil {
		t.Fatal(err)
	}
	_, err := loadPlan(context.Background(), f, sumHex("other"))
	mustRefuse(t, err, "SHA-256")
}

func TestSameHostPort(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://Plans.Example/x", "https://plans.example./y", true},
		{"https://plans.example/x", "https://plans.example:443/y", true},
		{"https://plans.example/x", "https://plans.example:8443/y", false},
		{"https://plans.example/x", "https://plans.example.evil.example/y", false},
		{"https://[2001:db8::1]/x", "https://[2001:db8::1]:443/y", true},
	} {
		a, _ := http.NewRequest(http.MethodGet, c.a, nil)
		b, _ := http.NewRequest(http.MethodGet, c.b, nil)
		if got := sameHostPort(a.URL, b.URL); got != c.want {
			t.Errorf("%s vs %s: got %v", c.a, c.b, got)
		}
	}
}

// An unattended install from a plain http plan without a checksum stops at
// once with the reason, without waiting for the network to come up.
func TestUnattendedRefusesPlainHTTPPlan(t *testing.T) {
	srv, hits := planServer(t, false, nil)
	r := newRig(t, basePlan, "basalt.inst.plan="+srv.URL+"/plan.yaml basalt.inst.confirm=vda")
	start := time.Now()
	r.ss.RunUnattended(context.Background(), time.Minute)
	a := r.ss.Status().Unattended
	if a.State != AutoRefused || !strings.Contains(a.Error, "was refused") || !strings.Contains(a.Error, "basalt.inst.plan.sha256") {
		t.Fatalf("status: %+v", a)
	}
	if time.Since(start) > 30*time.Second || hits.Load() != 0 || r.ss.Status().State != Idle {
		t.Fatal("the refusal must be immediate, without a request or an installation")
	}
	// With the checksum on the boot line the same plan installs.
	r = newRig(t, basePlan, "basalt.inst.plan="+srv.URL+"/plan.yaml basalt.inst.plan.sha256="+sumHex(basePlan)+" basalt.inst.confirm=vda")
	if a := r.run(t); a.State != AutoRunning {
		t.Fatalf("with the checksum: %+v", a)
	}
}
