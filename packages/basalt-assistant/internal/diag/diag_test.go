package diag

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func snapXML(n int, typ string, pre int, date, desc string) string {
	p := ""
	if pre > 0 {
		p = "<pre_num>" + itoa(pre) + "</pre_num>"
	}
	return `<?xml version="1.0"?><snapshot><type>` + typ + `</type><num>` + itoa(n) + `</num><date>` + date + `</date>` + p +
		`<description>` + desc + `</description><cleanup>number</cleanup><userdata><key>basalt</key><value>dnf</value></userdata></snapshot>`
}

func itoa(n int) string { return strconv.Itoa(n) }

// testEnv is a fake machine: files, labels, statfs and a command table.
type testEnv struct {
	*Env
	files  map[string]string
	labels map[string]string
	fake   *runner.Fake
}

func newEnv(t *testing.T) *testEnv {
	te := &testEnv{files: map[string]string{}, labels: map[string]string{}, fake: &runner.Fake{
		Answers: map[string]runner.Result{}, Prefixes: map[string]runner.Result{}, Default: &runner.Result{Code: 1}}}
	te.Env = &Env{
		R:   te.fake,
		Now: func() time.Time { return time.Unix(1791037400, 0).UTC() },
		Statfs: func(p string) (FSStat, error) {
			return FSStat{Path: p, Total: 100 << 30, Free: 80 << 30, Used: 20 << 30}, nil
		},
		Label: func(p string) string { return te.labels[p] },
		Inode: func(string) (uint64, bool) { return 0, false },
		ReadFile: func(p string) ([]byte, error) {
			s, ok := te.files[p]
			if !ok {
				return nil, errors.New("no such file")
			}
			return []byte(s), nil
		},
		SnapshotDir: "/.snapshots",
		Decide:      decide.NewRules(nil, nil),
	}
	te.Glob = func(pattern string) []string {
		var out []string
		for p := range te.files {
			if strings.HasPrefix(p, "/.snapshots/") && strings.HasSuffix(p, "/info.xml") {
				out = append(out, p)
			}
		}
		return out
	}
	te.labels["/usr/sbin/nginx"] = "system_u:object_r:httpd_exec_t:s0"
	te.fake.Prefixes["systemctl show --timestamp=unix"] = runner.Result{Out: fixture(t, "nginx-config-error.show.txt")}
	te.fake.Prefixes["systemctl show -p Id,ActiveState,Result,LoadState"] = runner.Result{Out: "Id=system.slice\nActiveState=active\nResult=success\nLoadState=loaded\n"}
	te.fake.Prefixes["journalctl --no-pager -o json _TRANSPORT=audit"] = runner.Result{}
	te.fake.Prefixes["ausearch"] = runner.Result{Out: "<no matches>", Code: 1}
	return te
}

func TestWhyConfigErrorProposesRestoreFromSnapshot(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: fixture(t, "nginx-config-error.journal.json")}
	te.fake.Answers["nginx -t"] = runner.Result{Out: fixture(t, "nginx-t-unknown-directive.txt"), Code: 1}
	te.fake.Prefixes["diff -u"] = runner.Result{Out: "-    bogus_directive on;", Code: 1}
	te.files["/etc/nginx/nginx.conf"] = "server_name _;\nbogus_directive on;\n"
	te.files["/.snapshots/2/info.xml"] = snapXML(2, "pre", 0, "2026-10-03 14:20:03", "dnf -y install nginx")
	te.files["/.snapshots/3/info.xml"] = snapXML(3, "post", 2, "2026-10-03 14:20:05", "dnf -y install nginx")
	te.files["/.snapshots/3/snapshot/etc/nginx/nginx.conf"] = "server_name _;\n"
	// The current file changed after nginx last started (@1791037335); the
	// copy in snapshot 3 is older: it was in place at that start.
	te.fake.Answers["stat -c '%s %Y' /etc/nginx/nginx.conf"] = runner.Result{Out: "36 1791037336"}
	te.fake.Answers["stat -c '%s %Y' /.snapshots/3/snapshot/etc/nginx/nginx.conf"] = runner.Result{Out: "15 1791037000"}

	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "config_error" || !rep.Decision.Confident {
		t.Fatalf("cause %s (%s)", rep.Cause, rep.Decision.Answer.String())
	}
	if rep.ConfigFile != "/etc/nginx/nginx.conf" || rep.ConfigLine != 36 {
		t.Errorf("config location %s:%d", rep.ConfigFile, rep.ConfigLine)
	}
	if rep.Restore == nil || rep.Restore.Snapshot != 3 || rep.Restore.Diff == "" {
		t.Fatalf("restore candidate %+v", rep.Restore)
	}
	if len(rep.Actions) != 2 || rep.Actions[0].Kind != action.FileRestore || rep.Actions[1].Kind != action.UnitRestart {
		t.Fatalf("actions %+v", rep.Actions)
	}
}

