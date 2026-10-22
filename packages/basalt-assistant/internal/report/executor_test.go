package report

import (
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/titles"
)

func TestExecutorUnit(t *testing.T) {
	for unit, want := range map[string]bool{
		"basalt-apply@p-f3604a_b7c416ad.service":  true,
		"basalt-gate-exec@g-0123456789ab.service": true,
		"basalt-apply@p-f3604a.service":           false,
		"basalt-gate-exec@x.service":              false,
		"nginx.service":                           false,
		"basalt-assistantd.service":               false,
	} {
		if got := ExecutorUnit(unit); got != want {
			t.Errorf("ExecutorUnit(%q) = %v", unit, got)
		}
	}
}

func TestExecutorProposalID(t *testing.T) {
	if id := ExecutorProposalID(&diag.UnitReport{Unit: "basalt-apply@p-f3604a_b7c416ad.service"}); id != "p-f3604a" {
		t.Errorf("apply unit: %q", id)
	}
	gate := &diag.UnitReport{Unit: "basalt-gate-exec@g-0123456789ab.service",
		Journal: []string{"claimed request g-0123456789ab", "basalt apply p-c0ffee --gate: dnf failed"}}
	if id := ExecutorProposalID(gate); id != "p-c0ffee" {
		t.Errorf("gate unit: %q", id)
	}
	if id := ExecutorProposalID(&diag.UnitReport{Unit: "basalt-gate-exec@g-0123456789ab.service"}); id != "" {
		t.Errorf("no id: %q", id)
	}
}

func TestExecutorInterrupted(t *testing.T) {
	for _, tc := range []struct {
		result, code string
		want         bool
	}{
		{"signal", "2", true},
		{"timeout", "1", true},
		{"exit-code", "1", false},
		{"exit-code", "2", true},
		{"success", "1", false},
	} {
		r := &diag.UnitReport{State: map[string]string{"Result": tc.result, "ExecMainCode": tc.code}}
		if got := ExecutorInterrupted(r); got != tc.want {
			t.Errorf("%s/%s: %v", tc.result, tc.code, got)
		}
	}
}

func TestFromExecutorPlainWords(t *testing.T) {
	r := &diag.UnitReport{Unit: "basalt-apply@p-f3604a_b7c416ad.service",
		State:   map[string]string{"Result": "exit-code", "ExecMainStatus": "1"},
		Journal: []string{"12:00:01 basalt-apply-exec[42]: [error] Error: No match for argument: basalt-nonfree-release"}}
	orig := &proposal.Proposal{ID: "p-f3604a", Title: "turn the basalt-nonfree-testing channel on", Status: proposal.Pending}
	p := FromExecutor("daemon", r, orig)
	if p.Title != `Applying "turn the basalt-nonfree-testing channel on" did not finish` {
		t.Errorf("title %q", p.Title)
	}
	want := "Applying \"turn the basalt-nonfree-testing channel on\" stopped before it was done. The log says: Error: No match for argument: basalt-nonfree-release. " +
		"The change itself (p-f3604a) still waits for you: open it to try again, or dismiss it. Nothing will be changed by this report."
	if p.Report != want {
		t.Errorf("report\n got %q\nwant %q", p.Report, want)
	}
	if p.Key != "apply:p-f3604a" || len(p.Actions) != 0 {
		t.Errorf("key %q actions %d", p.Key, len(p.Actions))
	}
	// Without the proposal's record: named by its id, nothing said to wait.
	q := FromExecutor("daemon", &diag.UnitReport{Unit: r.Unit, State: r.State}, nil)
	if q.Title != "Applying the change p-f3604a did not finish" ||
		q.Report != "Applying the change p-f3604a stopped before it was done, and its log does not say why. Nothing will be changed by this report." {
		t.Errorf("no record: %q / %q", q.Title, q.Report)
	}
	// Shown translated sentence by sentence (internal/titles).
	for _, rep := range []string{p.Report, q.Report} {
		if !titles.KnownReport(rep) {
			t.Errorf("a sentence of this report is not in internal/titles: %q", rep)
		}
	}
}
