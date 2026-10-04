package diag

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// Tests for the problems a live run on a lab VM found in the diagnosers.

const busy = "ERROR: Device or resource busy: /sys/fs/selinux/policy"

// Two diagnoses of the same event race on /sys/fs/selinux/policy: a query
// that keeps losing is an error and an incomplete diagnosis, never "no rule
// allows it" (which turned into a wrong label finding before).
func TestPolicyBusyIsAnErrorNotANegative(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: `{"MESSAGE":"nginx: [emerg] open() \"/srv/nginx-logs/access.log\" failed (13: Permission denied)","PRIORITY":"6","_SYSTEMD_UNIT":"nginx.service","__REALTIME_TIMESTAMP":"1791037300000000"}`}
	te.fake.Answers["nginx -t"] = runner.Result{Out: "nginx: configuration file /etc/nginx/nginx.conf test is successful"}
	te.labels["/srv"] = "system_u:object_r:var_t:s0"
	te.labels["/srv/nginx-logs"] = "unconfined_u:object_r:admin_home_t:s0"
	te.fake.Prefixes["sesearch -A"] = runner.Result{Out: busy, Code: 1}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) == 0 || !rep.Features["policy_query_failed"] {
		t.Fatalf("errors %v features %v", rep.Errors, rep.Features)
	}
	if len(rep.Actions) != 0 || len(rep.Hints) != 0 {
		t.Fatalf("a change proposed from an incomplete diagnosis: %+v", rep.Actions)
	}
	if !strings.Contains(rep.Explanation, "incomplete") {
		t.Errorf("explanation %q", rep.Explanation)
	}
	n := 0
	for _, q := range te.fake.Asked {
		if q == "sesearch -A -s httpd_t -t var_t -c dir -p search" {
			n++
		}
	}
	if n < 2 {
		t.Errorf("busy query tried %d times, want retries", n)
	}
}

// A busy policy that frees up after a few tries gives the normal answer.
func TestPolicyRetriesThenAnswers(t *testing.T) {
	calls := 0
	r := readerFunc(func(argv ...string) runner.Result {
		calls++
		if calls < 3 {
			return runner.Result{Out: busy, Code: 1}
		}
		return runner.Result{Out: "allow httpd_t var_t:dir { search };"}
	})
	var waits []time.Duration
	p := &selinux.Policy{R: r, Sleep: func(_ context.Context, d time.Duration) { waits = append(waits, d) }}
	out, err := p.Query(context.Background(), "sesearch", "-A", "-s", "httpd_t")
	if err != nil || !strings.Contains(out, "allow") || calls != 3 || len(waits) != 2 || waits[1] < waits[0]/2 {
		t.Fatalf("out %q err %v calls %d waits %v", out, err, calls, waits)
	}
	// Cached: the policy is not loaded again for the same query.
	if _, err := p.Query(context.Background(), "sesearch", "-A", "-s", "httpd_t"); err != nil || calls != 3 {
		t.Fatalf("cache: calls %d err %v", calls, err)
	}
	// A real failure is not retried and not cached.
	calls = 0
	fail := &selinux.Policy{R: readerFunc(func(argv ...string) runner.Result {
		calls++
		return runner.Result{Out: "sesearch: invalid type", Code: 1}
	}), Sleep: func(context.Context, time.Duration) {}}
	if _, err := fail.Query(context.Background(), "sesearch", "-A", "-s", "nope_t"); err == nil || calls != 1 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}

type readerFunc func(argv ...string) runner.Result

func (f readerFunc) Read(_ context.Context, argv ...string) runner.Result { return f(argv...) }

