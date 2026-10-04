package vsm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/knowledge"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/vsm/planner"
)

// The fixtures are recorded from VSM's reference implementation (its
// episode runtime, knowledge reader, binder and guard, with the reference
// planner runtime): testdata/episodes.jsonl replays scenarios of its
// evaluation sets in both modes, testdata/decide.jsonl questions of the
// shared evaluation suite through its decision seam, both on the test
// index. The planner's recorded decisions are replayed (no weights are
// needed); every text the runtime feeds the planner must match the
// recording character for character.

type op struct {
	Feed   *string            `json:"feed"`
	Decide *string            `json:"decide"`
	P      map[string]float64 `json:"p"`
}

// replay is a Decider that returns recorded decisions and checks feeds.
type replay struct {
	t    *testing.T
	id   string
	ops  []op
	i    int
	errs int
	// verdictFree: the binder's verdict after a pick (" V:ok >") may
	// differ. Decision questions carry no bindings (the reference fills
	// them from a stand-in policy table); the verdict comes after the
	// pick, so it cannot change the answer.
	verdictFree bool
}

func (r *replay) Reset() {}

func (r *replay) Feed(s string) {
	if r.i >= len(r.ops) || r.ops[r.i].Feed == nil {
		r.fail("unexpected feed %q", s)
		return
	}
	if ref := *r.ops[r.i].Feed; ref != s && !(r.verdictFree && strings.HasPrefix(s, " V:") && strings.HasPrefix(ref, " V:")) {
		r.fail("feed %q, reference %q", s, ref)
	}
	r.i++
}

func (r *replay) Decide() (rune, map[rune]float64, bool) {
	if r.i >= len(r.ops) || r.ops[r.i].Feed != nil {
		r.fail("unexpected decision")
		return 0, nil, false
	}
	o := r.ops[r.i]
	r.i++
	if o.Decide == nil {
		return 0, nil, false
	}
	p := map[rune]float64{}
	for k, v := range o.P {
		p[[]rune(k)[0]] = v
	}
	return []rune(*o.Decide)[0], p, true
}

func (r *replay) fail(f string, a ...any) {
	r.errs++
	if r.errs <= 2 {
		r.t.Errorf(r.id+": "+f, a...)
	}
}

type probeJSON struct {
	Codes []string          `json:"codes"`
	Lines []string          `json:"lines"`
	Bind  map[string]string `json:"bind"`
}

type scenario struct {
	ID      string                     `json:"id"`
	Goal    string                     `json:"goal"`
	Subject string                     `json:"subject"`
	View    string                     `json:"view"`
	Probes  map[string]json.RawMessage `json:"probes"`
	Ctx     struct {
		Fedora   int               `json:"fedora"`
		Packages map[string]string `json:"packages"`
	} `json:"ctx"`
}

func (s scenario) prober(t *testing.T) MapProber {
	pr := MapProber{}
	for l, raw := range s.Probes {
		if string(raw) == `"na"` {
			pr[l[0]] = ProbeResult{NA: true}
			continue
		}
		if string(raw) == "null" {
			pr[l[0]] = ProbeResult{}
			continue
		}
		var p probeJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("%s: probe %s: %v", s.ID, l, err)
		}
		bind := map[string]string{}
		for k, v := range p.Bind {
			bind[k] = v
		}
		pr[l[0]] = ProbeResult{Codes: p.Codes, Lines: p.Lines, Bind: bind}
	}
	return pr
}

type hitJSON struct {
	ID      string `json:"id"`
	M, R, X int
}

type outcomeJSON struct {
	Picked    *string           `json:"picked"`
	Abstained bool              `json:"abstained"`
	Guarded   bool              `json:"guarded"`
	Actions   []action.Action   `json:"actions"`
	Held      []action.Action   `json:"held_actions"`
	Precheck  *string           `json:"precheck"`
	Probes    []string          `json:"probes"`
	Evidence  []string          `json:"evidence"`
	Answer    string            `json:"answer"`
	Diagnosis *string           `json:"diagnosis"`
	Hits      []hitJSON         `json:"hits"`
	Bindings  map[string]string `json:"bindings"`
}

