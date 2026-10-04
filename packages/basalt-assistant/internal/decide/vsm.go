package decide

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/vsm"
)

// VSMBackend answers the four cause and class questions (unit.cause,
// avc.class, disk.cause, dnf.next) with VSM (ADR 0004): the knowledge
// index ranks the cases that match the diagnosers' findings, the planner
// judges (accept, pick another, abstain), and a deterministic guard
// outside the model turns a pick the evidence does not fully support into
// the cautious option. Severity, notification and routing stay with the
// rules. It is opt-in (decision.backend = vsm); the rules stay the default
// and answer, marked "(fallback)", whenever the knowledge or the planner
// cannot be loaded or a question takes too long.
//
// The knowledge (package basalt-knowledge) and the planner weights
// (package basalt-vsm-planner) are data; they are loaded on the first
// question and kept. A load error is retried at most once a minute.
type VSMBackend struct {
	KnowledgeDir string // the index for this Fedora release
	PlannerDir   string
	Fedora       int
	Timeout      time.Duration // per question (default 2 s)

	mu      sync.Mutex
	eng     *vsm.Engine
	lastErr error
	lastTry time.Time
	open    func(k, p string, ctx knowledge.Context) (*vsm.Engine, error)
}

// NewVSMBackend builds the backend for the default package locations
// (empty paths) or the given ones.
func NewVSMBackend(knowledgeRoot, knowledgeDir, plannerDir string) *VSMBackend {
	fed := vsm.FedoraRelease()
	if knowledgeDir == "" {
		knowledgeDir = vsm.KnowledgeDir(knowledgeRoot, fed)
	}
	if plannerDir == "" {
		plannerDir = vsm.DefaultPlannerDir
	}
	return &VSMBackend{KnowledgeDir: knowledgeDir, PlannerDir: plannerDir, Fedora: fed, Timeout: 2 * time.Second}
}

// NewVSMBackendFromEngine wraps an engine that is already loaded (tests,
// the evaluation tool).
func NewVSMBackendFromEngine(e *vsm.Engine) *VSMBackend {
	return &VSMBackend{eng: e, Timeout: 2 * time.Second}
}

// Name implements Backend.
func (b *VSMBackend) Name() string { return "vsm/" + vsm.DSLVersion }

// Engine loads (once) and returns the engine.
func (b *VSMBackend) Engine() (*vsm.Engine, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eng != nil {
		return b.eng, nil
	}
	if b.lastErr != nil && time.Since(b.lastTry) < time.Minute {
		return nil, b.lastErr
	}
	open := b.open
	if open == nil {
		open = vsm.Open
	}
	b.lastTry = time.Now()
	e, err := open(b.KnowledgeDir, b.PlannerDir, knowledge.Context{Fedora: b.Fedora})
	if err != nil {
		b.lastErr = fmt.Errorf("vsm unavailable: %v", err)
		return nil, b.lastErr
	}
	b.eng, b.lastErr = e, nil
	return e, nil
}

// Answer implements Backend.
func (b *VSMBackend) Answer(ctx context.Context, q Question) (Answer, error) {
	if !ModelQuestions[q.ID] {
		return Rules{}.Answer(ctx, q)
	}
	opts := vsm.Options[q.ID]
	if len(q.Options) != len(opts) {
		return Answer{}, fmt.Errorf("vsm: options of %s differ from the planner's", q.ID)
	}
	for i, o := range q.Options {
		if opts[i] != o {
			return Answer{}, fmt.Errorf("vsm: options of %s differ from the planner's", q.ID)
		}
	}
	e, err := b.Engine()
	if err != nil {
		return Answer{}, err
	}
	if err := ctx.Err(); err != nil {
		return Answer{}, err
	}
	t0 := time.Now()
	r, err := e.Ask(q.ID, q.Subject, q.Features, q.Facts)
	if err != nil {
		return Answer{}, err
	}
	took := time.Since(t0)
	if b.Timeout > 0 && took > b.Timeout {
		return Answer{}, fmt.Errorf("vsm: %s took %s", q.ID, took.Round(time.Millisecond))
	}
	a := finish(r.Probabilities, b.Name())
	v := &VSMInfo{Knowledge: e.Version(), Abstained: r.Abstained, Guarded: r.Guarded, Evidence: r.Evidence,
		Probes: r.Probes, Note: r.Note, Micros: took.Microseconds()}
	if r.Picked != nil {
		v.Case = r.Picked.ID
	}
	for _, h := range r.Hits {
		v.Candidates = append(v.Candidates, fmt.Sprintf("%s %d/%d-%d", h.Case.ID, h.M, h.R, h.X))
	}
	a.VSM = v
	return a, nil
}

// VSMInfo is what the VSM backend adds to an answer (and so to the audit
// log): the case it picked, the candidates, the evidence codes and the
// knowledge and planner it used.
type VSMInfo struct {
	Case       string   `json:"case,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Abstained  bool     `json:"abstained,omitempty"`
	Guarded    bool     `json:"guarded,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
	Probes     string   `json:"probes,omitempty"`
	Knowledge  string   `json:"knowledge"`
	Note       string   `json:"note,omitempty"`
	Micros     int64    `json:"us"`
}
