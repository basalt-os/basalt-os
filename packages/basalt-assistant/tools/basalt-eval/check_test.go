package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// casesDir finds eval/cases: BASALT_EVAL_CASES (set by build.sh test, which
// mounts the directory into the container) or the repository layout.
func casesDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("BASALT_EVAL_CASES"); d != "" {
		return d
	}
	d := filepath.Join("..", "..", "..", "..", "eval", "cases")
	if _, err := os.Stat(d); err != nil {
		t.Skip("eval/cases not available (package built outside the repository)")
	}
	return d
}

// Every expected action in every case file passes the assistant's own
// validators, and every snapshot it names is in the case's evidence.
func TestAllCasesValidate(t *testing.T) {
	n, err := checkDir(casesDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no cases checked")
	}
	t.Logf("%d cases checked", n)
}

func TestCheckCase(t *testing.T) {
	ok := []string{
		// v1: no snapshot evidence required.
		`{"schema":"basalt-case/v1","id":"a","expected":{"actions":[{"kind":"file.restore","params":{"path":"/etc/x.conf","snapshot":"5"}}],"proposal":"propose"}}`,
		`{"schema":"basalt-case/v1.1","id":"b","evidence":{"snapshots":[{"number":5,"role":"restore_source","path":"/etc/x.conf"}]},
		  "expected":{"actions":[{"kind":"file.restore","params":{"path":"/etc/x.conf","snapshot":"5"}}],"proposal":"propose"}}`,
		`{"schema":"basalt-case/v1.1","id":"c","evidence":{"snapshots":[{"number":63,"role":"transaction_pre","type":"pre"},
		  {"number":64,"role":"transaction_post","type":"post","pre_number":63}]},
		  "expected":{"actions":[{"kind":"snapshot.rollback","params":{"snapshot":"63"}}],"proposal":"propose"}}`,
		`{"schema":"basalt-case/v1.1","id":"d","expected":{"actions":[],"proposal":"review"}}`,
	}
	for _, c := range ok {
		if _, err := checkCase([]byte(c)); err != nil {
			t.Errorf("want ok, got %v for %s", err, c)
		}
	}
	bad := map[string]string{
		`{"schema":"basalt-case/v1.1","id":"e","expected":{"actions":[{"kind":"selinux.fcontext","params":{"dir":"/data/logs","type":"httpd_log_t"}}],"proposal":"propose"}}`:      "path",
		`{"schema":"basalt-case/v1.1","id":"f","expected":{"actions":[{"kind":"selinux.boolean","params":{"name":"httpd_can_network_relay"}}],"proposal":"propose"}}`:              "boolean value",
		`{"schema":"basalt-case/v1.1","id":"g","expected":{"actions":[{"kind":"selinux.port","params":{"type":"http_port_t","proto":"tcp","port":"8085"}}],"proposal":"propose"}}`: "mode",
		`{"schema":"basalt-case/v1.1","id":"h","expected":{"actions":[{"kind":"file.restore","params":{"path":"/etc/x.conf"}}],"proposal":"propose"}}`:                             "snapshot",
		`{"schema":"basalt-case/v1.1","id":"i","expected":{"actions":[{"kind":"file.restore","params":{"path":"/etc/x.conf","snapshot":"5"}}],"proposal":"propose"}}`:              "not in evidence.snapshots",
		`{"schema":"basalt-case/v1.1","id":"j","evidence":{"snapshots":[{"number":64,"role":"transaction_post","type":"post","pre_number":63}]},
		  "expected":{"actions":[{"kind":"snapshot.rollback","params":{"snapshot":"64"}}],"proposal":"propose"}}`: "pre snapshot",
		`{"schema":"basalt-case/v2","id":"k","expected":{"actions":[],"proposal":"review"}}`:  "schema",
		`{"schema":"basalt-case/v1.1","id":"l","expected":{"actions":[],"proposal":"maybe"}}`: "proposal",
	}
	for c, want := range bad {
		_, err := checkCase([]byte(c))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v for %s", want, err, c)
		}
	}
}
