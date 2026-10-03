package decide

import (
	"context"
	"errors"
	"fmt"
)

// ModelBackend is the seam for a language-model backend. It is NOT
// implemented in this milestone; selecting it makes the layer fall back to
// the rules backend and the fallback shows in every logged decision.
//
// The intended implementation (ADR 0004, "Decision layer"):
//
//   - endpoint: an OpenAI-compatible server on the machine (llama.cpp
//     server, Ollama, vLLM), e.g. http://127.0.0.1:8080/v1; remote
//     endpoints only by explicit opt-in, with redaction of what is sent;
//   - request: the question's Prompt plus a JSON rendering of Features and
//     Facts as the state, with the output constrained to exactly one of
//     Options (a grammar or JSON schema with an enum);
//   - probabilities: from the log-probabilities of the first token of each
//     option (top_logprobs), renormalized over the valid options;
//   - calibration: per-question temperature or Platt scaling fitted on the
//     shared evaluation suite, then the same thresholds apply.
//
// Safety does not depend on the backend: a model only answers bounded
// questions; proposals are still typed actions and every change still needs
// a person's confirmation.
type ModelBackend struct {
	Endpoint string // OpenAI-compatible base URL
	Model    string
}

// ErrNotImplemented is returned by the model backend in this milestone.
var ErrNotImplemented = errors.New("model backend not implemented in this release")

// Name implements Backend.
func (m ModelBackend) Name() string { return "openai-compatible:" + m.Model }

// Answer implements Backend.
func (m ModelBackend) Answer(_ context.Context, q Question) (Answer, error) {
	return Answer{}, fmt.Errorf("%w (question %s, endpoint %s)", ErrNotImplemented, q.ID, m.Endpoint)
}

// FromConfig builds the layer for a configured backend name.
func FromConfig(backend, endpoint, model string, log Logger, thresholds map[string]float64, def float64) *Layer {
	l := NewRules(log, thresholds)
	if def > 0 {
		l.Default = def
	}
	if backend == "openai-compatible" {
		l.Backend = ModelBackend{Endpoint: endpoint, Model: model}
		l.Fallback = Rules{}
	}
	return l
}
