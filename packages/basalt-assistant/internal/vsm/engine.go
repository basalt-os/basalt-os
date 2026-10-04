package vsm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/vsm/planner"
)

// Default locations of the data packages.
const (
	DefaultKnowledgeRoot = "/usr/share/basalt/knowledge"   // basalt-knowledge: <fedora>/{cases.jsonl,index.bin,manifest.json}
	DefaultPlannerDir    = "/usr/share/basalt/vsm-planner" // basalt-vsm-planner: weights.bin, config.txt
)

// Engine holds the knowledge index and the planner. Episodes are
// serialized (one recurrent state); one takes well under a millisecond
// of planner time.
type Engine struct {
	Index   *knowledge.Index
	Model   *planner.Model
	Ctx     knowledge.Context
	Ext     bool // map the assistant's newer features (Extensions)
	mu      sync.Mutex
	st      *planner.State
	Elapsed time.Duration // planner time of the last episode
}

// Open loads the knowledge index and the planner and checks that they
// speak the same DSL.
func Open(knowledgeDir, plannerDir string, ctx knowledge.Context) (*Engine, error) {
	ix, err := knowledge.Open(knowledgeDir)
	if err != nil {
		return nil, fmt.Errorf("knowledge %s: %v", knowledgeDir, err)
	}
	m, err := planner.Load(plannerDir)
	if err != nil {
		return nil, fmt.Errorf("planner %s: %v", plannerDir, err)
	}
	if m.DSL != DSLVersion || ix.Manifest.DSL != DSLVersion {
		return nil, fmt.Errorf("DSL mismatch: planner %q, knowledge %q, assistant %q", m.DSL, ix.Manifest.DSL, DSLVersion)
	}
	if len(ix.ExtractErrors) > 0 {
		return nil, fmt.Errorf("knowledge: extractors that do not compile: %s", strings.Join(ix.ExtractErrors, ", "))
	}
	return &Engine{Index: ix, Model: m, Ctx: ctx, Ext: true, st: m.NewState()}, nil
}

// KnowledgeDir is the index for a Fedora release under root.
func KnowledgeDir(root string, fedora int) string {
	if root == "" {
		root = DefaultKnowledgeRoot
	}
	return filepath.Join(root, strconv.Itoa(fedora))
}

// FedoraRelease is the Fedora release this system runs on (0 when
// unknown): os-release's VERSION_ID when it is a release number (Basalt
// OS sets it so), else its PLATFORM_ID (platform:f44), else the %fedora
// macro rpm itself uses (what `rpm -E %fedora` prints).
func FedoraRelease() int {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return string(b)
	}
	osr := read("/etc/os-release")
	if osr == "" {
		osr = read("/usr/lib/os-release")
	}
	return fedoraFrom(osr, read("/usr/lib/rpm/macros.d/macros.dist"))
}

