package selinux

import (
	"context"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// Paths on /proc and /sys get no label analysis: no matchpathcon (it would
// lstat other processes' /proc entries: getattr denials of the confined
// domain) and no relabel proposal.
func TestPseudoPathsAreNotLabelChecked(t *testing.T) {
	for p, want := range map[string]bool{"/proc": true, "/proc/1234/status": true, "/sys/fs/cgroup": true,
		"/procfs": false, "/srv/proc": false, "/system": false} {
		if PseudoPath(p) != want {
			t.Errorf("PseudoPath(%q) = %v", p, !want)
		}
	}
	z := Analyzer{R: failRunner{t}}
	if d := z.defaultType(context.Background(), "/proc/1/environ"); d != "" {
		t.Errorf("default type %q", d)
	}
	if _, found := z.AnalyzePath(context.Background(), "httpd_t", "/proc/1/environ", false, func(string) string { return "" }); found {
		t.Error("a /proc path was label-checked")
	}
}

type failRunner struct{ t *testing.T }

func (f failRunner) Read(_ context.Context, argv ...string) runner.Result {
	f.t.Errorf("ran %v", argv)
	return runner.Result{Code: 1}
}
