package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
)

// ModelBackend answers questions with a language model behind an
// OpenAI-compatible endpoint (llama.cpp's llama-server through the
// basalt-llm service, Ollama, vLLM). It is opt-in (decision.backend =
// openai-compatible); the rules stay the default and the fallback.
//
// How a question is asked (ADR 0004, "Decision layer"):
//
//   - the options are numbered 1..N in the prompt and the output is
//     constrained (JSON schema with an integer enum) to one of those
//     numbers, a single token each;
//   - the probability of every option is read from the log-probabilities
//     of that first token (top_logprobs) and renormalized over the valid
//     numbers; an option missing from the returned alternatives gets a
//     floor below the smallest one seen;
//   - an optional per-question temperature (Calibration) rescales the
//     log-probabilities before normalizing; it is fitted on the shared
//     evaluation suite (eval/) so that the thresholds keep their meaning.
//
// The state given to the model: the question's prompt, the features the
// diagnosers found (true ones) and the facts (evidence lines, labels,
// numbers). Safety does not depend on the backend: a model only answers
// bounded questions; proposals are still typed actions and every change
// still needs a person's confirmation.
type ModelBackend struct {
	C           *llm.Client
	Calibration map[string]float64 // per question id: temperature (1 = raw)
	TopN        int                // top_logprobs requested (default 20)
}

// NewModelBackend builds a backend for an endpoint.
func NewModelBackend(endpoint, model string, allowRemote bool) *ModelBackend {
	return &ModelBackend{C: &llm.Client{Endpoint: endpoint, Model: model, AllowRemote: allowRemote, Timeout: 30 * time.Second}}
}

// Name implements Backend.
func (m *ModelBackend) Name() string {
	n := m.C.Model
	if n == "" {
		n = "local"
	}
	return "model:" + n
}

// optionHelp describes options where the bare name is not enough.
var optionHelp = map[string]string{
	"unit.cause/config_error":      "the service's configuration is invalid",
	"unit.cause/selinux_denial":    "SELinux denied an access the service needs",
	"unit.cause/port_conflict":     "the port is already used by another process",
	"unit.cause/dependency_failed": "a unit it depends on failed",
	"unit.cause/disk_full":         "no space left on a file system",
	"unit.cause/missing_file":      "a file or program it needs does not exist",
	"unit.cause/crashed":           "killed by a signal or dumped core",
	"unit.cause/unknown":           "none of the above or not enough evidence",
	"avc.class/mislabeled":         "the object's label differs from the policy default, and the default would be allowed",
	"avc.class/missing_fcontext":   "the object has a generic label in a custom location; a file context rule for a service type is needed",
	"avc.class/port":               "the service binds or connects to a port without the right port type",
	"avc.class/boolean":            "a policy boolean that is off would allow it",
	"avc.class/unknown":            "no known fix",
	"avc.class/suspicious":         "the access targets security-sensitive objects (shadow, keys, policy) and should not be allowed",
	"dnf.next/rollback":            "roll the root back to the snapshot taken before the transaction",
	"dnf.next/investigate":         "nothing to roll back or not safe to; investigate first",
	"disk.cause/snapshots":         "snapshots hold most reclaimable space",
	"disk.cause/journal":           "the systemd journal is large",
	"disk.cause/package_cache":     "the package cache is large",
	"disk.cause/other_data":        "other data (user or service files), nothing safe to reclaim",
}

// Prompt renders the question for the model. Exported for the evaluation
// tool, so that it measures exactly what the daemon sends.
func Prompt(q Question) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Question (%s): %s\n", q.ID, q.Prompt)
	if q.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", q.Subject)
	}
	var on []string
	for k, v := range q.Features {
		if v {
			on = append(on, k)
		}
	}
	sort.Strings(on)
	if len(on) == 0 {
		b.WriteString("Findings: none\n")
	} else {
		fmt.Fprintf(&b, "Findings: %s\n", strings.Join(on, ", "))
	}
	if len(q.Facts) > 0 {
		keys := make([]string, 0, len(q.Facts))
		for k := range q.Facts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("Facts:\n")
		for _, k := range keys {
			v, _ := json.Marshal(q.Facts[k])
			s := string(v)
			if len(s) > 1500 {
				s = s[:1500] + "..."
			}
			fmt.Fprintf(&b, "  %s: %s\n", k, s)
		}
	}
	b.WriteString("Options:\n")
	for i, o := range q.Options {
		h := optionHelp[q.ID+"/"+o]
		switch {
		case q.Kind == Score:
			fmt.Fprintf(&b, "  %d: severity %s\n", i+1, o)
		case h != "":
			fmt.Fprintf(&b, "  %d: %s (%s)\n", i+1, o, h)
		default:
			fmt.Fprintf(&b, "  %d: %s\n", i+1, o)
		}
	}
	b.WriteString("Answer with the number of the best option only.")
	return b.String()
}