func TestWhyPortConflictProposesNothing(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: fixture(t, "nginx-port-conflict.journal.json")}
	te.fake.Answers["nginx -t"] = runner.Result{Out: "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok\nnginx: configuration file /etc/nginx/nginx.conf test is successful"}
	te.fake.Answers["ss -H -ltnup 'sport = :80'"] = runner.Result{Out: fixture(t, "ss-port-80.txt")}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx.service")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "port_conflict" || !rep.Decision.Confident {
		t.Fatalf("cause %s (%s)", rep.Cause, rep.Decision.Answer.String())
	}
	if len(rep.Ports) != 1 || rep.Ports[0].Port != 80 || !strings.Contains(rep.Ports[0].Owner, "python3") {
		t.Fatalf("ports %+v", rep.Ports)
	}
	if len(rep.Actions) != 0 {
		t.Errorf("a port conflict must not propose changes: %+v", rep.Actions)
	}
	if rep.Features["config_error"] || rep.Features["journal_config_error"] {
		t.Errorf("bind error taken for a config error: %v", rep.Features)
	}
}

func TestWhyPortDeniedBySELinux(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: fixture(t, "nginx-port-denied.journal.json")}
	te.fake.Prefixes["journalctl --no-pager -o json _TRANSPORT=audit"] = runner.Result{Out: fixture(t, "avc-port-8181.audit.json")}
	te.fake.Answers["nginx -t"] = runner.Result{Out: "nginx: [emerg] bind() to 0.0.0.0:8181 failed (13: Permission denied)\nnginx: configuration file /etc/nginx/nginx.conf test failed", Code: 1}
	te.fake.Answers["seinfo --portcon=8181"] = runner.Result{Out: fixture(t, "seinfo-portcon-8181-80.txt")}
	te.fake.Answers["sesearch -A -s httpd_t -c tcp_socket -p name_bind"] = runner.Result{Out: fixture(t, "sesearch-httpd-name_bind.txt")}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "selinux_denial" {
		t.Fatalf("cause %s (%s) features %v", rep.Cause, rep.Decision.Answer.String(), rep.Features)
	}
	if !rep.Features["config_check_runtime"] || rep.Features["config_check_failed"] {
		t.Errorf("nginx -t failing on bind is not a syntax error: %v", rep.Features)
	}
	if len(rep.AVCs) != 1 || rep.AVCs[0].Class != "port" {
		t.Fatalf("avcs %+v", rep.AVCs)
	}
	// 8181 belongs to intermapper_port_t: relabeling it is below the
	// threshold, so no change is proposed and the report asks for review.
	if len(rep.Actions) != 0 || !strings.Contains(rep.Explanation, "review") {
		t.Errorf("actions %+v explanation %q", rep.Actions, rep.Explanation)
	}
}

