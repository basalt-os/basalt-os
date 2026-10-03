package decide

import (
	"context"
	"math"
	"testing"
)

type memLog struct{ ds []Decision }

func (m *memLog) LogDecision(d Decision) error { m.ds = append(m.ds, d); return nil }

func sum(a Answer) float64 {
	s := 0.0
	for _, v := range a.Probabilities {
		s += v
	}
	return s
}

func TestEveryQuestionGivesADistribution(t *testing.T) {
	qs := []Question{
		UnitCause("x.service", map[string]bool{"config_check_failed": true}),
		AVCClass("k", map[string]bool{"port_case": true, "port_type_found": true}),
		Severity("k", map[string]bool{"unit_failed": true}),
		Notify("k", map[string]bool{"severity_high": true}),
		DnfNext("k", map[string]bool{"pre_without_post": true}),
		DiskCause("/", map[string]bool{"journal_large": true}),
		RouteBigger("k", nil),
	}
	l := NewRules(nil, nil)
	for _, q := range qs {
		d := l.Ask(context.Background(), q)
		if math.Abs(sum(d.Answer)-1) > 0.01 || len(d.Answer.Probabilities) != len(q.Options) {
			t.Errorf("%s: %v", q.ID, d.Answer.Probabilities)
		}
	}
}

func TestEvidenceMovesTheAnswer(t *testing.T) {
	l := NewRules(nil, nil)
	none := l.Ask(context.Background(), AVCClass("k", nil))
	if none.Confident || none.Answer.Top != "unknown" {
		t.Errorf("no evidence: %s confident=%v", none.Answer.String(), none.Confident)
	}
	mis := l.Ask(context.Background(), AVCClass("k", map[string]bool{"default_differs": true, "default_allowed": true, "has_path": true}))
	if !mis.Confident || mis.Answer.Top != "mislabeled" || mis.Answer.Confidence < 0.85 {
		t.Errorf("mislabeled: %s", mis.Answer.String())
	}
	sus := l.Ask(context.Background(), AVCClass("k", map[string]bool{"sensitive_target": true, "default_differs": true, "default_allowed": true}))
	if sus.Answer.Top != "suspicious" {
		t.Errorf("a sensitive target must win: %s", sus.Answer.String())
	}
	sev := l.Ask(context.Background(), Severity("k", map[string]bool{"disk_crit": true}))
	if sev.Answer.Expected() < 4 {
		t.Errorf("critical disk severity %.2f", sev.Answer.Expected())
	}
}

func TestThresholdsAndLogging(t *testing.T) {
	log := &memLog{}
	l := NewRules(log, map[string]float64{"avc.class": 0.95})
	d := l.Ask(context.Background(), AVCClass("k", map[string]bool{"port_case": true, "port_type_found": true}))
	if d.Confident || d.Threshold != 0.95 {
		t.Fatalf("threshold not applied: %+v", d)
	}
	l.Record(d, "review")
	if len(log.ds) != 1 || log.ds[0].Action != "review" || log.ds[0].Question.ID != "avc.class" {
		t.Fatalf("logged %+v", log.ds)
	}
}

func TestModelBackendFallsBackToRules(t *testing.T) {
	l := FromConfig("openai-compatible", "unix:/nonexistent/basalt-llm.sock", "m", nil, nil, 0.75)
	d := l.Ask(context.Background(), UnitCause("x", map[string]bool{"journal_address_in_use": true}))
	if d.Answer.Top != "port_conflict" || d.Answer.Backend != "rules/v1 (fallback)" {
		t.Fatalf("fallback: %+v", d.Answer)
	}
	if _, err := (Rules{}).Answer(context.Background(), Question{ID: "nope", Options: []string{"a"}}); err == nil {
		t.Error("unknown question accepted")
	}
}
