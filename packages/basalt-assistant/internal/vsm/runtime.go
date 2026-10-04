package vsm

import (
	"fmt"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/vsm/planner"
)

// Views: what the tools could read.
const (
	ViewRoot     = "root"     // basalt as root: config checkers, port owners, snapshot contents
	ViewConfined = "confined" // the daemon and the MCP server (basalt_assistant_t)
)

// ProbeResult is what one tool returns: codes, raw journal lines (read by
// the journal reader with the knowledge extractors) and bindings. NA means
// the tool is not available in this view.
type ProbeResult struct {
	NA    bool
	Codes []string
	Lines []string
	Bind  map[string]string
	// BindOrder fixes the order bindings are applied in (later wins);
	// empty means sorted keys.
	BindOrder []string
}

// Prober runs one tool by its letter (U J A L C P D T). Has reports the
// tools this episode can call at all (question mode probes all of them
// up front).
type Prober interface {
	Has(letter byte) bool
	Probe(letter byte) ProbeResult
}

// Runtime is one episode: the goal, the evidence gathered so far, the
// candidates and the outcome. It mirrors VSM's reference runtime.
type Runtime struct {
	Goal     string
	View     string
	Question bool // question mode: all evidence injected up front
	Guard    bool // full-match guard (always on when serving)

	ix   *knowledge.Index
	ctx  knowledge.Context
	pr   Prober
	done bool
	nAct int
	// phase: probe, pick, end
	phase string

	Evidence  map[string]bool
	Bindings  map[string]string
	outputs   map[byte]string
	RunOrder  []byte
	Hits      []knowledge.Hit
	Picked    *knowledge.Case
	Abstained bool
	Guarded   bool
	Actions   []action.Action
	Held      []action.Action // ADR 0008: held back as a hint
	Precheck  string          // ok, fail, hint, bad
	PreErr    string
}

// NewRuntime prepares an episode. subject is the unit for diagnose goals.
func NewRuntime(goal, subject, view string, question bool, ix *knowledge.Index, ctx knowledge.Context, pr Prober) *Runtime {
	rt := &Runtime{Goal: goal, View: view, Question: question, Guard: true, ix: ix, ctx: ctx, pr: pr,
		phase: "probe", Evidence: map[string]bool{}, Bindings: map[string]string{}, outputs: map[byte]string{}}
	if goal == GoalDiagnose {
		rt.Bindings["unit"] = subject
	} else {
		rt.Bindings["unit"] = ""
	}
	if question {
		for i := 0; i < len(toolLetters); i++ {
			if pr.Has(toolLetters[i]) {
				rt.probe(toolLetters[i])
			}
		}
	}
	return rt
}

func (rt *Runtime) probe(letter byte) string {
	if out, ok := rt.outputs[letter]; ok {
		return out
	}
	var out string
	if !rt.pr.Has(letter) {
		// A tool the episode has no data for answers with no evidence.
		rt.outputs[letter] = "-"
		rt.RunOrder = append(rt.RunOrder, letter)
		return "-"
	}
	r := rt.pr.Probe(letter)
	if r.NA {
		out = "na"
	} else {
		codes := map[string]bool{}
		for _, c := range r.Codes {
			codes[c] = true
		}
		if len(r.Lines) > 0 {
			unit := ""
			if rt.Goal == GoalDiagnose {
				unit = rt.Bindings["unit"]
			}
			jc, jb := JournalFeatures(r.Lines, rt.ix.Extractors, unit)
			for c := range jc {
				codes[c] = true
			}
			for k, v := range jb {
				if _, ok := rt.Bindings[k]; !ok {
					rt.Bindings[k] = v
				}
			}
		}
		keys := r.BindOrder
		if len(keys) == 0 {
			keys = sortedKeys(r.Bind)
		}
		for _, k := range keys {
			if v := r.Bind[k]; v != "" {
				rt.Bindings[k] = v
			}
		}
		for c := range codes {
			rt.Evidence[c] = true
		}
		out = fmtCodes(codes)
	}
	rt.outputs[letter] = out
	rt.RunOrder = append(rt.RunOrder, letter)
	return out
}

// Prompt is the planner's prompt for the goal.
func (rt *Runtime) Prompt() string { return prompt(rt.Goal) }

// FirstInject is the runtime's first turn.
func (rt *Runtime) FirstInject() string {
	if rt.Question {
		return " E:" + fmtCodes(rt.Evidence) + " >"
	}
	return " >"
}

// Done reports the end of the episode.
func (rt *Runtime) Done() bool { return rt.done }

// Phase is probe, pick or end.
func (rt *Runtime) Phase() string { return rt.phase }

