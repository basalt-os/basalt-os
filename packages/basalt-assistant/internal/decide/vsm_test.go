package decide

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/vsm"
)

// The test index (16 cases) and a tiny random planner speak the shipped
// DSL, so the backend's plumbing runs in CI without the real weights;
// the answers of the real planner are tested against the reference in
// internal/vsm.
const (
	testKnowledge = "../knowledge/testdata/index"
	testPlanner   = "../vsm/planner/testdata/tiny"
)

func TestVSMBackendAnswers(t *testing.T) {
	eng, err := vsm.Open(testKnowledge, testPlanner, knowledge.Context{Fedora: 44})
	if err != nil {
		t.Fatal(err)
	}
	b := NewVSMBackendFromEngine(eng)
	for _, q := range []Question{
		UnitCause("nginx.service", map[string]bool{"unit_failed": true, "config_check_failed": true}),
		AVCClass("httpd_t|unreserved_port_t|tcp_socket|name_bind|8181", map[string]bool{"port_case": true, "port_type_found": true}),
		DiskCause("/", map[string]bool{"journal_large": true}),
		DnfNext("dnf", map[string]bool{"rpmdb_unchanged": true}),
	} {
		a, err := b.Answer(context.Background(), q)
		if err != nil {
			t.Fatalf("%s: %v", q.ID, err)
		}
		sum := 0.0
		for _, o := range q.Options {
			sum += a.Probabilities[o]
		}
		if math.Abs(sum-1) > 0.01 || a.Top == "" || a.VSM == nil || !strings.Contains(a.VSM.Knowledge, "16 cases") {
			t.Errorf("%s: %+v %+v", q.ID, a, a.VSM)
		}
		if a.Backend != "vsm/basalt-os-dsl/2" {
			t.Errorf("backend %q", a.Backend)
		}
	}
	// Questions VSM does not answer go to the rules.
	a, err := b.Answer(context.Background(), Severity("x", map[string]bool{"unit_failed": true}))
	if err != nil || a.Backend != "rules/v1" {
		t.Errorf("severity: %v %v", a.Backend, err)
	}
	// A failed policy query: the cautious answer, whatever the codes say.
	a, _ = b.Answer(context.Background(), AVCClass("k", map[string]bool{"boolean_off": true, "policy_query_failed": true}))
	if a.Top != "unknown" || !a.VSM.Guarded {
		t.Errorf("policy query failed: %s %+v", a.Top, a.VSM)
	}
}

func TestVSMBackendFallsBackToRules(t *testing.T) {
	l := FromConfigFull(Config{Backend: "vsm", VSMKnowledge: t.TempDir(), VSMPlanner: t.TempDir()}, nil, nil, 0)
	d := l.Ask(context.Background(), UnitCause("nginx.service", map[string]bool{"config_check_failed": true}))
	if d.Answer.Backend != "rules/v1 (fallback)" || d.Answer.Top != "config_error" || d.Answer.FallbackReason == "" {
		t.Errorf("fallback: %+v", d.Answer)
	}
	// A planner that speaks another DSL is refused.
	if _, err := vsm.Open(testKnowledge, t.TempDir(), knowledge.Context{}); err == nil {
		t.Error("opened without a planner")
	}
}

func TestVSMHintIsNotConfident(t *testing.T) {
	b := fixedVSM{info: &VSMInfo{Case: "kb-avc-port-owned", Actionable: false}}
	l := &Layer{Backend: b, Default: 0.75}
	if d := l.Ask(context.Background(), AVCClass("k", nil)); d.Confident {
		t.Error("a hint case passed the threshold for avc.class")
	}
	b.info.Actionable = true
	if d := l.Ask(context.Background(), AVCClass("k", nil)); !d.Confident {
		t.Error("an actionable case did not")
	}
	b.info.Actionable = false
	if d := l.Ask(context.Background(), UnitCause("u", nil)); !d.Confident {
		t.Error("unit.cause is not gated by hint cases")
	}
}

type fixedVSM struct{ info *VSMInfo }

func (fixedVSM) Name() string { return "fixed" }

func (f fixedVSM) Answer(_ context.Context, q Question) (Answer, error) {
	return Answer{Probabilities: map[string]float64{q.Options[0]: 0.99}, Top: q.Options[0], Confidence: 0.99, VSM: f.info}, nil
}