const modelSystem = `You are the decision layer of the Basalt OS system assistant. You answer one bounded question about a Linux system event with the number of one option. Base the answer on the findings and facts given; when they do not support any specific option, choose the "unknown" or most cautious option.`

// ModelQuestions are the questions the model answers: those the shared
// evaluation suite measures (accuracy and calibration). Every other
// question (severity, notification, routing) stays with the rules until
// the suite covers it.
var ModelQuestions = map[string]bool{"unit.cause": true, "avc.class": true, "dnf.next": true, "disk.cause": true}

// Answer implements Backend.
func (m *ModelBackend) Answer(ctx context.Context, q Question) (Answer, error) {
	if !ModelQuestions[q.ID] {
		return Rules{}.Answer(ctx, q)
	}
	if len(q.Options) == 0 || len(q.Options) > 9 {
		return Answer{}, fmt.Errorf("model backend: %d options (1 to 9 supported)", len(q.Options))
	}
	enum := make([]int, len(q.Options))
	for i := range enum {
		enum[i] = i + 1
	}
	top := m.TopN
	if top == 0 {
		top = 20
	}
	resp, err := m.C.Complete(ctx, llm.Request{
		Messages:  []llm.Message{{Role: "system", Content: modelSystem}, {Role: "user", Content: Prompt(q)}},
		MaxTokens: 1, Schema: map[string]any{"type": "integer", "enum": enum},
		Logprobs: true, TopLogprobs: top,
	})
	if err != nil {
		return Answer{}, err
	}
	if len(resp.Logprobs) == 0 {
		return Answer{}, errors.New("model backend: the endpoint returned no log-probabilities")
	}
	lp := map[int]float64{}
	minSeen := 0.0
	consider := func(tok string, l float64) {
		n, err := strconv.Atoi(strings.TrimSpace(tok))
		if err != nil || n < 1 || n > len(q.Options) {
			return
		}
		if old, ok := lp[n]; !ok || l > old {
			lp[n] = l
		}
	}
	first := resp.Logprobs[0]
	consider(first.Token, first.Logprob)
	for _, t := range first.TopLogprobs {
		consider(t.Token, t.Logprob)
		if t.Logprob < minSeen {
			minSeen = t.Logprob
		}
	}
	if len(lp) == 0 {
		return Answer{}, fmt.Errorf("model backend: no option among the returned tokens (%q)", first.Token)
	}
	temp := 1.0
	if t, ok := m.Calibration[q.ID]; ok && t > 0 {
		temp = t
	}
	floor := minSeen - 2 // below everything the server returned
	p := map[string]float64{}
	maxL := math.Inf(-1)
	for i := range q.Options {
		l, ok := lp[i+1]
		if !ok {
			l = floor
		}
		lp[i+1] = l / temp
		if lp[i+1] > maxL {
			maxL = lp[i+1]
		}
	}
	for i, o := range q.Options {
		p[o] = math.Exp(lp[i+1] - maxL)
	}
	return finish(p, m.Name()), nil
}

// FromConfig builds the layer for a configured backend name. The rules are
// the default; a model backend always has the rules as its fallback.
func FromConfig(backend, endpoint, model string, log Logger, thresholds map[string]float64, def float64) *Layer {
	return FromConfigFull(Config{Backend: backend, Endpoint: endpoint, Model: model}, log, thresholds, def)
}

// Config selects and parameterizes a backend.
type Config struct {
	Backend     string
	Endpoint    string
	Model       string
	AllowRemote bool
	Calibration map[string]float64
}

// FromConfigFull is FromConfig with every model option.
func FromConfigFull(c Config, log Logger, thresholds map[string]float64, def float64) *Layer {
	l := NewRules(log, thresholds)
	if def > 0 {
		l.Default = def
	}
	if c.Backend == "openai-compatible" {
		mb := NewModelBackend(c.Endpoint, c.Model, c.AllowRemote)
		mb.Calibration = c.Calibration
		l.Backend = mb
		l.Fallback = Rules{}
	}
	return l
}