// A unit that runs a shell as a plain user and fails on a root-only
// directory: file modes (DAC), not SELinux. The domain comes from the
// policy (init_t runs shell_exec_t in unconfined_service_t), not from the
// label's name (shell_t), so no label check blames SELinux.
func TestDACDenialOfAShellUnit(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["systemctl show --timestamp=unix"] = runner.Result{Out: fixture(t, "dac.show.txt")}
	te.fake.Prefixes["journalctl --no-pager -o json -u basalt-lab-dac.service"] = runner.Result{Out: fixture(t, "dac.journal.json")}
	te.labels["/bin/sh"] = "system_u:object_r:shell_exec_t:s0"
	te.fake.Answers["sesearch -T -s init_t -t shell_exec_t -c process"] = runner.Result{Out: "type_transition init_t shell_exec_t:process unconfined_service_t;"}
	te.fake.Answers["id -u nobody"] = runner.Result{Out: "65534"}
	te.fake.Answers["id -G nobody"] = runner.Result{Out: "65534"}
	te.fake.Answers["stat -c '%f %u %g %U %G' /var"] = runner.Result{Out: "41ed 0 0 root root"}
	te.fake.Answers["stat -c '%f %u %g %U %G' /var/lib"] = runner.Result{Out: "41ed 0 0 root root"}
	te.fake.Answers["stat -c '%f %u %g %U %G' /var/lib/basalt-lab-dac"] = runner.Result{Out: "41c0 0 0 root root"}
	rep, err := WhyUnit(context.Background(), te.Env, "basalt-lab-dac")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Domain != "unconfined_service_t" {
		t.Errorf("domain %q (%s)", rep.Domain, rep.DomainHow)
	}
	if rep.Cause != "unknown" || !rep.Features["dac_denied"] || rep.Features["path_label_problem"] {
		t.Fatalf("cause %s (%s) features %v", rep.Cause, rep.Decision.Answer.String(), rep.Features)
	}
	if len(rep.DAC) != 1 || rep.DAC[0].Path != "/var/lib/basalt-lab-dac" || rep.DAC[0].Mode != "0700" || rep.DAC[0].Need != "search" {
		t.Fatalf("dac %+v", rep.DAC)
	}
	if len(rep.AVCs) != 0 || len(rep.Actions) != 0 || !strings.Contains(rep.Explanation, "not SELinux") {
		t.Fatalf("avcs %+v actions %+v explanation %q", rep.AVCs, rep.Actions, rep.Explanation)
	}
	for _, q := range te.fake.Asked {
		if strings.HasPrefix(q, "sesearch -A -s shell_t") || strings.HasPrefix(q, "sesearch -A -s unconfined_service_t") {
			t.Fatalf("label check for an unconfined unit: %s", q)
		}
	}
}

// Killed by the OOM killer at its MemoryMax: crashed, explained, and no
// restart (it would fail the same way).
func TestOOMKill(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["systemctl show --timestamp=unix"] = runner.Result{Out: fixture(t, "oom.show.txt")}
	te.fake.Prefixes["journalctl --no-pager -o json -u basalt-lab-oom.service"] = runner.Result{Out: fixture(t, "oom.journal.json")}
	te.fake.Prefixes["journalctl --no-pager -o cat -k"] = runner.Result{Out: fixture(t, "oom-kernel.txt")}
	te.labels["/usr/bin/python3"] = "system_u:object_r:bin_t:s0"
	te.fake.Answers["sesearch -T -s init_t -t bin_t -c process"] = runner.Result{Out: "type_transition init_t bin_t:process unconfined_service_t;"}
	rep, err := WhyUnit(context.Background(), te.Env, "basalt-lab-oom")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "crashed" || !rep.Decision.Confident || !rep.Features["oom_killed"] {
		t.Fatalf("cause %s (%s) features %v", rep.Cause, rep.Decision.Answer.String(), rep.Features)
	}
	if rep.OOM == nil || rep.OOM.Constraint != "CONSTRAINT_MEMCG" || rep.OOM.Limits["MemoryMax"] != "67108864" || len(rep.OOM.Kernel) != 2 {
		t.Fatalf("oom %+v", rep.OOM)
	}
	if len(rep.Actions) != 0 || !strings.Contains(rep.Explanation, "MemoryMax=64.0 MiB") || !strings.Contains(rep.Explanation, "set-property") {
		t.Fatalf("actions %+v explanation %q", rep.Actions, rep.Explanation)
	}
	// The journal alone (systemd's messages, what the evaluation suite
	// derives features from) shows it too.
	if !JournalFeatures([]string{"x.service: A process of this unit has been killed by the OOM killer."})["oom_killed"] {
		t.Error("journal feature")
	}
}