func fedoraFrom(osRelease, macros string) int {
	kv := map[string]string{}
	for _, line := range strings.Split(osRelease, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	if n, err := strconv.Atoi(kv["VERSION_ID"]); err == nil && n > 0 {
		return n
	}
	if v, ok := strings.CutPrefix(kv["PLATFORM_ID"], "platform:f"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	for _, line := range strings.Split(macros, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "%fedora" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// Version names the planner and the knowledge for logs and the audit log.
func (e *Engine) Version() string {
	s := e.Model.SHA256
	if len(s) > 12 {
		s = s[:12]
	}
	return fmt.Sprintf("planner %s, %s", s, e.Index.Version())
}

// Episode runs one episode with a prober.
func (e *Engine) Episode(goal, subject, view string, question bool, pr Prober) Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()
	t0 := time.Now()
	rt := NewRuntime(goal, subject, view, question, e.Index, e.Ctx, pr)
	out := Run(rt, e.st)
	e.Elapsed = time.Since(t0)
	return out
}

// Result is a decision-layer answer.
type Result struct {
	Outcome
	Probabilities map[string]float64
	Note          string // why the planner was not asked, if it was not
}

// ErrQuestion: VSM does not answer this question.
var ErrQuestion = errors.New("vsm: not a VSM question")

// Ask answers a decision-layer question in question mode: the
// diagnosers' features become the evidence (the journal facts go through
// the journal reader, so knowledge extractors apply), the index is looked
// up and the planner picks; its pick probabilities become the answer.
func (e *Engine) Ask(qid, subject string, features map[string]bool, facts map[string]any) (Result, error) {
	goal, ok := GoalOfQuestion[qid]
	if !ok {
		return Result{}, ErrQuestion
	}
	if features["policy_query_failed"] {
		// Deterministic guard outside the model: when the SELinux policy
		// could not be queried the evidence is incomplete, so the answer
		// is the cautious option, whatever the codes say.
		out := Outcome{Goal: goal, Question: qid, Answer: Cautious[qid], Abstained: true, Guarded: true}
		return Result{Outcome: out, Probabilities: out.Distribution(true), Note: "the policy could not be queried"}, nil
	}
	if goal == GoalDiagnose && subject == "" {
		subject = "/"
	}
	pr := QuestionProber(goal, subject, features, facts, e.Ext)
	out := e.Episode(goal, subject, ViewRoot, true, pr)
	return Result{Outcome: out, Probabilities: out.Distribution(true)}, nil
}

// MapProber is a prober over recorded results (tests, question mode).
type MapProber map[byte]ProbeResult

// Has implements Prober.
func (m MapProber) Has(l byte) bool { _, ok := m[l]; return ok }

// Probe implements Prober.
func (m MapProber) Probe(l byte) ProbeResult { return m[l] }

func pick(codes map[string]bool, keep func(string) bool) []string {
	var out []string
	for _, c := range sortedCodes(codes) {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// QuestionProber distributes a question's evidence over the tools that
// produce it, as VSM's decision seam does: unit state, journal (codes
// plus the raw lines), SELinux denials, label check, config checker
// (unavailable when it did not run), ports and disk for a unit; the
// denial, disk or transaction tool for the other goals.
func QuestionProber(goal, subject string, features map[string]bool, facts map[string]any, ext bool) MapProber {
	f := CodesOf(features, ext)
	pr := MapProber{}
	switch goal {
	case GoalDiagnose:
		pr['U'] = ProbeResult{Codes: pick(f, func(c string) bool { return unitCodes[c] }), Bind: map[string]string{"unit": subject}}
		pr['J'] = ProbeResult{Codes: pick(f, func(c string) bool { return journalCodes[c] }), Lines: factLines(facts, "journal")}
		pr['A'] = ProbeResult{Codes: pick(f, func(c string) bool {
			return c == "av" || !(unitCodes[c] || journalCodes[c] || configCodes[c] || labelCodes[c] || c == "po" || c == "fs")
		})}
		pr['L'] = ProbeResult{Codes: pick(f, func(c string) bool { return labelCodes[c] })}
		if cc := pick(f, func(c string) bool { return configCodes[c] }); len(cc) > 0 {
			pr['C'] = ProbeResult{Codes: cc}
		} else {
			pr['C'] = ProbeResult{NA: true}
		}
		pr['P'] = ProbeResult{Codes: pick(f, func(c string) bool { return c == "po" })}
		pr['D'] = ProbeResult{Codes: pick(f, func(c string) bool { return c == "fs" })}
	case GoalDenial:
		pr['A'] = ProbeResult{Codes: sortedCodes(f)}
	case GoalDisk:
		if x := largest(f, facts); x != "" {
			f[x] = true
		}
		pr['D'] = ProbeResult{Codes: sortedCodes(f)}
	case GoalRollback:
		pr['T'] = ProbeResult{Codes: sortedCodes(f)}
	}
	return pr
}

// largest is the disk tool's computed code: which flagged holder
// (snapshots, journal, package cache) is largest (xs, xj, xk).
func largest(f map[string]bool, facts map[string]any) string {
	flagged := []struct{ code, fact, x string }{
		{"sl", "snapshots_bytes", "xs"}, {"jl", "journal_bytes", "xj"}, {"kl", "package_cache_bytes", "xk"}}
	var have []int
	for i, fl := range flagged {
		if f[fl.code] {
			have = append(have, i)
		}
	}
	if len(have) == 0 {
		return ""
	}
	if len(have) == 1 || len(facts) == 0 {
		return flagged[have[0]].x
	}
	best, bv := have[0], factNum(facts, flagged[have[0]].fact)
	for _, i := range have[1:] {
		if v := factNum(facts, flagged[i].fact); v > bv {
			best, bv = i, v
		}
	}
	return flagged[best].x
}

func factNum(facts map[string]any, k string) float64 {
	switch v := facts[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	}
	return 0
}

func factLines(facts map[string]any, k string) []string {
	switch v := facts[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
