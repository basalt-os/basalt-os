package selinux

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// helperRunner answers basalt-policy-query from a table of sesearch/seinfo
// outputs (as the real helper would) and anything else from a Fake.
type helperRunner struct {
	answers map[string]string // joined argv -> output
	errs    map[string]string
	busy    int // the first busy calls report EBUSY
	batches [][]string
	direct  []string
}

func (h *helperRunner) Read(_ context.Context, argv ...string) runner.Result {
	if argv[0] != "helper" {
		h.direct = append(h.direct, runner.Join(argv))
		if argv[0] == "test" {
			return runner.Result{Code: 1}
		}
		if out, ok := h.answers[runner.Join(argv)]; ok {
			return runner.Result{Out: out}
		}
		return runner.Result{}
	}
	if h.busy > 0 {
		h.busy--
		return runner.Result{Out: "basalt-policy-query: cannot load the policy: [Errno 16] Device or resource busy", Code: 2}
	}
	var qs [][]string
	if err := json.Unmarshal([]byte(argv[1]), &qs); err != nil {
		return runner.Result{Code: 64}
	}
	var keys []string
	var res []map[string]string
	for _, q := range qs {
		k := runner.Join(q)
		keys = append(keys, k)
		switch {
		case q[0] == "unsupported":
			res = append(res, map[string]string{"unsupported": "unsupported command"})
		case h.errs[k] != "":
			res = append(res, map[string]string{"error": h.errs[k]})
		default:
			res = append(res, map[string]string{"out": h.answers[k]})
		}
	}
	h.batches = append(h.batches, keys)
	b, _ := json.Marshal(map[string]any{"results": res})
	return runner.Result{Out: "noise from python\n" + string(b)}
}

func noSleep(context.Context, time.Duration) {}

// Several queries, one walk of the policy; the Query calls that follow are
// answered from the cache.
func TestPrefetchBatchesQueries(t *testing.T) {
	h := &helperRunner{answers: map[string]string{
		"sesearch -A -s httpd_t -t httpd_log_t -c file -p append": "allow httpd_t httpd_log_t:file { append open };",
		"seinfo --portcon=8181":                                   "\nPortcon: 1\n   portcon tcp 8181 system_u:object_r:intermapper_port_t:s0",
	}, errs: map[string]string{"sesearch -A -s nosuch_t -c file -p read": "InvalidType: nosuch_t is not a valid type"}}
	p := &Policy{R: h, Helper: "helper", Sleep: noSleep}
	ctx := context.Background()
	p.Prefetch(ctx, AllowQuery("httpd_t", "httpd_log_t", "file", "append"), PortconQuery(8181),
		AllowQuery("httpd_t", "", "file", "append"), AllowQuery("nosuch_t", "", "file", "read"), PortconQuery(8181))
	if len(h.batches) != 1 || len(h.batches[0]) != 4 {
		t.Fatalf("batches %v", h.batches)
	}
	if out, err := p.Query(ctx, AllowQuery("httpd_t", "httpd_log_t", "file", "append")...); err != nil || !strings.Contains(out, "append") {
		t.Fatalf("%q %v", out, err)
	}
	if out, err := p.Query(ctx, AllowQuery("httpd_t", "", "file", "append")...); err != nil || out != "" {
		t.Fatalf("an empty answer is a real nothing: %q %v", out, err)
	}
	if _, err := p.Query(ctx, AllowQuery("nosuch_t", "", "file", "read")...); err == nil || !strings.Contains(err.Error(), "not a valid type") {
		t.Fatalf("an invalid query stays an error: %v", err)
	}
	if len(h.direct) != 0 || len(h.batches) != 1 {
		t.Fatalf("direct %v batches %v", h.direct, h.batches)
	}
	// A form the helper does not know runs on its own.
	h.answers["unsupported x"] = "direct answer"
	if out, err := p.Query(ctx, "unsupported", "x"); err != nil || out != "direct answer" || len(h.direct) != 1 {
		t.Fatalf("%q %v %v", out, err, h.direct)
	}
}