// The checker runs in the sandbox (no side effects); a newer snapshot copy
// that fails the checker is skipped for an older one that passes, even
// though the older one changed after the last good start.
func TestRestorePicksTheCopyThatPassesTheChecker(t *testing.T) {
	te := newEnv(t)
	te.Isolate = func(argv []string, replace map[string]string) []string {
		out := []string{"sandbox"}
		for d, s := range replace {
			out = append(out, d+"="+s)
		}
		return append(out, argv...)
	}
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: fixture(t, "nginx-config-error.journal.json")}
	te.fake.Answers["sandbox nginx -t"] = runner.Result{Out: fixture(t, "nginx-t-unknown-directive.txt"), Code: 1}
	te.fake.Answers["sandbox /etc/nginx/nginx.conf=/.snapshots/5/snapshot/etc/nginx/nginx.conf nginx -t"] = runner.Result{Out: "nginx: [emerg] unexpected \"}\" in /etc/nginx/nginx.conf:50", Code: 1}
	te.fake.Answers["sandbox /etc/nginx/nginx.conf=/.snapshots/3/snapshot/etc/nginx/nginx.conf nginx -t"] = runner.Result{Out: "nginx: configuration file /etc/nginx/nginx.conf test is successful"}
	te.fake.Prefixes["diff -u"] = runner.Result{Out: "-    bogus_directive on;", Code: 1}
	te.files["/etc/nginx/nginx.conf"] = "server_name _;\nbogus_directive on;\n"
	te.files["/.snapshots/3/info.xml"] = snapXML(3, "post", 2, "2026-10-03 14:20:05", "x")
	te.files["/.snapshots/5/info.xml"] = snapXML(5, "post", 4, "2026-10-03 14:30:05", "y")
	te.files["/.snapshots/3/snapshot/etc/nginx/nginx.conf"] = "server_name _;\n"
	te.files["/.snapshots/5/snapshot/etc/nginx/nginx.conf"] = "server_name _;\n}\n"
	te.fake.Answers["stat -c '%s %Y' /etc/nginx/nginx.conf"] = runner.Result{Out: "36 1791037336"}
	te.fake.Answers["stat -c '%s %Y' /.snapshots/5/snapshot/etc/nginx/nginx.conf"] = runner.Result{Out: "17 1791037000"}
	te.fake.Answers["stat -c '%s %Y' /.snapshots/3/snapshot/etc/nginx/nginx.conf"] = runner.Result{Out: "15 1791037400"}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range te.fake.Asked {
		if q == "nginx -t" {
			t.Fatal("the config checker ran outside the sandbox")
		}
	}
	if rep.Restore == nil || rep.Restore.Snapshot != 3 || rep.Restore.Checked != "nginx -t" {
		t.Fatalf("restore %+v evidence %v", rep.Restore, rep.Evidence)
	}
	if len(rep.Actions) != 2 || rep.Actions[0].Params["snapshot"] != "3" {
		t.Fatalf("actions %+v", rep.Actions)
	}
	found := false
	for _, e := range rep.Evidence {
		found = found || strings.Contains(e, "snapshot 5") && strings.Contains(e, "fails `nginx -t` too")
	}
	if !found {
		t.Errorf("rejected copy not in the evidence: %v", rep.Evidence)
	}
	// No copy passes: nothing to restore.
	te.fake.Answers["sandbox /etc/nginx/nginx.conf=/.snapshots/3/snapshot/etc/nginx/nginx.conf nginx -t"] = runner.Result{Out: "nginx: [emerg] x in /etc/nginx/nginx.conf:2", Code: 1}
	rep, _ = WhyUnit(context.Background(), te.Env, "nginx")
	if rep.Restore != nil || len(rep.Actions) != 0 {
		t.Fatalf("restore %+v actions %+v", rep.Restore, rep.Actions)
	}
}

// When the sandbox cannot be set up (not root, an old kernel), the checker
// is not run at all: the report says so.
func TestCheckerNotRunWithoutTheSandbox(t *testing.T) {
	te := newEnv(t)
	te.Isolate = func(argv []string, _ map[string]string) []string { return append([]string{"sandbox"}, argv...) }
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: fixture(t, "nginx-port-conflict.journal.json")}
	te.fake.Answers["sandbox nginx -t"] = runner.Result{Out: "basalt-sandbox: needs root (a private mount namespace); run the diagnosis as root", Code: 125}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if rep.ConfigCheck != nil || len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0], "needs root") {
		t.Fatalf("check %+v skipped %v", rep.ConfigCheck, rep.Skipped)
	}
}

// The confined daemon sees snapshot metadata only: a restore is a hint.
func TestConfinedRestoreIsAHint(t *testing.T) {
	te := newEnv(t)
	te.Confined = true
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: fixture(t, "nginx-config-error.journal.json")}
	te.fake.Prefixes["stat -c"] = runner.Result{Out: "100 1"}
	te.files["/.snapshots/3/info.xml"] = snapXML(3, "post", 2, "2026-10-03 14:20:05", "dnf -y install nginx")
	te.fake.Answers["stat -c '%s %Y' /.snapshots/3/snapshot/etc/nginx/nginx.conf"] = runner.Result{Out: "90 0"}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "config_error" || len(rep.Actions) != 0 || len(rep.Hints) != 1 || len(rep.Hints[0].Actions) != 2 ||
		rep.Hints[0].Actions[0].Kind != action.FileRestore || !strings.Contains(rep.Explanation, "hint") {
		t.Fatalf("cause %s actions %+v hints %+v %q", rep.Cause, rep.Actions, rep.Hints, rep.Explanation)
	}
}

