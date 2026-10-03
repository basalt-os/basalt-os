package selinux

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
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

const avcPort = `AVC avc:  denied  { name_bind } for  pid=3073 comm="nginx" src=8181 scontext=system_u:system_r:httpd_t:s0 tcontext=system_u:object_r:intermapper_port_t:s0 tclass=tcp_socket permissive=0`

func TestParseAVC(t *testing.T) {
	a, ok := ParseAVC(avcPort)
	if !ok {
		t.Fatal("not parsed")
	}
	if a.SType() != "httpd_t" || a.TType() != "intermapper_port_t" || a.Class != "tcp_socket" || a.Port != 8181 ||
		!a.IsPort() || a.Proto() != "tcp" || a.Comm != "nginx" || a.Permissive {
		t.Fatalf("fields: %+v", a)
	}
	f, ok := ParseAVC(`type=AVC msg=audit(1791037400.123:456): avc:  denied  { append } for  pid=12 comm="nginx" name="access.log" dev="vda3" ino=257 scontext=system_u:system_r:httpd_t:s0 tcontext=unconfined_u:object_r:default_t:s0 tclass=file permissive=1`)
	if !ok || f.Name != "access.log" || f.Ino != 257 || f.Dev != "vda3" || !f.IsFile() || !f.Permissive {
		t.Fatalf("file AVC: %+v", f)
	}
	if _, ok := ParseAVC(`avc:  granted  { setenforce } for pid=1`); ok {
		t.Error("granted record parsed as a denial")
	}
	g := GroupAVCs([]AVC{a, a, f})
	if len(g) != 2 || g[0].Count != 2 {
		t.Errorf("grouping: %+v", g)
	}
}

func TestParseSesearch(t *testing.T) {
	rules := ParseSesearch(fixture(t, "sesearch-httpd-name_bind.txt"))
	var plain, cond int
	for _, r := range rules {
		if r.Bool != "" {
			cond++
		} else {
			plain++
		}
	}
	if plain == 0 || cond == 0 {
		t.Fatalf("plain %d conditional %d", plain, cond)
	}
	// Multi-boolean expressions are not simple fixes and are skipped.
	for _, r := range ParseSesearch(fixture(t, "sesearch-httpd-log-types.txt")) {
		if strings.ContainsAny(r.Bool, " &|") {
			t.Errorf("complex expression kept: %s", r.Line)
		}
	}
}

// fake builds a runner answering from fixtures.
func fake(extra map[string]runner.Result) *runner.Fake {
	f := &runner.Fake{Answers: map[string]runner.Result{}, Default: &runner.Result{}}
	for k, v := range extra {
		f.Answers[k] = v
	}
	return f
}

func TestPortOwnedByAnotherTypeNeedsReview(t *testing.T) {
	a, _ := ParseAVC(avcPort)
	r := fake(map[string]runner.Result{
		"seinfo --portcon=8181":                             {Out: fixture(t, "seinfo-portcon-8181-80.txt")},
		"sesearch -A -s httpd_t -c tcp_socket -p name_bind": {Out: fixture(t, "sesearch-httpd-name_bind.txt")},
	})
	f := Analyzer{R: r}.Analyze(context.Background(), Group{AVC: a, Count: 1})
	if f.Class != ClassPort || !f.Features["port_owned_by_other"] {
		t.Fatalf("class %s features %v", f.Class, f.Features)
	}
	if len(f.Actions) != 1 || f.Actions[0].Params["type"] != "http_port_t" || f.Actions[0].Params["mode"] != "modify" {
		t.Fatalf("actions %+v", f.Actions)
	}
	// The conditional rule on ephemeral_port_type (another attribute) must
	// not turn into a boolean suggestion.
	if f.Features["boolean_off"] {
		t.Error("unrelated boolean proposed")
	}
}

func TestPortUnlabeled(t *testing.T) {
	a, _ := ParseAVC(strings.ReplaceAll(strings.ReplaceAll(avcPort, "8181", "8099"), "intermapper_port_t", "unreserved_port_t"))
	r := fake(map[string]runner.Result{
		"seinfo --portcon=8099":                             {Out: "Portcon: 2\n   portcon tcp 1024-32767 system_u:object_r:unreserved_port_t:s0\n   portcon udp 1024-32767 system_u:object_r:unreserved_port_t:s0\n"},
		"sesearch -A -s httpd_t -c tcp_socket -p name_bind": {Out: fixture(t, "sesearch-httpd-name_bind.txt")},
	})
	f := Analyzer{R: r}.Analyze(context.Background(), Group{AVC: a, Count: 1})
	cmds, _ := f.Actions[0].Commands()
	if f.Class != ClassPort || cmds[0].String() != "semanage port -a -t http_port_t -p tcp 8099" {
		t.Fatalf("class %s cmds %v", f.Class, cmds)
	}
}

