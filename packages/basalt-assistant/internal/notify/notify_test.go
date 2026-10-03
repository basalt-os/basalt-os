package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func entry(unit, typ string, data map[string]any) []byte {
	d, _ := json.Marshal(data)
	b, _ := json.Marshal(map[string]string{
		"__CURSOR": "s=abc;i=1", "__REALTIME_TIMESTAMP": "1791000000000000", "_HOSTNAME": "basalt",
		"_SYSTEMD_UNIT": unit, "BASALT_AUDIT_TYPE": typ, "BASALT_AUDIT_SEQ": "42",
		"BASALT_AUDIT_DATA": string(d), "MESSAGE": "basalt-assistant audit #42 finding: p-1: nginx.service failed",
	})
	return b
}

func TestParseEntry(t *testing.T) {
	data := map[string]any{"proposal": "p-1", "kind": "unit", "subject": "nginx.service", "severity": 4.2049, "notify": true}
	ev, ok, err := ParseEntry(entry(DaemonUnit, "finding", data))
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if ev.Seq != 42 || ev.Proposal != "p-1" || ev.Title != "nginx.service failed" || !ev.Notify || ev.Severity != 4.2 ||
		ev.Host != "basalt" || ev.Cursor != "s=abc;i=1" || ev.Time.IsZero() {
		t.Fatalf("%+v", ev)
	}
	// Another unit (or logger(1) from a user) is not a finding, whatever it claims.
	if _, ok, _ := ParseEntry(entry("user@1000.service", "finding", data)); ok {
		t.Fatal("finding from another unit accepted")
	}
	if _, ok, _ := ParseEntry(entry(DaemonUnit, "decision", data)); ok {
		t.Fatal("non-finding accepted")
	}
}

func TestCheckURL(t *testing.T) {
	for _, u := range []string{"https://hooks.example.org/x", "http://127.0.0.1:9000/h", "http://localhost/h", "http://[::1]/h"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://hooks.example.org/x", "ftp://x/y", "https://", "hooks.example.org"} {
		if err := CheckURL(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestReadSecret(t *testing.T) {
	p := filepath.Join(t.TempDir(), "webhook.key")
	_ = os.WriteFile(p, []byte("0123456789abcdef0123\n"), 0o644)
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("world-readable key accepted")
	}
	_ = os.Chmod(p, 0o600)
	if k, err := ReadSecret(p); err != nil || string(k) != "0123456789abcdef0123" {
		t.Fatalf("%q %v", k, err)
	}
	_ = os.WriteFile(p, []byte("short"), 0o600)
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestWebhookSignedAndRetried(t *testing.T) {
	secret := []byte("0123456789abcdef0123")
	var calls atomic.Int32
	var got Payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		ts := r.Header.Get("X-Basalt-Timestamp")
		if r.Header.Get("X-Basalt-Signature") != Sign(secret, ts, body) || r.Header.Get("X-Basalt-Event") != "finding" {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		if n == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	ev := Event{Seq: 7, Proposal: "p-7", Title: "disk almost full", Severity: 3, Notify: true}
	w := Webhook{URL: srv.URL, Secret: secret, Backoff: time.Millisecond, Version: "test"}
	if err := w.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || got.Event != "finding" || got.Finding.Proposal != "p-7" || got.Hint != "basalt show p-7" {
		t.Fatalf("calls %d, payload %+v", calls.Load(), got)
	}
	// A wrong key is refused by the receiver and not retried (4xx).
	calls.Store(1)
	w.Secret = []byte("another-key-of-20-bytes")
	if err := w.Send(context.Background(), ev); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong key: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("4xx retried: %d calls", calls.Load())
	}
}

func TestSignKnownValue(t *testing.T) {
	// printf '1700000000.{}' | openssl dgst -sha256 -hmac key
	want := "sha256=9d713ed406bb7076d4123f0dc2c39d2df5c654ed4b0cd56b52c8b4c940bd63ae"
	if got := Sign([]byte("key"), "1700000000", []byte("{}")); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestGraphicalSessions(t *testing.T) {
	props := map[string]map[string]string{
		"1": ParseProperties("Type=wayland\nClass=user\nState=active\nRemote=no\nUser=1000\nName=ana\n"),
		"2": ParseProperties("Type=tty\nClass=user\nState=active\nRemote=no\nUser=1001\nName=bob\n"),
		"3": ParseProperties("Type=x11\nClass=user\nState=online\nRemote=yes\nUser=1002\nName=cid\n"),
		"4": ParseProperties("Type=wayland\nClass=greeter\nState=active\nRemote=no\nUser=980\nName=gdm\n"),
		"5": ParseProperties("Type=x11\nClass=user\nState=closing\nRemote=no\nUser=1003\nName=dee\n"),
	}
	s := GraphicalSessions(props)
	if len(s) != 1 || s[0].UID != 1000 || s[0].User != "ana" {
		t.Fatalf("%+v", s)
	}
	if len(GraphicalSessions(nil)) != 0 {
		t.Fatal("sessions out of nothing")
	}
}

func TestBusctlArgs(t *testing.T) {
	a := BusctlArgs(Event{Proposal: "p-1", Title: "nginx.service failed", Subject: "nginx.service", Severity: 4.5})
	joined := strings.Join(a, "|")
	if !strings.Contains(joined, "Notify|susssasa{sv}i|Basalt OS|0|dialog-warning|Basalt OS: nginx.service failed|") ||
		!strings.HasSuffix(joined, "|0|1|urgency|y|2|-1") || !strings.Contains(joined, "basalt show p-1") {
		t.Fatalf("%s", joined)
	}
	if a := BusctlArgs(Event{Severity: 2}); a[len(a)-2] != "1" {
		t.Fatalf("normal urgency: %v", a)
	}
}