func readRows(t *testing.T, path string, each func(b []byte)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		each(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

func norm(as []action.Action) string {
	var s []string
	for _, a := range as {
		b, _ := json.Marshal(a.Params)
		s = append(s, a.Kind+string(b))
	}
	sort.Strings(s)
	return strings.Join(s, ";")
}

// compare checks an outcome against the reference; it returns the number
// of differences.
func compare(t *testing.T, id string, got Outcome, dist map[string]float64, want outcomeJSON, wantDist map[string]float64, strictActions bool) int {
	t.Helper()
	var diffs []string
	pick := ""
	if got.Picked != nil {
		pick = got.Picked.ID
	}
	wpick := ""
	if want.Picked != nil {
		wpick = *want.Picked
	}
	if pick != wpick {
		diffs = append(diffs, "picked "+pick+" vs "+wpick)
	}
	if got.Abstained != want.Abstained || got.Guarded != want.Guarded {
		diffs = append(diffs, "abstained/guarded")
	}
	if got.Answer != want.Answer {
		diffs = append(diffs, "answer "+got.Answer+" vs "+want.Answer)
	}
	if want.Probes != nil && got.Probes != strings.Join(want.Probes, "") {
		diffs = append(diffs, "probes "+got.Probes+" vs "+strings.Join(want.Probes, ""))
	}
	if !reflect.DeepEqual(append([]string{}, got.Evidence...), append([]string{}, want.Evidence...)) {
		diffs = append(diffs, "evidence "+strings.Join(got.Evidence, " ")+" vs "+strings.Join(want.Evidence, " "))
	}
	var gh, wh []string
	for _, h := range got.Hits {
		gh = append(gh, h.Case.ID)
	}
	for _, h := range want.Hits {
		wh = append(wh, h.ID)
	}
	if strings.Join(gh, ",") != strings.Join(wh, ",") {
		diffs = append(diffs, "hits "+strings.Join(gh, ",")+" vs "+strings.Join(wh, ","))
	}
	if strictActions {
		if norm(got.Actions) != norm(want.Actions) || norm(got.Held) != norm(want.Held) {
			diffs = append(diffs, "actions "+norm(got.Actions)+" vs "+norm(want.Actions))
		}
		wp := ""
		if want.Precheck != nil {
			wp = *want.Precheck
		}
		if got.Precheck != wp {
			diffs = append(diffs, "precheck "+got.Precheck+" vs "+wp)
		}
		wd := ""
		if want.Diagnosis != nil {
			wd = *want.Diagnosis
		}
		if got.Diagnosis != wd {
			diffs = append(diffs, "diagnosis "+got.Diagnosis+" vs "+wd)
		}
	}
	// The reference's probabilities passed through a 9-decimal print.
	for k, v := range wantDist {
		if d := math.Abs(dist[k] - v); d > 1e-8 {
			diffs = append(diffs, fmt.Sprintf("distribution %s %.3g", k, d))
			break
		}
	}
	if len(diffs) > 0 {
		t.Errorf("%s: %s", id, strings.Join(diffs, "; "))
	}
	return len(diffs)
}

func testIndex(t *testing.T) *knowledge.Index {
	ix, err := knowledge.Open("../knowledge/testdata/index")
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

type episodeRow struct {
	Set      string             `json:"set"`
	Mode     string             `json:"mode"`
	Scenario json.RawMessage    `json:"scenario"`
	Ops      []op               `json:"ops"`
	Outcome  outcomeJSON        `json:"outcome"`
	Dist     map[string]float64 `json:"distribution"`
	Coverage map[string]float64 `json:"coverage"`
}

func episodes(t *testing.T, ix *knowledge.Index, path string, dec func(row episodeRow) Decider) (n, bad int) {
	readRows(t, path, func(b []byte) {
		var row episodeRow
		if err := json.Unmarshal(b, &row); err != nil {
			t.Fatal(err)
		}
		var sc scenario
		if err := json.Unmarshal(row.Scenario, &sc); err != nil {
			t.Fatal(err)
		}
		id := sc.ID + "/" + row.Mode
		rt := NewRuntime(sc.Goal, sc.Subject, sc.View, row.Mode == "question", ix,
			knowledge.Context{Fedora: sc.Ctx.Fedora, Packages: sc.Ctx.Packages}, sc.prober(t))
		out := Run(rt, dec(row))
		lam := 0.0
		if sc.View == ViewConfined {
			lam = row.Coverage[out.Question]
		}
		if compare(t, id, out, out.CalibratedDistribution(true, lam), row.Outcome, row.Dist, true) > 0 {
			bad++
		}
		n++
	})
	return n, bad
}

func TestEpisodesReplay(t *testing.T) {
	ix := testIndex(t)
	n, bad := episodes(t, ix, "testdata/episodes.jsonl", func(row episodeRow) Decider {
		return &replay{t: t, id: "replay", ops: row.Ops}
	})
	if n < 100 || bad > 0 {
		t.Errorf("%d episodes, %d differ", n, bad)
	}
}

type decideRow struct {
	Case string `json:"case"`
	Q    struct {
		ID       string          `json:"id"`
		Subject  string          `json:"subject"`
		Features map[string]bool `json:"features"`
		Facts    map[string]any  `json:"facts"`
	} `json:"q"`
	Ops      []op               `json:"ops"`
	Evidence []string           `json:"evidence"`
	Picked   *string            `json:"picked"`
	Answer   string             `json:"answer"`
	Abst     bool               `json:"abstained"`
	Guarded  bool               `json:"guarded"`
	Hits     []hitJSON          `json:"hits"`
	Dist     map[string]float64 `json:"distribution"`
	Coverage map[string]float64 `json:"coverage"`
}

func questions(t *testing.T, ix *knowledge.Index, path string, dec func(row decideRow) Decider) (n, bad int) {
	readRows(t, path, func(b []byte) {
		var row decideRow
		if err := json.Unmarshal(b, &row); err != nil {
			t.Fatal(err)
		}
		goal := GoalOfQuestion[row.Q.ID]
		subj := row.Q.Subject
		if subj == "" {
			subj = "/"
		}
		pr := QuestionProber(goal, subj, row.Q.Features, row.Q.Facts, false)
		rt := NewRuntime(goal, subj, ViewRoot, true, ix, knowledge.Context{Fedora: 44}, pr)
		out := Run(rt, dec(row))
		want := outcomeJSON{Picked: row.Picked, Abstained: row.Abst, Guarded: row.Guarded, Answer: row.Answer,
			Evidence: row.Evidence, Hits: row.Hits}
		if row.Hits == nil {
			want.Hits = nil
		}
		lam := 0.0
		if v, _ := row.Q.Facts["view"].(string); v == ViewConfined {
			lam = row.Coverage[row.Q.ID]
		}
		if compare(t, row.Case+"/"+row.Q.ID, out, out.CalibratedDistribution(true, lam), want, row.Dist, false) > 0 {
			bad++
		}
		n++
	})
	return n, bad
}

func TestQuestionsReplay(t *testing.T) {
	ix := testIndex(t)
	n, bad := questions(t, ix, "testdata/decide.jsonl", func(row decideRow) Decider {
		return &replay{t: t, id: row.Case, ops: row.Ops, verdictFree: true}
	})
	if n < 30 || bad > 0 {
		t.Errorf("%d questions, %d differ", n, bad)
	}
}

// TestShippedEpisodes runs the Go planner (no replay) with the shipped
// weights and knowledge over the full reference recordings
// (BASALT_VSM_PLANNER, BASALT_VSM_KNOWLEDGE, BASALT_VSM_GOLDEN).
func TestShippedEpisodes(t *testing.T) {
	pd, kd, gold := os.Getenv("BASALT_VSM_PLANNER"), os.Getenv("BASALT_VSM_KNOWLEDGE"), os.Getenv("BASALT_VSM_GOLDEN")
	if pd == "" || kd == "" || gold == "" {
		t.Skip("BASALT_VSM_PLANNER, BASALT_VSM_KNOWLEDGE and BASALT_VSM_GOLDEN not set")
	}
	m, err := planner.Load(pd)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := knowledge.Open(kd)
	if err != nil {
		t.Fatal(err)
	}
	st := m.NewState()
	n, bad := episodes(t, ix, filepath.Join(gold, "episodes.jsonl"), func(episodeRow) Decider { return st })
	t.Logf("episodes: %d, %d differ from the reference", n, bad)
	qn, qbad := questions(t, ix, filepath.Join(gold, "decide.jsonl"), func(decideRow) Decider { return st })
	t.Logf("decision questions: %d, %d differ from the reference", qn, qbad)
}

func TestGuardAndADR0008(t *testing.T) {
	ix := testIndex(t)
	// A partial match picked by the planner is refused by the guard.
	pr := MapProber{'A': {Codes: []string{"pc"}}}
	rt := NewRuntime(GoalDenial, "", ViewRoot, true, ix, knowledge.Context{Fedora: 44}, pr)
	rt.Step('K')
	if len(rt.Hits) == 0 || rt.Hits[0].Full() {
		t.Fatalf("hits %v", rt.Hits)
	}
	rt.Step('0')
	if !rt.Guarded || !rt.Abstained || rt.Picked != nil {
		t.Error("the guard accepted a partial match")
	}
	// From the confined view a file restore stays a hint without root confirmation.
	acts := []action.Action{{Kind: action.FileRestore, Params: map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "7"}}}
	if RootConfirmationMissing(acts, ViewConfined, map[string]string{}) == "" {
		t.Error("confined restore not held")
	}
	if RootConfirmationMissing(acts, ViewConfined, map[string]string{"root_confirmed": "yes"}) != "" {
		t.Error("confirmed restore held")
	}
	if RootConfirmationMissing(acts, ViewRoot, map[string]string{}) != "" {
		t.Error("root restore held")
	}
	// The binder refuses a missing binding and an invalid parameter.
	c := ix.CaseByID("kb-avc-port")
	if _, err := Bind(c.Actions, map[string]string{"port": "8181", "proto": "tcp"}); err == nil {
		t.Error("bound without need_port_type")
	}
	if _, err := Bind(c.Actions, map[string]string{"port": "99999", "proto": "tcp", "need_port_type": "http_port_t"}); err == nil {
		t.Error("bound an out-of-range port")
	}
	as, err := Bind(c.Actions, map[string]string{"port": "8181", "proto": "tcp", "need_port_type": "http_port_t"})
	if err != nil || len(as) != 1 || as[0].Params["port"] != "8181" || as[0].Params["mode"] != "add" {
		t.Errorf("bind: %v %v", as, err)
	}
}

func TestJournalReader(t *testing.T) {
	ix := testIndex(t)
	codes, b := JournalFeatures([]string{
		`nginx: [emerg] unknown directive "bogus" in /etc/nginx/nginx.conf:47`,
		`nginx: [emerg] bind() to 0.0.0.0:8181 failed (13: Permission denied)`,
		`x509: certificate has expired or is not yet valid`,
		`Dependency failed for other.service`,
		`Main process killed by the OOM killer`,
	}, ix.Extractors, "nginx.service")
	for _, c := range []string{"ce", "cl", "pd", "cx", "om"} {
		if !codes[c] {
			t.Errorf("code %s missing: %v", c, codes)
		}
	}
	if codes["dp"] {
		t.Error("a dependency failure of another unit counted")
	}
	if b["config_file"] != "/etc/nginx/nginx.conf" || b["config_line"] != "47" || b["port"] != "8181" {
		t.Errorf("bindings %v", b)
	}
}

func TestFedoraRelease(t *testing.T) {
	for _, c := range []struct {
		osr, macros string
		want        int
	}{
		{"NAME=\"Basalt OS\"\nVERSION_ID=44\n", "", 44},
		{"VERSION_ID=\"45\"\n", "", 45},
		{"VERSION_ID=0.0.1\nPLATFORM_ID=\"platform:f44\"\n", "", 44},
		{"VERSION_ID=0.0.1\n", "%fedora              44\n%distcore .fc%{fedora}\n", 44},
		{"VERSION_ID=0.0.1\n", "", 0},
	} {
		if got := fedoraFrom(c.osr, c.macros); got != c.want {
			t.Errorf("%q %q: %d, want %d", c.osr, c.macros, got, c.want)
		}
	}
}
