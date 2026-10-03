// Package decide is the decision layer: typed questions (choice, score,
// boolean) about a state, answered with a probability for every option.
// The application, not the backend, decides what to do with the answer:
// above the question's threshold the automatic path is taken (diagnose and
// propose; never apply), below it the case is left for a person to review.
// Every decision is logged with its question, options, probabilities and
// the action taken.
//
// The first backend is deterministic rules with fixed, roughly calibrated
// probabilities. A model backend (an OpenAI-compatible local endpoint that
// restricts the output to the valid options and reads each option's
// probability from token log-probabilities) plugs in behind the same
// Backend interface; see model.go.
package decide

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Kinds of question.
const (
	Choice  = "choice"  // one of a fixed set of options
	Score   = "score"   // a value on a rubric, options "1".."N"
	Boolean = "boolean" // options "true" and "false"
)

// Question is one bounded question about a state.
type Question struct {
	ID       string          `json:"id"`   // stable name, e.g. "avc.class"
	Kind     string          `json:"kind"` // choice, score, boolean
	Prompt   string          `json:"prompt"`
	Options  []string        `json:"options"`
	Features map[string]bool `json:"features"`          // what the diagnosers found
	Facts    map[string]any  `json:"facts,omitempty"`   // numbers and labels for backends and logs
	Subject  string          `json:"subject,omitempty"` // what it is about (unit, path, mount)
}

// Answer carries a probability for every option.
type Answer struct {
	Probabilities map[string]float64 `json:"probabilities"`
	Top           string             `json:"top"`
	Confidence    float64            `json:"confidence"`
	Backend       string             `json:"backend"`
	Rules         []string           `json:"rules,omitempty"` // which rules fired (rules backend)
}

// Backend answers questions.
type Backend interface {
	Name() string
	Answer(ctx context.Context, q Question) (Answer, error)
}

// Decision is what gets logged.
type Decision struct {
	Question  Question `json:"question"`
	Answer    Answer   `json:"answer"`
	Threshold float64  `json:"threshold"`
	Confident bool     `json:"confident"`
	Action    string   `json:"action"` // set by the caller: propose, review, notify, suppress
}

// Logger records decisions (the audit chain).
type Logger interface {
	LogDecision(d Decision) error
}

// Layer asks questions through a backend, applies thresholds and logs.
type Layer struct {
	Backend    Backend
	Fallback   Backend // used when Backend fails (rules)
	Thresholds map[string]float64
	Default    float64
	Log        Logger
}

// NewRules is the default layer: the rules backend, default threshold 0.75.
func NewRules(log Logger, thresholds map[string]float64) *Layer {
	return &Layer{Backend: Rules{}, Thresholds: thresholds, Default: 0.75, Log: log}
}

// Threshold for a question.
func (l *Layer) Threshold(id string) float64 {
	if t, ok := l.Thresholds[id]; ok {
		return t
	}
	if l.Default > 0 {
		return l.Default
	}
	return 0.75
}

// Ask answers a question. The decision is not logged yet: the caller sets
// Action once it knows what it did, then calls Record.
func (l *Layer) Ask(ctx context.Context, q Question) Decision {
	a, err := l.Backend.Answer(ctx, q)
	if err != nil && l.Fallback != nil {
		a, err = l.Fallback.Answer(ctx, q)
		a.Backend += " (fallback)"
	}
	if err != nil {
		a = uniform(q)
		a.Backend = "none: " + err.Error()
	}
	t := l.Threshold(q.ID)
	return Decision{Question: q, Answer: a, Threshold: t, Confident: a.Confidence >= t}
}

// Record logs a decision with the action taken.
func (l *Layer) Record(d Decision, act string) Decision {
	d.Action = act
	if l.Log != nil {
		_ = l.Log.LogDecision(d)
	}
	return d
}

func uniform(q Question) Answer {
	p := map[string]float64{}
	for _, o := range q.Options {
		p[o] = 1 / float64(len(q.Options))
	}
	return finish(p, "")
}

// finish normalizes probabilities and picks the top option (ties broken by
// option order, then name).
func finish(p map[string]float64, backend string) Answer {
	sum := 0.0
	for _, v := range p {
		sum += v
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a := Answer{Probabilities: map[string]float64{}, Backend: backend}
	for _, k := range keys {
		v := p[k] / sum
		v = math.Round(v*1000) / 1000
		a.Probabilities[k] = v
		if v > a.Confidence {
			a.Top, a.Confidence = k, v
		}
	}
	return a
}

// Expected value of a score answer (options are numbers).
func (a Answer) Expected() float64 {
	e := 0.0
	for k, v := range a.Probabilities {
		n, err := strconv.ParseFloat(k, 64)
		if err == nil {
			e += n * v
		}
	}
	return e
}

// String is a compact rendering for reports: "port 0.86, unknown 0.07, ...".
func (a Answer) String() string {
	type kv struct {
		k string
		v float64
	}
	var xs []kv
	for k, v := range a.Probabilities {
		xs = append(xs, kv{k, v})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].v != xs[j].v {
			return xs[i].v > xs[j].v
		}
		return xs[i].k < xs[j].k
	})
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s %.2f", x.k, x.v)
	}
	return s
}
