package engine

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/journal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

func newEngine(t *testing.T, maxPerHour int) (*Engine, proposal.Store, string) {
	dir := t.TempDir()
	show, _ := os.ReadFile("../../testdata/nginx-config-error.show.txt")
	jr, _ := os.ReadFile("../../testdata/nginx-config-error.journal.json")
	fake := &runner.Fake{Default: &runner.Result{}, Prefixes: map[string]runner.Result{
		"systemctl show --timestamp=unix":                {Out: string(show)},
		"journalctl --no-pager -o json -u":               {Out: string(jr)},
		"journalctl --no-pager -o json _TRANSPORT=audit": {},
	}}
	al := audit.New(dir+"/audit.jsonl", "basalt-assistantd")
	al.Journal = false
	layer := decide.NewRules(al, nil)
	env := &diag.Env{R: fake, Confined: true, Now: time.Now, Decide: layer, SnapshotDir: dir,
		Glob: func(string) []string { return nil }, Label: func(string) string { return "" },
		Statfs:   func(p string) (diag.FSStat, error) { return diag.FSStat{Total: 100, Free: 90}, nil },
		ReadFile: os.ReadFile, HistoryPath: dir + "/h.jsonl"}
	cfg := config.Defaults()
	cfg.StateDir, cfg.MaxPerHour, cfg.AVCSettle, cfg.UnitSettle = dir, maxPerHour, time.Hour, time.Hour
	st := proposal.Store{Dir: dir + "/proposals"}
	return New(cfg, env, st, al, layer, io.Discard), st, dir
}

func TestUnitFailureDedup(t *testing.T) {
	g, st, dir := newEngine(t, 20)
	jr, _ := os.ReadFile("../../testdata/nginx-config-error.journal.json")
	for _, e := range journal.ParseAll(string(jr)) {
		g.onEntry(e) // two failure records for the same unit
	}
	g.flush()
	g.onEntry(journal.Entry{PID: 1, Unit: "nginx.service", MessageID: journal.MsgUnitFailed, Message: "nginx.service: Failed with result 'exit-code'."})
	g.flush()
	ps, _ := st.List("")
	if len(ps) != 1 {
		t.Fatalf("%d proposals, want 1", len(ps))
	}
	if ps[0].Seen != 2 || ps[0].Source != "daemon" || ps[0].Kind != "unit" {
		t.Fatalf("proposal %+v", ps[0])
	}
	if g.Stats()["deduplicated"] != 1 {
		t.Errorf("stats %v", g.Stats())
	}
	// Every decision of the proposal was logged in the chain.
	rs, _ := audit.Tail(dir+"/audit.jsonl", 0)
	var decisions int
	for _, r := range rs {
		if r.Type == "decision" {
			decisions++
		}
	}
	if decisions < 3 {
		t.Errorf("%d decisions logged", decisions)
	}
	if _, err := audit.Verify(dir + "/audit.jsonl"); err != nil {
		t.Error(err)
	}
}

func TestRateLimit(t *testing.T) {
	g, st, _ := newEngine(t, 1)
	for _, u := range []string{"a.service", "b.service", "c.service"} {
		g.onEntry(journal.Entry{PID: 1, Unit: u, MessageID: journal.MsgUnitFailed, Message: u + ": Failed with result 'exit-code'."})
	}
	g.flush()
	ps, _ := st.List("")
	if len(ps) != 1 || g.Stats()["rate_limited"] != 2 {
		t.Fatalf("%d proposals, stats %v", len(ps), g.Stats())
	}
}

func TestOwnMessagesIgnored(t *testing.T) {
	g, _, _ := newEngine(t, 20)
	g.onEntry(journal.Entry{Identifier: "basalt-assistant", PID: 1, Unit: "x.service", MessageID: journal.MsgUnitFailed})
	if len(g.batch) != 0 {
		t.Fatal("the daemon reacted to its own audit messages")
	}
}
