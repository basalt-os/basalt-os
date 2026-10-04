package diag

import (
	"context"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

func crashEnv(t *testing.T, unit, show, journal, history string) *testEnv {
	t.Helper()
	te := newEnv(t)
	te.fake.Prefixes["systemctl show --timestamp=unix"] = runner.Result{Out: fixture(t, show)}
	if journal != "" {
		te.fake.Prefixes["journalctl --no-pager -o json -u "+unit] = runner.Result{Out: fixture(t, journal)}
	}
	te.fake.Prefixes["journalctl --no-pager -o cat -u "+unit] = runner.Result{Out: fixture(t, history)}
	te.labels["/bin/sh"] = "system_u:object_r:shell_exec_t:s0"
	return te
}

func noPolicyQueries(t *testing.T, te *testEnv) {
	t.Helper()
	for _, q := range te.fake.Asked {
		if strings.HasPrefix(q, "sesearch") || strings.HasPrefix(q, "seinfo") {
			t.Errorf("a crash without denials walked the policy: %s", q)
		}
	}
}

// A program that kills itself with SIGSEGV as it starts (the lab's
// basalt-lab-segv): the same core dump as on an earlier run, 0 s after it
// started. No restart: it would crash the same way and fail verification.
func TestCrashAtStartProposesNoRestart(t *testing.T) {
	te := crashEnv(t, "basalt-lab-segv.service", "crash-segv.show.txt", "crash-segv.journal.json", "crash-segv.history.txt")
	rep, err := WhyUnit(context.Background(), te.Env, "basalt-lab-segv")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "crashed" || rep.Crash == nil || !rep.Crash.Repeating {
		t.Fatalf("cause %s crash %+v", rep.Cause, rep.Crash)
	}
	c := rep.Crash
	if c.Code != "dumped" || c.Status != "11/SEGV" || c.Count != 2 || c.RanSeconds != 0 || c.RanFor != "0 s" {
		t.Fatalf("crash %+v", c)
	}
	if len(rep.Actions) != 0 || !strings.Contains(rep.Explanation, "keeps crashing") ||
		!strings.Contains(rep.Explanation, "coredumpctl info COREDUMP_UNIT=basalt-lab-segv.service") {
		t.Fatalf("actions %+v explanation %q", rep.Actions, rep.Explanation)
	}
	if rep.Domain != "" {
		t.Errorf("domain looked up for a crash: %q", rep.Domain)
	}
	noPolicyQueries(t, te)
}

// Restart=on-failure until the start limit: systemd's own restarts failed,
// so another one would too.
func TestCrashLoopStartLimit(t *testing.T) {
	te := crashEnv(t, "basalt-lab-loop.service", "crash-loop.show.txt", "", "crash-loop.history.txt")
	rep, err := WhyUnit(context.Background(), te.Env, "basalt-lab-loop")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Crash == nil || !rep.Crash.StartLimit || rep.Crash.Restarts != 5 || rep.Crash.Count != 2 || !rep.Crash.Repeating {
		t.Fatalf("cause %s crash %+v", rep.Cause, rep.Crash)
	}
	if len(rep.Actions) != 0 || !strings.Contains(rep.Explanation, "systemctl reset-failed basalt-lab-loop.service") {
		t.Fatalf("actions %+v explanation %q", rep.Actions, rep.Explanation)
	}
}

// A first SIGKILL after three hours of work is plausibly transient: the
// restart is still proposed.
func TestFirstCrashAfterLongRunProposesRestart(t *testing.T) {
	te := crashEnv(t, "worker.service", "crash-once.show.txt", "", "crash-once.history.txt")
	rep, err := WhyUnit(context.Background(), te.Env, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cause != "crashed" || rep.Crash == nil || rep.Crash.Repeating || rep.Crash.RanFor != "3 h" || rep.Crash.Count != 1 {
		t.Fatalf("cause %s crash %+v", rep.Cause, rep.Crash)
	}
	if len(rep.Actions) != 1 || rep.Actions[0].Kind != action.UnitRestart || !strings.Contains(rep.Explanation, "after running for 3 h") {
		t.Fatalf("actions %+v explanation %q", rep.Actions, rep.Explanation)
	}
	noPolicyQueries(t, te)
}
