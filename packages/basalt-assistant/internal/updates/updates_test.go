package updates

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// fake answers commands by their first words.
type fake map[string]string

func (f fake) Read(ctx context.Context, argv ...string) runner.Result {
	key := strings.Join(argv, " ")
	for k, v := range f {
		if strings.HasPrefix(key, k) {
			return runner.Result{Out: v}
		}
	}
	return runner.Result{Code: 1}
}

// Real dnf5 5.4 output (Fedora 44 lab, 2026-10-06), with a Basalt and an
// app update added.
const repoquery = `basalt-shell-0:0.9.0-1.fc44.x86_64|basalt-shell|0.9.0-1.fc44|x86_64|basalt-testing|2345678|9876543|The Basalt OS desktop shell
firefox-0:149.0-1.fc44.x86_64|firefox|149.0-1.fc44|x86_64|updates|80000000|250000000|Mozilla Firefox Web browser
openssl-libs-1:3.5.9-1.fc44.x86_64|openssl-libs|1:3.5.9-1.fc44|x86_64|updates|3100000|9600000|A general purpose cryptography library with TLS implementation
tzdata-0:2026c-1.fc44.noarch|tzdata|2026c-1.fc44|noarch|updates|450000|1800000|Timezone data | with a separator in the summary
`

const advisories = `[
  {
    "name":"FEDORA-2026-12f3f3d569",
    "type":"security",
    "severity":"Important",
    "nevra":"openssl-libs-1:3.5.9-1.fc44.x86_64",
    "buildtime":1790989781
  },
  {
    "name":"FEDORA-2026-0000aaaa11",
    "type":"bugfix",
    "severity":"None",
    "nevra":"tzdata-2026c-1.fc44.noarch",
    "buildtime":1790989781
  }
]`

func testSys(t *testing.T) Sys {
	s := newSys(t)
	Refresh(context.Background(), s, time.Now())
	return s
}

func newSys(t *testing.T) Sys {
	d := t.TempDir()
	apps := filepath.Join(d, "applications")
	_ = os.MkdirAll(apps, 0o755)
	_ = os.WriteFile(filepath.Join(apps, "firefox.desktop"), []byte("[Desktop Entry]\n"), 0o644)
	boot := time.Unix(1790000000, 0)
	f := fake{
		"dnf repoquery":                 repoquery,
		"dnf advisory list":             advisories,
		"rpm -qf":                       "firefox\n",
		"rpm -qa":                       "openssl-libs.x86_64 1:3.5.8-1.fc44\nfirefox.x86_64 0:148.0-1.fc44\ntzdata.noarch 0:2026b-1.fc44\nbasalt-shell.x86_64 0:0.8.0-1.fc44\n",
		"rpm -q --qf":                   "kernel-core 1789000000 6.17.1-200.fc44.x86_64\nkernel-core 1790000500 6.17.3-200.fc44.x86_64\nglibc 1789000000 2.43-1.fc44.x86_64\n",
		"btrfs subvolume get-default":   "ID 256 gen 10 top level 5 path root",
		"btrfs inspect-internal rootid": "256",
		"uname -r":                      "6.17.1-200.fc44.x86_64",
	}
	return Sys{R: f, Store: proposal.Store{Dir: filepath.Join(d, "proposals")}, CachePath: filepath.Join(d, "updates.json"),
		BootID:  func() string { return "boot-1" },
		AppsDir: apps, BootTime: func() time.Time { return boot }}
}

func TestPreviewParsing(t *testing.T) {
	s := newSys(t)
	if r := Build(context.Background(), s); len(r.Updates) != 0 || r.Checked != "" {
		t.Fatalf("a report before any check: %+v", r)
	}
	Refresh(context.Background(), s, time.Time{})
	r := Build(context.Background(), s)
	if len(r.Errors) > 0 {
		t.Fatal(r.Errors)
	}
	if r.Counts["total"] != 4 || r.Counts[GroupSecurity] != 1 || r.Counts[GroupBasalt] != 1 || r.Counts[GroupApps] != 1 || r.Counts[GroupSystem] != 1 {
		t.Fatalf("counts %v", r.Counts)
	}
	by := map[string]Update{}
	for _, u := range r.Updates {
		by[u.Name] = u
	}
	o := by["openssl-libs"]
	if o.Group != GroupSecurity || o.Advisory != "FEDORA-2026-12f3f3d569" || o.Severity != "Important" || o.From != "1:3.5.8-1.fc44" || o.Download != 3100000 {
		t.Errorf("openssl: %+v", o)
	}
	if by["basalt-shell"].NEVRA != "basalt-shell-0.9.0-1.fc44.x86_64" || by["basalt-shell"].Group != GroupBasalt {
		t.Errorf("basalt-shell: %+v", by["basalt-shell"])
	}
	if by["tzdata"].Summary != "Timezone data | with a separator in the summary" || by["tzdata"].Group != GroupSystem {
		t.Errorf("tzdata: %+v", by["tzdata"])
	}
	if r.Download != 2345678+80000000+3100000+450000 {
		t.Errorf("download %d", r.Download)
	}
	if !r.Restart.Needed || len(r.Restart.Reasons) != 1 || !strings.HasPrefix(r.Restart.Reasons[0], "kernel-core 6.17.3") {
		t.Errorf("restart %+v", r.Restart)
	}
	if r.Checked != "" {
		t.Error("never checked, but a time is shown")
	}
}