func TestWhyHiddenDenialOnMovedLogDir(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["journalctl --no-pager -o json -u nginx.service"] = runner.Result{Out: `{"MESSAGE":"nginx: [emerg] open() \"/srv/nginx-logs/access.log\" failed (13: Permission denied)","PRIORITY":"6","_SYSTEMD_UNIT":"nginx.service","__REALTIME_TIMESTAMP":"1791037300000000"}` + "\n" +
		`{"MESSAGE":"nginx.service: Failed with result 'exit-code'.","PRIORITY":"4","UNIT":"nginx.service","_PID":"1","MESSAGE_ID":"d9b373ed55a64feb8242e02dbe79a49c","__REALTIME_TIMESTAMP":"1791037300100000"}`}
	te.fake.Answers["nginx -t"] = runner.Result{Out: "nginx: configuration file /etc/nginx/nginx.conf test is successful"}
	te.labels["/srv"] = "system_u:object_r:var_t:s0"
	te.labels["/srv/nginx-logs"] = "unconfined_u:object_r:admin_home_t:s0"
	te.labels["/srv/nginx-logs/access.log"] = "unconfined_u:object_r:admin_home_t:s0"
	te.fake.Answers["sesearch -A -s httpd_t -t var_t -c dir -p search"] = runner.Result{Out: "allow httpd_t file_type:dir { getattr open search };"}
	te.fake.Answers["sesearch -A -s httpd_t -t admin_home_t -c dir -p search"] = runner.Result{Out: "allow domain admin_home_t:dir { getattr open search };"}
	te.fake.Answers["matchpathcon -n /srv/nginx-logs/access.log"] = runner.Result{Out: "system_u:object_r:var_t:s0"}
	te.fake.Answers["sesearch -A -s httpd_t -t httpd_log_t -c file -p append"] = runner.Result{Out: "allow daemon logfile:file { append getattr ioctl lock };"}
	rep, err := WhyUnit(context.Background(), te.Env, "nginx")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "selinux_denial" || !rep.Decision.Confident {
		t.Fatalf("cause %s (%s) features %v", rep.Cause, rep.Decision.Answer.String(), rep.Features)
	}
	var got []string
	for _, a := range rep.Actions {
		cs, _ := a.Commands()
		for _, c := range cs {
			got = append(got, c.String())
		}
	}
	want := []string{`semanage fcontext -a -t httpd_log_t '/srv/nginx-logs(/.*)?'`, "restorecon -Rv /srv/nginx-logs", "systemctl restart nginx.service"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands\n got %q\nwant %q", got, want)
	}
}

func TestWhyHealthyUnit(t *testing.T) {
	te := newEnv(t)
	te.fake.Prefixes["systemctl show --timestamp=unix"] = runner.Result{Out: "Id=sshd.service\nLoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nExecStart={ path=/usr/sbin/sshd ; argv[]=/usr/sbin/sshd -D }\n"}
	te.fake.Answers["sshd -t"] = runner.Result{}
	rep, err := WhyUnit(context.Background(), te.Env, "sshd")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy || len(rep.Actions) != 0 {
		t.Fatalf("healthy %v actions %+v", rep.Healthy, rep.Actions)
	}
}

func TestConfinedSkipsPrivilegedProbes(t *testing.T) {
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
	for _, q := range te.fake.Asked {
		if q == "nginx -t" {
			t.Fatal("confined diagnosis ran the config checker")
		}
	}
	// The journal alone (ExecStartPre=nginx -t output) still finds it.
	if rep.Cause != "config_error" || rep.Restore == nil || !strings.HasPrefix(rep.Restore.How, "size/mtime differ") {
		t.Fatalf("cause %s restore %+v", rep.Cause, rep.Restore)
	}
}

func TestParsersAndForecast(t *testing.T) {
	u := ParseBtrfsUsage(fixture(t, "btrfs-usage.txt"))
	if u["device_size"] != 15474884608 || u["used"] != 1377763328 || u["data_ratio"] != 100 {
		t.Fatalf("btrfs usage %v", u)
	}
	tot, ex, ok := ParseFiDu(fixture(t, "btrfs-fi-du.txt"))
	if !ok || tot != 1184198656 || ex != 2142208 {
		t.Fatalf("fi du %d %d %v", tot, ex, ok)
	}
	s, err := ParseInfoXML([]byte(fixture(t, "snapper-info-post.xml")))
	if err != nil || s.Number != 3 || s.Type != "post" || s.Pre != 2 || s.Userdata["basalt"] != "dnf" || s.Time.IsZero() {
		t.Fatalf("info.xml %+v %v", s, err)
	}
	t0 := time.Unix(1791000000, 0)
	var samples []Sample
	for i := 0; i < 5; i++ {
		samples = append(samples, Sample{Time: t0.Add(time.Duration(i) * 6 * time.Hour), Used: uint64(10<<30 + i*(1<<30)), Total: 20 << 30})
	}
	fc := Predict(samples, FSStat{Total: 20 << 30, Free: 6 << 30})
	// 1 GiB per 6 h = 4 GiB/day; 6 GiB free -> 1.5 days.
	if fc.DaysToFull < 1.49 || fc.DaysToFull > 1.51 {
		t.Fatalf("forecast %+v", fc)
	}
	if Predict(samples[:2], FSStat{}).DaysToFull != -1 {
		t.Error("forecast from two samples")
	}
}

