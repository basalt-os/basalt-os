package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

const applyUnit = "basalt-apply@p-f3604a_b7c416ad.service"

// executorEngine is an engine whose systemctl show describes a failed
// executor unit with the given result and main process code.
func executorEngine(t *testing.T, result, code string) (*Engine, proposal.Store) {
	dir := t.TempDir()
	show := "Id=" + applyUnit + "\nLoadState=loaded\nActiveState=failed\nSubState=failed\nResult=" + result +
		"\nExecMainCode=" + code + "\nExecMainStatus=1\nNRestarts=0\nFragmentPath=/usr/lib/systemd/system/basalt-apply@.service\n"
	fake := &runner.Fake{Default: &runner.Result{}, Prefixes: map[string]runner.Result{
		"systemctl show --timestamp=unix": {Out: show},
	}}
	al := audit.New(dir+"/audit.jsonl", "basalt-assistantd")
	al.Journal = false
	layer := decide.NewRules(al, nil)
	env := &diag.Env{R: fake, Confined: true, Now: time.Now, Decide: layer, SnapshotDir: dir,
		Glob: func(string) []string { return nil }, Label: func(string) string { return "" },
		Statfs:   func(p string) (diag.FSStat, error) { return diag.FSStat{Total: 100, Free: 90}, nil },
		ReadFile: func(string) ([]byte, error) { return nil, io.EOF }, HistoryPath: dir + "/h.jsonl"}
	cfg := config.Defaults()
	cfg.StateDir, cfg.MaxPerHour, cfg.AVCSettle, cfg.UnitSettle = dir, 20, time.Hour, time.Hour
	st := proposal.Store{Dir: dir + "/proposals"}
	return New(cfg, env, st, al, layer, io.Discard), st
}

func saveOriginal(t *testing.T, st proposal.Store, status string) {
	t.Helper()
	now := time.Now().UTC()
	p := &proposal.Proposal{ID: "p-f3604a", Created: now, Updated: now, LastSeen: now, Source: "cli", Kind: "update",
		Subject: "updates", Key: "update:all", Title: "install 12 updates", Status: status}
	if err := st.Save(p); err != nil {
		t.Fatal(err)
	}
}

func failExecutor(g *Engine) {
	g.onEntry(journal.Entry{PID: 1, Unit: applyUnit, MessageID: journal.MsgUnitFailed,
		Message: applyUnit + ": Failed with result 'exit-code'."})
	g.flush()
}

// Never a unit proposal named after the assistant's own executor unit.
func TestExecutorNeverRawUnitProposal(t *testing.T) {
	for _, tc := range []struct {
		name, result, code, original string
		want                         int // proposals besides the original
	}{
		{"record has the outcome", "exit-code", "1", proposal.Failed, 0},
		{"interrupted by a signal", "signal", "2", proposal.Pending, 0},
		{"failed without a record", "exit-code", "1", proposal.Pending, 1},
		{"unknown proposal", "exit-code", "1", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, st := executorEngine(t, tc.result, tc.code)
			if tc.original != "" {
				saveOriginal(t, st, tc.original)
			}
			failExecutor(g)
			ps, _ := st.List("")
			var other []*proposal.Proposal
			for _, p := range ps {
				if strings.Contains(p.Title, "basalt-apply@") || strings.Contains(p.Title, "cause unknown") {
					t.Errorf("raw unit proposal: %q", p.Title)
				}
				if p.ID != "p-f3604a" {
					other = append(other, p)
				}
			}
			if len(other) != tc.want {
				t.Fatalf("%d new proposals, want %d", len(other), tc.want)
			}
			if tc.want == 0 {
				return
			}
			p := other[0]
			if p.Kind != "apply" || len(p.Actions) != 0 || !p.NeedsReview {
				t.Errorf("report: %+v", p)
			}
			if tc.original != "" && p.Title != `Applying "install 12 updates" did not finish` {
				t.Errorf("title %q", p.Title)
			}
			if tc.original == "" && p.Title != "Applying the change p-f3604a did not finish" {
				t.Errorf("title %q", p.Title)
			}
			if !strings.Contains(p.Report, "p-f3604a") || !strings.Contains(p.Report, "Nothing will be changed") {
				t.Errorf("report text %q", p.Report)
			}
		})
	}
}

// A unit proposal an older assistant filed about its executor is closed.
func TestExecutorOldProposalClosed(t *testing.T) {
	g, st := executorEngine(t, "exit-code", "1")
	now := time.Now().UTC()
	old := &proposal.Proposal{ID: "p-0a0b0c", Created: now, Updated: now, LastSeen: now, Source: "daemon", Kind: "unit",
		Subject: applyUnit, Key: "unit:" + applyUnit + ":unknown", Title: applyUnit + " stopped: cause unknown", Status: proposal.Pending}
	if err := st.Save(old); err != nil {
		t.Fatal(err)
	}
	g.resolveUnits(t.Context())
	p, err := st.Load("p-0a0b0c")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != proposal.Resolved {
		t.Errorf("status %s, want resolved", p.Status)
	}
}

// A long transaction is not a failed one: once the pre snapshot has its
// post, the "unfinished transaction" proposal is closed.
func TestLongTransactionResolvedByItsPost(t *testing.T) {
	g, st := executorEngine(t, "exit-code", "1")
	dir := t.TempDir()
	info := func(num int, typ string, pre int) {
		d := filepath.Join(dir, strconv.Itoa(num))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		x := fmt.Sprintf("<snapshot><type>%s</type><num>%d</num><date>2026-10-20 21:51:44</date><pre_num>%d</pre_num><description>dnf</description></snapshot>", typ, num, pre)
		if err := os.WriteFile(filepath.Join(d, "info.xml"), []byte(x), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info(10, "pre", 0)
	g.Env.SnapshotDir = dir
	g.Env.Glob = func(p string) []string { m, _ := filepath.Glob(p); return m }
	g.Env.ReadFile = os.ReadFile
	now := time.Now().UTC()
	open := &proposal.Proposal{ID: "p-14bac3", Created: now, Updated: now, LastSeen: now, Source: "daemon", Kind: "dnf",
		Subject: "dnf -y upgrade --refresh", Key: "dnf:10", Title: "package transaction failed: dnf -y upgrade --refresh", Status: proposal.Pending}
	other := &proposal.Proposal{ID: "p-0c0c0c", Created: now, Updated: now, LastSeen: now, Source: "daemon", Kind: "dnf",
		Subject: "dnf", Key: "dnf:2026-10-20T21:00:00Z", Title: "x", Status: proposal.Pending}
	for _, p := range []*proposal.Proposal{open, other} {
		if err := st.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	g.resolveFinished(g.Env.Snapshots(t.Context()))
	if p, _ := st.Load("p-14bac3"); p.Status != proposal.Pending {
		t.Fatalf("closed before the post snapshot: %s", p.Status)
	}
	info(11, "post", 10)
	g.resolveFinished(g.Env.Snapshots(t.Context()))
	if p, _ := st.Load("p-14bac3"); p.Status != proposal.Resolved {
		t.Errorf("status %s, want resolved", p.Status)
	}
	if p, _ := st.Load("p-0c0c0c"); p.Status != proposal.Pending {
		t.Errorf("a failure found in dnf's log was closed: %s", p.Status)
	}
}