// A busy policy is retried as a whole batch; a missing helper falls back
// to one command per query, once and for all.
func TestPrefetchBusyAndMissingHelper(t *testing.T) {
	h := &helperRunner{busy: 2, answers: map[string]string{"sesearch -T -s init_t -t bin_t -c process": "type_transition init_t bin_t:process unconfined_service_t;"}}
	p := &Policy{R: h, Helper: "helper", Sleep: noSleep}
	if d, err := p.ServiceDomain(context.Background(), "bin_t"); err != nil || d != "unconfined_service_t" || len(h.batches) != 1 || len(h.direct) != 0 {
		t.Fatalf("%q %v batches %v direct %v", d, err, h.batches, h.direct)
	}
	f := &runner.Fake{Answers: map[string]runner.Result{"sesearch -T -s init_t -t bin_t -c process": {Out: "type_transition init_t bin_t:process unconfined_service_t;"}},
		Default: &runner.Result{Code: 127, Err: runner.ErrNotFound}}
	q := &Policy{R: f, Helper: "/usr/libexec/basalt/basalt-policy-query", Sleep: noSleep, TTL: -1}
	for range 2 {
		if d, err := q.ServiceDomain(context.Background(), "bin_t"); err != nil || d != "unconfined_service_t" {
			t.Fatalf("%q %v", d, err)
		}
	}
	helperCalls := 0
	for _, a := range f.Asked {
		if strings.HasPrefix(a, "/usr/libexec/basalt/basalt-policy-query") {
			helperCalls++
		}
	}
	if helperCalls != 1 || len(f.Asked) != 3 {
		t.Fatalf("asked %v", f.Asked)
	}
}

// With a load identity an answer lives as long as that policy is loaded,
// however old; a new load (semanage, a module) drops every answer.
func TestCacheFollowsPolicyLoad(t *testing.T) {
	f := &runner.Fake{Answers: map[string]runner.Result{"seinfo --portcon=80": {Out: "portcon tcp 80 system_u:object_r:http_port_t:s0"}}}
	now := time.Unix(1000, 0)
	load := "load-1"
	p := &Policy{R: f, Now: func() time.Time { return now }, LoadID: func() (string, bool) { return load, true }}
	ctx := context.Background()
	_, _ = p.Query(ctx, PortconQuery(80)...)
	now = now.Add(time.Hour)
	_, _ = p.Query(ctx, PortconQuery(80)...)
	if len(f.Asked) != 1 {
		t.Fatalf("asked %v", f.Asked)
	}
	load = "load-2"
	_, _ = p.Query(ctx, PortconQuery(80)...)
	if len(f.Asked) != 2 {
		t.Fatalf("a new policy load must be queried again: %v", f.Asked)
	}
	// Without an identity: the short time to live.
	q := &Policy{R: f, Now: func() time.Time { return now }}
	_, _ = q.Query(ctx, PortconQuery(80)...)
	now = now.Add(2 * time.Minute)
	_, _ = q.Query(ctx, PortconQuery(80)...)
	if len(f.Asked) != 4 {
		t.Fatalf("asked %v", f.Asked)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Dir: filepath.Join(t.TempDir(), "cache", "policy"), Owner: os.Getuid(),
		BootID: func() (string, bool) { return "0f6c1b2a-boot", true }, Label: func(string) string { return "" }}
}