func TestInstallProposal(t *testing.T) {
	s := testSys(t)
	us, _ := Query(context.Background(), s, true)
	p, err := InstallProposal(us, "cli", "all")
	if err != nil {
		t.Fatal(err)
	}
	a := p.Actions[0]
	if a.Kind != action.UpdateInstall || a.Params["count"] != "4" || p.Validate() != nil {
		t.Fatalf("%+v %v", a, p.Validate())
	}
	want := "basalt-shell-0.9.0-1.fc44.x86_64 firefox-149.0-1.fc44.x86_64 openssl-libs-1:3.5.9-1.fc44.x86_64 tzdata-2026c-1.fc44.noarch"
	if a.Params["packages"] != want {
		t.Errorf("packages %q", a.Params["packages"])
	}
	sec, err := InstallProposal(us, "cli", "security")
	if err != nil || sec.Actions[0].Params["packages"] != "openssl-libs-1:3.5.9-1.fc44.x86_64" {
		t.Fatalf("security: %v %v", sec, err)
	}
	if _, err := InstallProposal(nil, "cli", "all"); err == nil {
		t.Error("a proposal without updates")
	}
}

func TestRollbackProposalAndHistory(t *testing.T) {
	s := testSys(t)
	us, _ := Query(context.Background(), s, true)
	p, _ := InstallProposal(us, "cli", "all")
	p.Status = proposal.Applied
	p.Result = &proposal.Result{Time: time.Now().UTC(), OK: true, PreSnapshot: 41, PostSnapshot: 42}
	if err := s.Store.Save(p); err != nil {
		t.Fatal(err)
	}
	r := Build(context.Background(), s)
	if r.Undo == nil || r.Undo.Proposal != p.ID || r.Undo.Snapshot != 41 || r.Undo.Count != 4 {
		t.Fatalf("undo %+v", r.Undo)
	}
	rb, err := RollbackProposal(*r.Undo, "cli")
	if err != nil {
		t.Fatal(err)
	}
	a := rb.Actions[0]
	cmds, _ := a.Commands()
	if a.Kind != action.UpdateRollback || a.Params["snapshot"] != "41" || a.Params["proposal"] != p.ID || cmds[0].String() != "basalt-rollback --yes 41" {
		t.Fatalf("%+v %v", a, cmds)
	}
	// From the confined view a rollback is only a hint.
	rb.Source = "daemon"
	if rb.Validate() == nil {
		t.Error("a rollback proposal from the daemon validated")
	}
	rb.Source = "cli"
	rb.Status = proposal.Applied
	if err := s.Store.Save(rb); err != nil {
		t.Fatal(err)
	}
	r = Build(context.Background(), s)
	if r.Undo != nil {
		t.Errorf("an undone update can be undone again: %+v", r.Undo)
	}
	if len(r.History) != 2 || r.History[1].Status != "undone" && r.History[0].Status != "undone" {
		t.Errorf("history %+v", r.History)
	}
}

func TestRunningProgress(t *testing.T) {
	s := testSys(t)
	us, _ := Query(context.Background(), s, true)
	p, _ := InstallProposal(us, "cli", "all")
	_ = s.Store.Save(p)
	old := proposal.ProgressDir
	proposal.ProgressDir = t.TempDir()
	defer func() { proposal.ProgressDir = old }()
	proposal.WriteProgress(p.ID, 2, 4, "download 4 updates", time.Now())
	r := Build(context.Background(), s)
	if r.Running == nil || r.Running.Step != 2 || r.Running.Steps != 4 || r.Pending != "" {
		t.Errorf("running %+v pending %q", r.Running, r.Pending)
	}
	// The report reads the cache only; nothing queries dnf while it runs.
	proposal.WriteProgress(p.ID, 0, 0, "", time.Now())
	r = Build(context.Background(), s)
	if r.Running != nil || r.Pending != p.ID || len(r.Updates) != 4 {
		t.Errorf("after the apply: running %+v pending %q updates %d", r.Running, r.Pending, len(r.Updates))
	}
}