func TestOrphanPre(t *testing.T) {
	now := time.Unix(1791037400, 0)
	snaps := []Snapshot{
		{Number: 2, Type: "pre", Time: now.Add(-time.Hour)},
		{Number: 3, Type: "post", Pre: 2, Time: now.Add(-time.Hour)},
		{Number: 4, Type: "pre", Time: now.Add(-10 * time.Minute)},
		{Number: 5, Type: "pre", Time: now.Add(-10 * time.Second)},
	}
	o := OrphanPre(snaps, now, 2*time.Minute)
	if len(o) != 1 || o[0].Number != 4 {
		t.Fatalf("orphans %+v", o)
	}
}

func TestDiskProposesSnapshotDelete(t *testing.T) {
	te := newEnv(t)
	te.Statfs = func(p string) (FSStat, error) { return FSStat{Path: p, Total: 100 << 30, Free: 4 << 30}, nil }
	te.HistoryPath = t.TempDir() + "/h.jsonl"
	te.files["/.snapshots/6/info.xml"] = snapXML(6, "post", 5, "2026-10-03 15:00:00", "dnf -y install bigthing")
	te.fake.Answers["journalctl --disk-usage"] = runner.Result{Out: fixture(t, "journal-disk-usage.txt")}
	te.fake.Answers["du -sb /var/cache/libdnf5"] = runner.Result{Out: "1048576\t/var/cache/libdnf5"}
	te.fake.Answers["btrfs filesystem du -s --raw /.snapshots/6/snapshot"] = runner.Result{Out: "     Total   Exclusive  Set shared  Filename\n21474836480 20401094656 1073741824  /.snapshots/6/snapshot"}
	r, err := te.Disk(context.Background(), DefaultDiskThresholds, 30)
	if err != nil {
		t.Fatal(err)
	}
	if r.Decision.Answer.Top != "snapshots" || len(r.Actions) != 1 || r.Actions[0].Kind != action.SnapshotDelete || r.Actions[0].Params["snapshot"] != "6" {
		t.Fatalf("decision %s actions %+v", r.Decision.Answer.String(), r.Actions)
	}
}

func TestDnfFailureFromAuditRecord(t *testing.T) {
	te := newEnv(t)
	te.files["/.snapshots/6/info.xml"] = snapXML(6, "pre", 0, "2026-10-03 14:23:46", "dnf -y install /root/basalt-lab-broken-1-1.noarch.rpm")
	te.files["/.snapshots/7/info.xml"] = snapXML(7, "post", 6, "2026-10-03 14:23:46", "dnf -y install /root/basalt-lab-broken-1-1.noarch.rpm")
	te.fake.Prefixes["journalctl --no-pager -o json _TRANSPORT=audit"] = runner.Result{Out: fixture(t, "rpm-install-failed.audit.json")}
	te.files[DnfLog] = "2026-10-03T14:23:46+0000 [3271] ERROR [rpm] %prein(basalt-lab-broken-1-1.noarch) scriptlet failed, exit status 1\n"
	same := "basalt-release.noarch 0:44-3.fc44\n"
	te.fake.Prefixes["rpm -qa --dbpath /.snapshots/6/snapshot"] = runner.Result{Out: same}
	te.fake.Prefixes["rpm -qa --dbpath /usr/lib/sysimage/rpm"] = runner.Result{Out: same}
	r := te.DiagnoseDnf(context.Background(), 0, time.Unix(1791037426, 0))
	if r.Pre == nil || r.Pre.Number != 6 || len(r.Failed) != 1 {
		t.Fatalf("pre %+v failed %v", r.Pre, r.Failed)
	}
	// %pre failed: nothing was installed, nothing to roll back.
	if r.Decision.Answer.Top != "investigate" || len(r.Actions) != 0 {
		t.Fatalf("decision %s actions %+v", r.Decision.Answer.String(), r.Actions)
	}
	// Same failure after packages changed: roll back to the pre snapshot.
	te.fake.Prefixes["rpm -qa --dbpath /usr/lib/sysimage/rpm"] = runner.Result{Out: same + "basalt-lab-broken.noarch 0:2-1\n"}
	r = te.DiagnoseDnf(context.Background(), 0, time.Unix(1791037426, 0))
	if r.Decision.Answer.Top != "rollback" || len(r.Actions) != 1 || r.Actions[0].Params["snapshot"] != "6" {
		t.Fatalf("decision %s actions %+v", r.Decision.Answer.String(), r.Actions)
	}
}