// The root command line keeps answers across runs, for the same boot and
// policy load only.
func TestStoreAcrossRuns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := &runner.Fake{Answers: map[string]runner.Result{"seinfo --portcon=80": {Out: "portcon tcp 80 system_u:object_r:http_port_t:s0"}}}
	id := func() (string, bool) { return "load-7", true }
	_, _ = (&Policy{R: f, LoadID: id, Store: s}).Query(ctx, PortconQuery(80)...)
	g := &runner.Fake{}
	if out, err := (&Policy{R: g, LoadID: id, Store: s}).Query(ctx, PortconQuery(80)...); err != nil || !strings.Contains(out, "http_port_t") || len(g.Asked) != 0 {
		t.Fatalf("second run: %q %v asked %v", out, err, g.Asked)
	}
	if len(s.Load("load-8")) != 0 {
		t.Fatal("answers of another policy load were returned")
	}
	s2 := *s
	s2.BootID = func() (string, bool) { return "another-boot", true }
	if len(s2.Load("load-7")) != 0 {
		t.Fatal("answers of another boot were returned")
	}
	// Saving for a new load removes the old file.
	if err := s.Save("load-8", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(s.Dir, "*.json")); len(m) != 1 {
		t.Fatalf("files %v", m)
	}
}

// Files the root command line must not trust: another owner, writable by
// others, a link, or a type the confined daemon may write.
func TestStoreTrust(t *testing.T) {
	s := testStore(t)
	if err := s.Save("load-1", map[string]string{"q": "a"}); err != nil {
		t.Fatal(err)
	}
	file, _, _ := s.file("load-1")
	if len(s.Load("load-1")) != 1 {
		t.Fatal("own file not read")
	}
	other := *s
	other.Owner = os.Getuid() + 1
	if len(other.Load("load-1")) != 0 {
		t.Error("file of another owner trusted")
	}
	daemon := *s
	daemon.Label = func(p string) string {
		if p == file {
			return "system_u:object_r:basalt_assistant_var_lib_t:s0"
		}
		return ""
	}
	if len(daemon.Load("load-1")) != 0 {
		t.Error("file with a daemon type trusted")
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(s.Load("load-1")) != 0 {
		t.Error("file readable by others trusted")
	}
	_ = os.Chmod(file, 0o600)
	if err := os.Chmod(s.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if len(s.Load("load-1")) != 0 {
		t.Error("directory open to others trusted")
	}
	_ = os.Chmod(s.Dir, 0o700)
	target := file + ".real"
	_ = os.Rename(file, target)
	_ = os.Symlink(target, file)
	if len(s.Load("load-1")) != 0 {
		t.Error("symbolic link followed")
	}
}

// A path checked component by component: all the rule lookups in one walk.
func TestAnalyzePathBatches(t *testing.T) {
	labels := map[string]string{
		"/srv": "system_u:object_r:var_t:s0", "/srv/www": "system_u:object_r:var_t:s0",
		"/srv/www/index.html": "unconfined_u:object_r:user_home_t:s0",
	}
	h := &helperRunner{answers: map[string]string{
		"sesearch -A -s httpd_t -t var_t -c dir -p search":              "allow httpd_t var_t:dir { getattr open search };",
		"sesearch -A -s httpd_t -t httpd_sys_content_t -c file -p open": "allow httpd_t httpd_sys_content_t:file { open read };",
		"matchpathcon -n /srv/www/index.html":                           "system_u:object_r:var_t:s0",
		"sesearch -A -s httpd_t -t user_home_t -c file -p open":         "",
		"sesearch -A -s httpd_t -c file -p open":                        "",
		"sesearch -A -s httpd_t -t var_t -c file -p open":               "",
	}}
	z := Analyzer{R: h, Policy: &Policy{R: h, Helper: "helper", Sleep: noSleep}}
	f, ok := z.AnalyzePath(context.Background(), "httpd_t", "/srv/www/index.html", false, func(p string) string { return labels[p] })
	if !ok || len(f.Actions) == 0 {
		t.Fatalf("ok %v fix %+v", ok, f)
	}
	for _, d := range h.direct {
		if strings.HasPrefix(d, "sesearch") || strings.HasPrefix(d, "seinfo") {
			t.Errorf("query outside a batch: %s", d)
		}
	}
	if len(h.batches) != 2 {
		t.Fatalf("batches %v", h.batches)
	}
}
