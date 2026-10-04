package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary doubles as the sandbox helper (as /usr/bin/basalt does).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == Command {
		os.Exit(Main(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestMountTable(t *testing.T) {
	b, err := os.ReadFile("../../testdata/mountinfo-fedora.txt")
	if err != nil {
		t.Fatal(err)
	}
	ms := Order(ParseMountinfo(string(b)))
	if len(ms) != 19 || ms[0].Point != "/" {
		t.Fatalf("%d mounts, first %+v", len(ms), ms[0])
	}
	pos := map[string]int{}
	kinds := map[string]string{}
	for i, m := range ms {
		pos[m.Point], kinds[m.Point] = i, Kind(m)
	}
	// Parents before children.
	for _, pair := range [][2]string{{"/sys", "/sys/fs/selinux"}, {"/dev", "/dev/shm"}, {"/boot", "/boot/efi"}, {"/run", "/run/credentials/systemd-journald.service"}} {
		if pos[pair[0]] > pos[pair[1]] {
			t.Errorf("%s after %s", pair[0], pair[1])
		}
	}
	want := map[string]string{"/": "overlay", "/var/log": "overlay", "/boot/efi": "overlay", "/tmp": "overlay", "/dev/shm": "overlay",
		"/proc": "bind", "/sys/fs/selinux": "bind", "/dev": "bind", "/proc/sys/fs/binfmt_misc": "skip",
		"/run/credentials/systemd-journald.service": "bind-ro", "/srv data": "overlay"}
	for p, k := range want {
		if kinds[p] != k {
			t.Errorf("%s: %s, want %s", p, kinds[p], k)
		}
	}
}

func TestWrapAndParse(t *testing.T) {
	argv := Wrap("/usr/bin/basalt", []string{"nginx", "-t"}, map[string]string{"/etc/nginx/nginx.conf": "/.snapshots/3/snapshot/etc/nginx/nginx.conf"})
	want := "/usr/bin/basalt __sandbox --replace /etc/nginx/nginx.conf=/.snapshots/3/snapshot/etc/nginx/nginx.conf -- nginx -t"
	if strings.Join(argv, " ") != want {
		t.Fatalf("%q", argv)
	}
	o, err := parse(argv[2:])
	if err != nil || o.replace["/etc/nginx/nginx.conf"] == "" || strings.Join(o.argv, " ") != "nginx -t" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{{"--"}, {"--replace", "etc=x", "--", "true"}, {"--replace", "/a/../b=/c", "--", "true"}, {"true"}} {
		if _, err := parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if !Failed(FailCode, Prefix+" needs root") || Failed(1, Prefix+" x") || Failed(FailCode, "nginx: test failed") {
		t.Error("Failed")
	}
}

// Runs only as root on a real system (the lab VM): a check that creates
// files and writes over an existing one leaves no trace, and sees a
// replaced file in place of the real one.
func TestNoSideEffects(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("BASALT_SANDBOX_TEST") != "1" {
		t.Skip("needs root on a real system: BASALT_SANDBOX_TEST=1 as root")
	}
	dir, err := os.MkdirTemp("/var/tmp", "basalt-sandbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	keep := filepath.Join(dir, "keep.conf")
	alt := filepath.Join(dir, "alt.conf")
	if err := os.WriteFile(keep, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alt, []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "set -e; cat " + keep + "; mkdir -p " + dir + "/newdir; echo x > " + dir + "/newdir/new.log; " +
		"echo appended >> " + keep + "; touch /etc/basalt-sandbox-test-marker; echo done"
	argv := Wrap(os.Args[0], []string{"sh", "-c", script}, map[string]string{keep: alt})
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "replacement") || !strings.Contains(string(out), "done") {
		t.Fatalf("output %q", out)
	}
	if b, _ := os.ReadFile(keep); string(b) != "original\n" {
		t.Errorf("the real file changed: %q", b)
	}
	for _, p := range []string{dir + "/newdir", "/etc/basalt-sandbox-test-marker"} {
		if _, err := os.Stat(p); err == nil {
			os.RemoveAll(p)
			t.Errorf("%s was created on the real system", p)
		}
	}
}