// The root view confirms a hint: the snapshot copy must pass the checker.
func TestConfirmHint(t *testing.T) {
	te := newEnv(t)
	te.Isolate = func(argv []string, replace map[string]string) []string {
		out := []string{"sandbox"}
		for d, s := range replace {
			out = append(out, d+"="+s)
		}
		return append(out, argv...)
	}
	te.files["/.snapshots/3/info.xml"] = snapXML(3, "post", 2, "2026-10-03 14:20:05", "x")
	te.files["/.snapshots/3/snapshot/etc/nginx/nginx.conf"] = "server_name _;\n"
	te.files["/etc/nginx/nginx.conf"] = "server_name _;\nbogus_directive on;\n"
	acts := []action.Action{
		{Kind: action.FileRestore, Params: map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "3"}},
		{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}},
	}
	key := "sandbox /etc/nginx/nginx.conf=/.snapshots/3/snapshot/etc/nginx/nginx.conf nginx -t"
	te.fake.Answers[key] = runner.Result{Out: "nginx: configuration file /etc/nginx/nginx.conf test is successful"}
	ev, review, err := te.ConfirmHint(context.Background(), "nginx.service", acts)
	if err != nil || review || len(ev) == 0 || !strings.Contains(ev[0], "passes `nginx -t`") {
		t.Fatalf("ev %v review %v err %v", ev, review, err)
	}
	te.fake.Answers[key] = runner.Result{Out: "nginx: [emerg] unknown directive in /etc/nginx/nginx.conf:2", Code: 1}
	if _, _, err := te.ConfirmHint(context.Background(), "nginx.service", acts); err == nil || !strings.Contains(err.Error(), "fails") {
		t.Fatalf("a copy that fails the checker was confirmed: %v", err)
	}
	acts[0].Params["snapshot"] = "9"
	if _, _, err := te.ConfirmHint(context.Background(), "nginx.service", acts); err == nil {
		t.Fatal("a missing snapshot was confirmed")
	}
	te.Confined = true
	if _, _, err := te.ConfirmHint(context.Background(), "nginx.service", acts); err == nil {
		t.Fatal("confirmed from the confined view")
	}
}

// A denied page under a document root the nginx configuration names:
// found by inode, never from find's error text.
func TestDocRootSearch(t *testing.T) {
	te := newEnv(t)
	te.files["/etc/nginx/nginx.conf"] = "http {\n  server {\n    root         /data/www;\n    location /x { alias \"/srv/alias\"; }\n  }\n}\n"
	inodes := map[string]uint64{"/data/www": 10, "/data/www/index.html": 4242, "/usr/share/nginx/html": 11}
	te.Inode = func(p string) (uint64, bool) { n, ok := inodes[p]; return n, ok }
	te.fake.Answers["journalctl --no-pager -o cat --since @1791036800 -p 0..5 -n 2000"] = runner.Result{}
	te.fake.Answers["find /data/www -xdev -inum 4242 -name index.html -print -quit"] = runner.Result{Out: "/data/www/index.html"}
	a := selinux.AVC{Name: "index.html", Ino: 4242, Time: time.Unix(1791037400, 0)}
	p, how := te.resolveAVCPath(context.Background(), a)
	if p != "/data/www/index.html" || how != "inode search" {
		t.Fatalf("path %q how %q", p, how)
	}
	roots := te.docRoots()
	if len(roots) < 3 || roots[0] != "/data/www" || roots[1] != "/srv/alias" {
		t.Fatalf("roots %v", roots)
	}
	for _, q := range te.fake.Asked {
		if strings.HasPrefix(q, "find /var/www") || strings.HasPrefix(q, "find /srv/alias") {
			t.Fatalf("searched a start directory that does not exist: %s", q)
		}
	}
	// find's error message for a missing start directory is never a path.
	te.fake.Answers["find /data/www -xdev -inum 4242 -name index.html -print -quit"] = runner.Result{Out: "/usr/bin/find: '/var/www': No such file or directory", Code: 1}
	if p, _ := te.resolveAVCPath(context.Background(), a); p != "" {
		t.Fatalf("got %q", p)
	}
}