func TestBooleanOff(t *testing.T) {
	a, _ := ParseAVC(`avc:  denied  { read } for  pid=1 comm="nginx" name="index.html" dev="vda3" ino=9 scontext=system_u:system_r:httpd_t:s0 tcontext=unconfined_u:object_r:user_home_t:s0 tclass=file permissive=0`)
	a.Path = "/home/alice/public_html/index.html"
	r := fake(map[string]runner.Result{
		"matchpathcon -n /home/alice/public_html/index.html":    {Out: "unconfined_u:object_r:user_home_t:s0"},
		"sesearch -A -s httpd_t -t user_home_t -c file -p read": {Out: fixture(t, "sesearch-httpd-user_home.txt")},
		"getsebool httpd_read_user_content":                     {Out: "httpd_read_user_content --> off"},
	})
	f := Analyzer{R: r}.Analyze(context.Background(), Group{AVC: a, Count: 1})
	if f.Class != ClassBoolean || f.Actions[0].Kind != action.SELinuxBoolean || f.Actions[0].Params["name"] != "httpd_read_user_content" {
		t.Fatalf("class %s actions %+v", f.Class, f.Actions)
	}
}

func TestMislabeledRestorecon(t *testing.T) {
	a, _ := ParseAVC(`avc:  denied  { append } for  pid=1 comm="nginx" name="site.log" dev="vda3" ino=9 scontext=system_u:system_r:httpd_t:s0 tcontext=unconfined_u:object_r:admin_home_t:s0 tclass=file permissive=0`)
	a.Path = "/var/log/nginx/site.log"
	r := fake(map[string]runner.Result{
		"matchpathcon -n /var/log/nginx/site.log":                 {Out: "system_u:object_r:httpd_log_t:s0"},
		"sesearch -A -s httpd_t -t httpd_log_t -c file -p append": {Out: "allow daemon logfile:file { append getattr ioctl lock };"},
	})
	f := Analyzer{R: r}.Analyze(context.Background(), Group{AVC: a, Count: 1})
	cmds, _ := f.Actions[0].Commands()
	if f.Class != ClassMislabeled || cmds[0].String() != "restorecon -v /var/log/nginx/site.log" {
		t.Fatalf("class %s cmds %v", f.Class, cmds)
	}
}

func TestSuspiciousIsNeverFixed(t *testing.T) {
	a, _ := ParseAVC(`avc:  denied  { read } for  pid=1 comm="nginx" name="shadow" dev="vda3" ino=9 scontext=system_u:system_r:httpd_t:s0 tcontext=system_u:object_r:shadow_t:s0 tclass=file permissive=0`)
	a.Path = "/etc/shadow"
	r := fake(map[string]runner.Result{"matchpathcon -n /etc/shadow": {Out: "system_u:object_r:shadow_t:s0"}})
	f := Analyzer{R: r}.Analyze(context.Background(), Group{AVC: a, Count: 1})
	if f.Class != ClassSuspicious || len(f.Actions) != 0 {
		t.Fatalf("class %s actions %+v", f.Class, f.Actions)
	}
}

func TestAnalyzePathHiddenDenial(t *testing.T) {
	labels := map[string]string{
		"/srv":                       "system_u:object_r:var_t:s0",
		"/srv/nginx-logs":            "unconfined_u:object_r:admin_home_t:s0",
		"/srv/nginx-logs/access.log": "unconfined_u:object_r:admin_home_t:s0",
	}
	r := fake(map[string]runner.Result{
		"sesearch -A -s httpd_t -t var_t -c dir -p search":        {Out: "allow httpd_t file_type:dir { getattr open search };"},
		"sesearch -A -s httpd_t -t admin_home_t -c dir -p search": {Out: "allow domain admin_home_t:dir { getattr open search };"},
		"matchpathcon -n /srv/nginx-logs/access.log":              {Out: "system_u:object_r:var_t:s0"},
		"sesearch -A -s httpd_t -t httpd_log_t -c file -p append": {Out: "allow daemon logfile:file { append getattr ioctl lock };"},
		"test -d /srv/nginx-logs/access.log":                      {Code: 1},
	})
	f, ok := Analyzer{R: r}.AnalyzePath(context.Background(), "httpd_t", "/srv/nginx-logs/access.log", true,
		func(p string) string { return labels[p] })
	if !ok {
		t.Fatal("no finding")
	}
	cmds, _ := f.Actions[0].Commands()
	if f.Class != ClassMissingFcontext || cmds[0].String() != `semanage fcontext -a -t httpd_log_t '/srv/nginx-logs(/.*)?'` {
		t.Fatalf("class %s cmds %v features %v", f.Class, cmds, f.Features)
	}
	if !f.Features["no_avc"] {
		t.Error("no_avc feature missing")
	}
}