// Step applies one planner action and returns the runtime's reply.
func (rt *Runtime) Step(a rune) string {
	rt.nAct++
	if a == endChar || rt.nAct > maxActions {
		rt.done = true
		return ""
	}
	switch rt.phase {
	case "probe":
		switch {
		case a < 128 && strings.IndexByte(toolLetters, byte(a)) >= 0:
			return fmt.Sprintf(" %c:%s >", a, rt.probe(byte(a)))
		case a == lookupChar:
			rt.phase = "pick"
			rt.Hits = rt.ix.Lookup(rt.Goal, rt.Evidence, rt.ctx)
			return " K:" + knowledge.Format(rt.Hits) + " >"
		case a == abstainChar:
			rt.Abstained, rt.phase = true, "end"
			return " >"
		}
		rt.done = true // a pick before any lookup: invalid, stop
		return ""
	case "pick":
		rt.phase = "end"
		if a == abstainChar {
			rt.Abstained = true
			return " >"
		}
		if a < 128 && strings.IndexByte(pickChars, byte(a)) >= 0 {
			i := int(a - '0')
			if i >= len(rt.Hits) {
				rt.Precheck = "bad"
				return " V:bad >"
			}
			h := rt.Hits[i]
			if rt.Guard && !h.Full() {
				// The deterministic guard: a candidate the evidence does
				// not fully support is never accepted.
				rt.Guarded, rt.Abstained, rt.done = true, true, true
				return ""
			}
			rt.Picked = h.Case
			if h.Case.Actionable() {
				acts, err := Bind(h.Case.Actions, rt.Bindings)
				if err != nil {
					rt.Actions, rt.Precheck, rt.PreErr = nil, "fail", err.Error()
				} else {
					rt.Actions, rt.Precheck = acts, "ok"
				}
				if why := RootConfirmationMissing(rt.Actions, rt.View, rt.Bindings); why != "" {
					rt.Held, rt.Actions, rt.Precheck, rt.PreErr = rt.Actions, nil, "hint", why
				}
			} else {
				rt.Precheck = "hint"
			}
			return " V:" + rt.Precheck + " >"
		}
		rt.done = true
		return ""
	}
	rt.done = true
	return ""
}

// Trace is one planner decision.
type Trace struct {
	Phase  string
	Action rune
	P      map[rune]float64
}

// Outcome of an episode.
type Outcome struct {
	Goal      string
	Question  string
	Answer    string
	Picked    *knowledge.Case
	Abstained bool
	Guarded   bool
	Evidence  []string
	Bindings  map[string]string
	Hits      []knowledge.Hit
	Actions   []action.Action
	Held      []action.Action
	Precheck  string
	PreErr    string
	Probes    string
	Diagnosis string
	Trace     []Trace
}

// Decider is the planner's interface (planner.State implements it).
type Decider interface {
	Reset()
	Feed(text string)
	Decide() (rune, map[rune]float64, bool)
}

var _ Decider = (*planner.State)(nil)

// Run rolls the planner out over the runtime.
func Run(rt *Runtime, st Decider) Outcome {
	st.Reset()
	st.Feed(rt.Prompt())
	st.Feed(rt.FirstInject())
	var trace []Trace
	for !rt.Done() {
		a, p, ok := st.Decide()
		trace = append(trace, Trace{Phase: rt.Phase(), Action: a, P: p})
		if !ok {
			break
		}
		if inj := rt.Step(a); inj != "" {
			st.Feed(inj)
		}
	}
	q := QuestionOfGoal[rt.Goal]
	out := Outcome{Goal: rt.Goal, Question: q, Picked: rt.Picked, Abstained: rt.Abstained, Guarded: rt.Guarded,
		Evidence: sortedCodes(rt.Evidence), Bindings: rt.Bindings, Hits: rt.Hits, Actions: rt.Actions, Held: rt.Held,
		Precheck: rt.Precheck, PreErr: rt.PreErr, Probes: string(rt.RunOrder), Trace: trace}
	out.Answer = Cautious[q]
	if rt.Picked != nil {
		out.Answer = rt.Picked.Answers[q]
		out.Diagnosis = knowledge.Render(rt.Picked, rt.Bindings)
	}
	return out
}

// Distribution turns the planner's pick probabilities into probabilities
// over the question's options: candidate i -> its case's answer, a
// candidate the guard would refuse -> the cautious option, abstain -> the
// cautious option; every option gets a floor of 0.001 before normalizing.
func (o Outcome) Distribution(guard bool) map[string]float64 {
	const floor = 1e-3
	opts := Options[o.Question]
	p := make(map[string]float64, len(opts))
	for _, x := range opts {
		p[x] = floor
	}
	cautious := Cautious[o.Question]
	var pick *Trace
	for i := range o.Trace {
		if o.Trace[i].Phase == "pick" && o.Trace[i].P != nil {
			pick = &o.Trace[i]
			break
		}
	}
	if pick == nil {
		p[o.Answer] += 1.0
	} else {
		pp := pick.P
		z := 0.0
		for _, c := range pickChars + string(abstainChar) {
			z += pp[c]
		}
		if z == 0 {
			z = 1
		}
		for i, h := range o.Hits {
			mass := pp[rune('0'+i)] / z
			ans := h.Case.Answers[o.Question]
			if guard && !h.Full() {
				p[cautious] += mass
			} else if _, ok := p[ans]; ok {
				p[ans] += mass
			}
		}
		p[cautious] += pp[abstainChar] / z
	}
	s := 0.0
	for _, x := range opts {
		s += p[x]
	}
	out := make(map[string]float64, len(p))
	for _, x := range opts {
		out[x] = p[x] / s
	}
	return out
}