// After an undo the proposals go back with the root; the audit log on
// /var/log keeps the history.
func TestHistoryFromAuditSurvivesRollback(t *testing.T) {
	s := testSys(t)
	s.AuditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	log := audit.New(s.AuditPath, "basalt")
	us, _ := Query(context.Background(), s, true)
	p, _ := InstallProposal(us, "cli", "all")
	t0 := time.Now().UTC().Add(-time.Hour)
	if _, err := log.Append("apply", "x", map[string]any{"proposal": p.ID, "actions": p.Actions,
		"result": proposal.Result{Time: t0, OK: false, PreSnapshot: 75, PostSnapshot: 78}}); err != nil {
		t.Fatal(err)
	}
	r := Build(context.Background(), s)
	if r.Undo == nil || r.Undo.Snapshot != 75 || len(r.History) != 1 || r.History[0].Status != "failed" {
		t.Fatalf("undo %+v history %+v", r.Undo, r.History)
	}
	rb, _ := RollbackProposal(*r.Undo, "cli")
	_, _ = log.Append("apply", "y", map[string]any{"proposal": rb.ID, "actions": rb.Actions,
		"result": proposal.Result{Time: t0.Add(time.Minute), OK: true}})
	r = Build(context.Background(), s)
	if r.Undo != nil || len(r.History) != 2 || r.History[0].Kind != "rollback" || r.History[1].Status != "undone" {
		t.Errorf("undo %+v history %+v", r.Undo, r.History)
	}
	// A new try under the same id (the stored proposal came back with the
	// root) is not undone by the first rollback.
	_, _ = log.Append("apply", "z", map[string]any{"proposal": p.ID, "actions": p.Actions,
		"result": proposal.Result{Time: t0.Add(2 * time.Minute), OK: true, PreSnapshot: 81, PostSnapshot: 82}})
	r = Build(context.Background(), s)
	if r.Undo == nil || r.Undo.Snapshot != 81 || r.History[0].Status != "applied" {
		t.Errorf("second try: undo %+v history %+v", r.Undo, r.History)
	}
}

// An apply that never finished (the computer was reset while dnf hung)
// keeps its snapshot: the update shows as interrupted and can be undone.
func TestInterruptedApplyCanBeUndone(t *testing.T) {
	s := testSys(t)
	us, _ := Query(context.Background(), s, true)
	p, _ := InstallProposal(us, "cli", "all")
	_ = s.Store.Save(p)
	s.PreSnapshots = func() map[string]int { return map[string]int{p.ID: 87} }
	r := Build(context.Background(), s)
	if r.Undo == nil || r.Undo.Snapshot != 87 || len(r.History) != 1 || r.History[0].Status != "interrupted" || r.Pending != "" {
		t.Fatalf("undo %+v history %+v pending %q", r.Undo, r.History, r.Pending)
	}
}

// A set with core packages installs offline; a small set without them
// stays live; the report says which.
func TestOfflineMode(t *testing.T) {
	s := testSys(t)
	r := Build(context.Background(), s)
	if !r.Offline || r.OfflineSecurity {
		t.Fatalf("offline %v security %v core %v", r.Offline, r.OfflineSecurity, r.Core)
	}
	us, _ := Query(context.Background(), s, true)
	us = append(us, Update{Name: "kernel-core", NEVRA: "kernel-core-6.17.3-200.fc44.x86_64", Group: GroupSystem})
	p, err := InstallProposal(us, "cli", "all")
	if err != nil {
		t.Fatal(err)
	}
	a := p.Actions[0]
	cmds, _ := a.Commands()
	if a.Params["mode"] != "offline" || len(cmds) != 2 || !cmds[1].AfterRecord || cmds[1].String() != "dnf -y offline reboot" {
		t.Fatalf("%+v %v", a.Params["mode"], cmds)
	}
	sec, _ := InstallProposal(us, "cli", "security")
	if sec.Actions[0].Params["mode"] != "live" {
		t.Errorf("security set without core packages: %s", sec.Actions[0].Params["mode"])
	}
}