func TestDnfPostScriptletFailure(t *testing.T) {
	if _, ok := DnfLogFailure(`2026-10-03T14:50:23+0000 [7689] WARNING [rpm] %post(basalt-lab-postfail-1-1.noarch) scriptlet failed, exit status 1`); !ok {
		t.Fatal("post scriptlet failure line not recognized")
	}
	if _, ok := DnfLogFailure(`2026-10-03T14:50:23+0000 [7689] INFO RPM callback start %post scriptlet "x"`); ok {
		t.Fatal("info line taken for a failure")
	}
	te := newEnv(t)
	te.Confined = true // the daemon: no rpm database comparison
	te.files["/.snapshots/14/info.xml"] = snapXML(14, "pre", 0, "2026-10-03 14:50:18", "dnf -y install broken")
	te.files["/.snapshots/15/info.xml"] = snapXML(15, "post", 14, "2026-10-03 14:50:19", "dnf -y install broken")
	te.files["/.snapshots/16/info.xml"] = snapXML(16, "pre", 0, "2026-10-03 14:50:23", "dnf -y install postfail")
	te.files["/.snapshots/17/info.xml"] = snapXML(17, "post", 16, "2026-10-03 14:50:23", "dnf -y install postfail")
	te.files[DnfLog] = "2026-10-03T14:50:19+0000 [7097] ERROR [rpm] %prein(broken-1-1.noarch) scriptlet failed, exit status 1\n" +
		"2026-10-03T14:50:23+0000 [7689] WARNING [rpm] %post(postfail-1-1.noarch) scriptlet failed, exit status 1\n"
	at, _ := time.Parse(time.RFC3339, "2026-10-03T14:50:23Z")
	r := te.DiagnoseDnf(context.Background(), 0, at)
	if r.Pre == nil || r.Pre.Number != 16 {
		t.Fatalf("pre %+v", r.Pre)
	}
	for _, l := range r.LogLines {
		if strings.Contains(l, "prein") {
			t.Fatalf("a line of the previous transaction leaked in: %s", l)
		}
	}
	if r.Decision.Answer.Top != "rollback" || len(r.Actions) != 1 || r.Actions[0].Params["snapshot"] != "16" {
		t.Fatalf("decision %s actions %+v", r.Decision.Answer.String(), r.Actions)
	}
}

func TestRestoreSkipsCopiesNewerThanTheLastGoodStart(t *testing.T) {
	te := newEnv(t)
	te.Confined = true
	te.files["/.snapshots/3/info.xml"] = snapXML(3, "post", 2, "2026-10-03 14:20:05", "x")
	te.files["/.snapshots/4/info.xml"] = snapXML(4, "post", 2, "2026-10-03 14:30:05", "y")
	te.fake.Answers["stat -c '%s %Y' /etc/nginx/nginx.conf"] = runner.Result{Out: "36 1791037500"}
	te.fake.Answers["stat -c '%s %Y' /.snapshots/4/snapshot/etc/nginx/nginx.conf"] = runner.Result{Out: "50 1791037400"} // broken too, newer
	te.fake.Answers["stat -c '%s %Y' /.snapshots/3/snapshot/etc/nginx/nginx.conf"] = runner.Result{Out: "15 1791037000"}
	rc := te.findRestore(context.Background(), "/etc/nginx/nginx.conf", time.Unix(1791037335, 0))
	if rc == nil || rc.Snapshot != 3 {
		t.Fatalf("candidate %+v", rc)
	}
}

func TestFoundPathSkipsFindErrors(t *testing.T) {
	out := "/usr/bin/find: '/var/www': No such file or directory\n/srv/data/x.log\n"
	if p := foundPath(out); p != "/srv/data/x.log" {
		t.Fatalf("got %q", p)
	}
	if p := foundPath("/usr/bin/find: '/var/www': No such file or directory\n"); p != "" {
		t.Fatalf("an error message was taken for a path: %q", p)
	}
}
