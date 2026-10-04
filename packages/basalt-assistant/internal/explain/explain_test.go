package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
)

func nginxFacts() (*Facts, []action.Action) {
	f := New("unit", "config_error", "nginx.service")
	f.Set("file", "/etc/nginx/nginx.conf").Set("line", "47").Set("checker", "nginx -t").
		Set("checker_msg", `unknown directive "bogus_directive" in /etc/nginx/nginx.conf:47`).
		Set("snapshot", "27").Set("snapshot_date", "2026-10-03 18:20:01").Set("restore_path", "/etc/nginx/nginx.conf")
	acts := []action.Action{{Kind: action.FileRestore, Params: map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "27"}},
		{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}}}
	return f, acts
}

func TestCheck(t *testing.T) {
	f, acts := nginxFacts()
	al := NewAllowed(f, acts)
	ok := []string{
		"nginx is down because /etc/nginx/nginx.conf has a typo on line 47. Applying the fix puts back the copy from snapshot 27 and restarts nginx.service.",
		"Your web server, nginx.service, stopped: line 47 of /etc/nginx/nginx.conf has an unknown directive. The snapshot copy from 2026-10-03 fixes it.",
		"nginx.service won't start because of a bad line in its config file in /etc/nginx.",
	}
	for _, s := range ok {
		if bad := al.Check(s, DefaultMaxChars); len(bad) > 0 {
			t.Errorf("rejected a faithful text: %v\n%s", bad, s)
		}
	}
	for s, want := range map[string]string{
		"nginx.service failed on line 48 of /etc/nginx/nginx.conf.":               `number "48"`,
		"nginx.service failed; restore snapshot 26.":                              `number "26"`,
		"nginx.service failed; restore it from snapshot twenty-six.":              `number "six"`,
		"nginx.service failed because /etc/nginx/conf.d/site.conf is broken.":     `path "/etc/nginx/conf.d/site.conf"`,
		"nginx.service failed; httpd.service holds the port.":                     `unit "httpd.service"`,
		"nginx.service failed; run systemctl restart nginx.":                      `command "systemctl"`,
		"nginx.service failed. Run $(nginx -t) to see.":                           "markup or command characters",
		"nginx.service failed because site.conf is broken.":                       `file "site.conf"`,
		"nginx.service failed. Run `systemctl restart nginx` now.":                `command "systemctl"`,
		"nginx.service failed; label it httpd_sys_content_t.":                     `SELinux type "httpd_sys_content_t"`,
		"nginx.service failed; turn on httpd_can_network_connect.":                `name "httpd_can_network_connect"`,
		"nginx.service failed. You could disable SELinux to get it going.":        "forbidden advice",
		"nginx.service failed, see https://example.org/help.":                     "address",
		"nginx.service failed; proposal p-abcdef fixes it.":                       `proposal id "p-abcdef"`,
		"The web server failed because of a configuration error.":                 `does not name "nginx.service"`,
		"nginx.service 失败了.":                                                      "non-Latin character",
		strings.Repeat("nginx.service failed because of the configuration. ", 20): "longer than",
		"": "empty text",
		"nginx.service failed; snapper shows the old copy.":                                     `command "snapper"`,
		"nginx.service failed after the upgrade to version 1.26 of the package on the 3rd day.": `number "1.26"`,
		"nginx.service is down; I already rolled it back for you via basalt-rollback --yes 27.": `command "basalt-rollback"`,
	} {
		bad := al.Check(s, DefaultMaxChars)
		found := false
		for _, b := range bad {
			if strings.Contains(b, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: want a problem containing %q, got %v", s, want, bad)
		}
	}
}

// The templates (default wording and every variant) pass the check.
func TestVariantsAreFaithful(t *testing.T) {
	f, acts := nginxFacts()
	r := rand.New(rand.NewSource(1))
	al := NewAllowed(f, acts)
	for i := 0; i < 50; i++ {
		txt := WriteVariant(f, acts, r).Prose()
		if bad := al.Check(txt, 0); len(bad) > 0 {
			t.Fatalf("variant fails: %v\n%s", bad, txt)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"db error: password=hunter2 at startup":                                 "db error: password=[redacted] at startup",
		"Authorization: Bearer abcdefghijklmnop":                                "Authorization: [redacted]",
		"key AKIA1234567890ABCDEFGHIJ1234567890XYZ leaked":                      "key [redacted] leaked",
		"cannot open /home/alice/site/index.html":                               "cannot open /home/[redacted]/site/index.html",
		"connect to 203.0.113.4:5432 failed":                                    "connect to [redacted]:5432 failed",
		"mail from bob@example.org rejected":                                    "mail from [redacted] rejected",
		"19:05:37 nginx: [emerg] unknown directive in /etc/nginx/nginx.conf:47": "19:05:37 nginx: [emerg] unknown directive in /etc/nginx/nginx.conf:47",
		"/var/lib/basalt-assistant/proposals/p-1a2b3c.json":                     "/var/lib/basalt-assistant/proposals/p-1a2b3c.json",
		"httpd_can_network_connect_db is off":                                   "httpd_can_network_connect_db is off",
	} {
		if got := RedactString(in); got != want {
			t.Errorf("RedactString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRiskAndUndo(t *testing.T) {
	restart := action.Action{Kind: action.UnitRestart, Params: map[string]string{"unit": "a.service"}}
	del := action.Action{Kind: action.SnapshotDelete, Params: map[string]string{"snapshot": "4"}}
	if l, _ := Risk([]action.Action{restart}); l != RiskLow {
		t.Errorf("restart risk %d", l)
	}
	if l, why := Risk([]action.Action{restart, del}); l != RiskHigh || !strings.Contains(why, "snapshot 4") {
		t.Errorf("delete risk %d %q", l, why)
	}
	if u, cmd := Undo([]action.Action{del}); cmd || !strings.Contains(u, "cannot be undone") {
		t.Errorf("undo of a delete: %q %v", u, cmd)
	}
	fc := action.Action{Kind: action.SELinuxFcontext, Params: map[string]string{"path": "/srv/www", "type": "httpd_sys_content_t"}}
	if u, cmd := Undo([]action.Action{fc}); !cmd || !strings.Contains(u, "/srv") {
		t.Errorf("undo of a relabel under /srv: %q %v", u, cmd)
	}
}

// mockServer is an OpenAI-compatible endpoint answering with a fixed text,
// as one JSON response or as a stream of server-sent events. It records the
// requests it gets.
type mockServer struct {
	mu     sync.Mutex
	answer string
	bodies []map[string]any
	auth   []string
}

func (m *mockServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"some-model"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		m.mu.Unlock()
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, piece := range strings.SplitAfter(m.answer, " ") {
				b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": piece}}}})
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": m.answer}}}})
		_, _ = w.Write(b)
	})
}

func TestHumanizeMockEndpoint(t *testing.T) {
	m := &mockServer{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	f, acts := nginxFacts()
	tmpl := Write(f, acts).Prose()
	h := &Humanizer{C: &llm.Client{Endpoint: srv.URL + "/v1", Timeout: 5 * time.Second}}
	ctx := context.Background()

	// A faithful answer is accepted, as a whole and streamed.
	m.answer = "nginx.service stopped because line 47 of /etc/nginx/nginx.conf has an unknown directive. Applying the fix puts back the copy from snapshot 27 and restarts nginx."
	res := h.Write(ctx, f, acts, tmpl, nil)
	if !res.Accepted || res.Text != m.answer {
		t.Fatalf("faithful text not accepted: %+v", res)
	}
	var got []string
	res = h.Write(ctx, f, acts, tmpl, func(s string) { got = append(got, s) })
	if !res.Accepted || !res.Streamed || len(got) != 2 || res.Text != m.answer {
		t.Fatalf("stream: %+v %q", res, got)
	}

	// An invented value falls back to the template; streaming stops at the
	// first bad sentence (the first one was shown, the second never is).
	m.answer = "nginx.service stopped because of a mistake in /etc/nginx/nginx.conf. Run systemctl restart nginx after editing line 52. Then it works."
	got = nil
	res = h.Write(ctx, f, acts, tmpl, func(s string) { got = append(got, s) })
	if res.Accepted || res.Text != tmpl || len(got) != 1 || len(res.Problems) == 0 {
		t.Fatalf("unfaithful text accepted or streamed: %+v %q", res, got)
	}
	res = h.Write(ctx, f, acts, tmpl, nil)
	if res.Accepted || res.Text != tmpl {
		t.Fatalf("unfaithful text accepted: %+v", res)
	}

	// The local server gets llama.cpp's options; no API key is sent.
	b := m.bodies[0]
	if b["chat_template_kwargs"] == nil || m.auth[0] != "" {
		t.Errorf("local request: %v auth %q", b, m.auth[0])
	}

	// A model that is down: the template, with the error.
	down := &Humanizer{C: &llm.Client{Endpoint: "http://127.0.0.1:1/v1", Timeout: time.Second}}
	if res := down.Write(ctx, f, acts, tmpl, nil); res.Accepted || res.Err == nil || res.Text != tmpl {
		t.Fatalf("unreachable model: %+v", res)
	}
}

// toMock sends every request to the mock server, whatever the host.
type toMock struct{ target string }

func (t toMock) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = "http", t.target
	return http.DefaultTransport.RoundTrip(r2)
}

// A remote provider (a hosted API, opt-in): the facts are redacted before
// they are sent, the request is shown before it leaves, the key goes in a
// header only, llama.cpp's options are left out, and the same slots, check
// and fallback apply.
func TestHumanizeRemoteProvider(t *testing.T) {
	m := &mockServer{}
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := &llm.Client{Endpoint: "https://api.provider.example/v1", Model: "big-model", AllowRemote: true, APIKey: "sk-test-key",
		Timeout: 5 * time.Second, Transport: toMock{strings.TrimPrefix(srv.URL, "http://")}}
	if !c.Remote() {
		t.Fatal("endpoint not remote")
	}
	f := New("unit", "missing_file", "app.service")
	f.Set("journal", "open /home/alice/app/config.yml failed: token=s3cr3t-T0KEN-value (db 198.51.100.5)")
	var shownTo string
	var shown []byte
	h := &Humanizer{C: c, Sent: func(ep string, b []byte) { shownTo, shown = ep, b }}
	tmpl := Write(f, nil).Prose()

	m.answer = "app.service stopped because a file it reads is missing under /home/[redacted]/app."
	res := h.Write(context.Background(), f, nil, tmpl, nil)
	if res.Accepted || res.Text != tmpl || res.Redacted == 0 {
		t.Fatalf("a text repeating a redacted value was accepted: %+v", res)
	}
	m.answer = "app.service stopped because a configuration file it needs is missing. Put the file back and start it again."
	res = h.Write(context.Background(), f, nil, tmpl, nil)
	if !res.Accepted || res.Text != m.answer {
		t.Fatalf("faithful remote text: %+v", res)
	}
	sent, _ := json.Marshal(m.bodies[len(m.bodies)-1])
	for _, secret := range []string{"alice", "s3cr3t", "198.51.100.5", "sk-test-key"} {
		if strings.Contains(string(sent), secret) || strings.Contains(string(shown), secret) {
			t.Errorf("%q left the machine: %s", secret, sent)
		}
	}
	if !strings.Contains(string(sent), Redacted) || shownTo != c.Endpoint || !strings.Contains(string(shown), Redacted) {
		t.Errorf("request not shown or not redacted: %s / %s", shownTo, shown)
	}
	if _, ok := m.bodies[0]["chat_template_kwargs"]; ok {
		t.Errorf("llama.cpp options sent to a remote provider: %v", m.bodies[0])
	}
	if m.auth[0] != "Bearer sk-test-key" || m.bodies[0]["model"] != "big-model" {
		t.Errorf("auth %q model %v", m.auth[0], m.bodies[0]["model"])
	}
	// Without the opt-in the client refuses the endpoint: the template.
	c2 := &llm.Client{Endpoint: c.Endpoint, Model: c.Model, Transport: c.Transport}
	h2 := &Humanizer{C: c2}
	if res := h2.Write(context.Background(), f, nil, tmpl, nil); res.Accepted || res.Err == nil {
		t.Fatalf("remote without opt-in: %+v", res)
	}
}
