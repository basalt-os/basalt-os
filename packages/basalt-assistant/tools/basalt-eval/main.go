// basalt-eval runs the shared evaluation suite (eval/ at the top of the
// repository) against the decision-layer backends and the translator.
// It is a development tool: it is not packaged and never touches the
// system, it only reads the case files and talks to a model endpoint.
//
//	basalt-eval translate -endpoint E [-set eval/translator.jsonl] [-out FILE]
//	basalt-eval decide [-endpoint E] [-cases DIR] [-out FILE] [-folds 5]
//	basalt-eval check [-cases DIR]
//	basalt-eval render-data -split train|heldout [-n N] [-seed S] [-out FILE]
//	basalt-eval humanize -endpoint E [-set FILE] [-out FILE] [-stream]
//
// Output: a JSON summary on stdout; per-item results (JSON lines) in -out.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/translate"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: basalt-eval translate|decide|check|render-data|humanize [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "translate":
		err = runTranslate(os.Args[2:])
	case "decide":
		err = runDecide(os.Args[2:])
	case "check":
		err = runCheck(os.Args[2:])
	case "derive":
		err = runDerive()
	case "render-data":
		err = runRenderData(os.Args[2:])
	case "humanize":
		err = runHumanize(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "basalt-eval:", err)
		os.Exit(1)
	}
}

func readJSONL(path string, each func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		b := []byte(strings.TrimSpace(sc.Text()))
		if len(b) == 0 || b[0] == '#' {
			continue
		}
		if err := each(b); err != nil {
			return fmt.Errorf("%s:%d: %v", path, n, err)
		}
	}
	return sc.Err()
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

func round(v float64, d int) float64 {
	m := math.Pow(10, float64(d))
	return math.Round(v*m) / m
}

// ---------------------------------------------------------------- translator

type tcase struct {
	ID     string             `json:"id"`
	Text   string             `json:"text"`
	Lang   string             `json:"lang"`
	Tags   []string           `json:"tags"`
	Want   translate.Intent   `json:"want"`
	Accept []translate.Intent `json:"accept"`
}

// norm makes argument comparison independent of spelling the user may
// choose (a unit with or without ".service").
func norm(in translate.Intent) translate.Intent {
	if in.Intent == translate.Why && in.Unit != "" && !strings.Contains(in.Unit, ".") {
		in.Unit += ".service"
	}
	if in.Intent == translate.FixSELinux && strings.HasSuffix(in.Since, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(in.Since, "d"))
		if err == nil {
			in.Since = strconv.Itoa(n*24) + "h"
		}
	}
	if in.Intent == translate.Snapshots {
		if in.Mode == "" {
			in.Mode = "list"
		}
		if in.Mode == "list" {
			in.A, in.B = 0, 0
		}
		if in.Mode == "rollback" {
			in.B = 0
		}
	}
	return in
}

func sameArgs(a, b translate.Intent) bool {
	a, b = norm(a), norm(b)
	if a.Intent == translate.Why {
		return strings.EqualFold(a.Unit, b.Unit)
	}
	return a == b
}

func runTranslate(args []string) error {
	fs := flag.NewFlagSet("translate", flag.ExitOnError)
	ep := fs.String("endpoint", "http://127.0.0.1:8080/v1", "OpenAI-compatible endpoint")
	model := fs.String("model", "", "model name sent to the endpoint")
	set := fs.String("set", "eval/translator.jsonl", "test set")
	out := fs.String("out", "", "per-item results (JSON lines)")
	label := fs.String("label", "", "label for the summary")
	compact := fs.Bool("compact", false, "use the compact prompt (fine-tuned model); same as -prompt compact")
	prompt := fs.String("prompt", "examples", "prompt: examples, compact, or auto (by the served model's name)")
	_ = fs.Parse(args)

	var cases []tcase
	if err := readJSONL(*set, func(b []byte) error {
		var c tcase
		if err := json.Unmarshal(b, &c); err != nil {
			return err
		}
		cases = append(cases, c)
		return nil
	}); err != nil {
		return err
	}
	tr := &translate.Translator{C: &llm.Client{Endpoint: *ep, Model: *model, Timeout: 120 * time.Second}, Prompt: *prompt}
	if *compact {
		tr.Prompt = translate.PromptCompact
	}
	var w *bufio.Writer
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = bufio.NewWriter(f)
		defer w.Flush()
	}
	mScore, gScore := newScore(), newScore()
	var lat, tps, ptoks []float64
	invalid := 0
	for _, c := range cases {
		res, err := tr.Translate(context.Background(), c.Text)
		got, raw := res.Intent, res.Model
		if err != nil {
			invalid++
			got = translate.Intent{Intent: translate.Clarify}
			raw = got
		}
		lat = append(lat, float64(res.Elapsed.Milliseconds()))
		if res.Usage.PredictedMS > 0 && res.Usage.CompletionTokens > 0 {
			tps = append(tps, float64(res.Usage.CompletionTokens)/(res.Usage.PredictedMS/1000))
		}
		ptoks = append(ptoks, float64(res.Usage.PromptTokens))
		mS, mL := mScore.add(c, raw)
		gS, gL := gScore.add(c, got)
		if w != nil {
			rec := map[string]any{"id": c.ID, "text": c.Text, "want": c.Want, "model": raw, "got": got, "raw": res.Raw,
				"grounding": res.Grounding, "model_strict": mS, "model_lenient": mL, "strict": gS, "lenient": gL,
				"ms": res.Elapsed.Milliseconds(), "err": errString(err)}
			b, _ := json.Marshal(rec)
			w.Write(append(b, '\n'))
		}
	}
	sum := map[string]any{
		"label": *label, "n": len(cases), "invalid_outputs": invalid,
		"model": mScore.summary(), "grounded": gScore.summary(),
		"latency_ms_p50": pct(lat, 50), "latency_ms_p95": pct(lat, 95),
		"gen_tokens_per_s_p50": round(pct(tps, 50), 1),
		"prompt_tokens_p50":    pct(ptoks, 50),
	}
	return json.NewEncoder(os.Stdout).Encode(sum)
}

type bucket struct{ n, strict, lenient int }

// score accumulates translator metrics for one way of reading the output.
type score struct {
	n, strict, lenient, intentOK, argN, argOK, unsafe int
	by                                                map[string]*bucket
}

func newScore() *score { return &score{by: map[string]*bucket{}} }

func (s *score) add(c tcase, got translate.Intent) (bool, bool) {
	s.n++
	iOK := got.Intent == c.Want.Intent
	sOK := iOK && sameArgs(got, c.Want)
	lOK := sOK
	for _, a := range c.Accept {
		if got.Intent == a.Intent && sameArgs(got, a) {
			lOK = true
		}
	}
	if iOK {
		s.intentOK++
		if hasArgs(c.Want) {
			s.argN++
			if sOK {
				s.argOK++
			}
		}
	}
	if sOK {
		s.strict++
	}
	if lOK {
		s.lenient++
	}
	// A change the person did not ask for (or with other arguments) is the
	// costly error, even though it still needs a confirmation.
	if got.Changes() && !(c.Want.Changes() && sOK) {
		s.unsafe++
	}
	keys := []string{"lang:" + c.Lang, "want:" + c.Want.Intent}
	for _, t := range c.Tags {
		keys = append(keys, "tag:"+t)
	}
	for _, k := range keys {
		b := s.by[k]
		if b == nil {
			b = &bucket{}
			s.by[k] = b
		}
		b.n++
		if sOK {
			b.strict++
		}
		if lOK {
			b.lenient++
		}
	}
	return sOK, lOK
}

func (s *score) summary() map[string]any {
	n := float64(s.n)
	by := map[string]any{}
	for k, b := range s.by {
		by[k] = map[string]any{"n": b.n, "strict": round(float64(b.strict)/float64(b.n), 3), "lenient": round(float64(b.lenient)/float64(b.n), 3)}
	}
	return map[string]any{
		"intent_accuracy":     round(float64(s.intentOK)/n, 3),
		"exact_strict":        round(float64(s.strict)/n, 3),
		"exact_lenient":       round(float64(s.lenient)/n, 3),
		"argument_accuracy":   round(float64(s.argOK)/math.Max(1, float64(s.argN)), 3),
		"argument_cases":      s.argN,
		"unrequested_changes": s.unsafe,
		"by":                  by,
	}
}

func hasArgs(in translate.Intent) bool {
	switch in.Intent {
	case translate.Why, translate.FixSELinux, translate.Snapshots, translate.Pending, translate.Show, translate.Apply:
		return true
	}
	return false
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ------------------------------------------------------------------ decisions

// Case is one labeled case of the shared evaluation suite (basalt-case/v1
// or v1.1, see docs/eval-suite.md). Only the fields the decision layer needs are
// read here.
type Case struct {
	ID         string          `json:"id"`
	Schema     string          `json:"schema"`
	Subject    string          `json:"subject"`
	Evidence   map[string]any  `json:"evidence"`
	Questions  []CaseQuestion  `json:"questions"`
	Expected   json.RawMessage `json:"expected"`
	Provenance struct {
		Source string `json:"source"`
	} `json:"provenance"`
}

// CaseQuestion is one decision-layer question of a case, exactly as the
// diagnosers ask it (features, facts), with the expected answer.
type CaseQuestion struct {
	ID       string          `json:"id"`
	Subject  string          `json:"subject"`
	Features map[string]bool `json:"features"`
	Facts    map[string]any  `json:"facts,omitempty"`
	Want     string          `json:"want"`
	Derive   bool            `json:"derive,omitempty"`
}

var builders = map[string]func(string, map[string]bool) decide.Question{
	"unit.cause": decide.UnitCause, "avc.class": decide.AVCClass, "dnf.next": decide.DnfNext,
	"disk.cause": decide.DiskCause, "event.severity": decide.Severity, "event.notify": decide.Notify,
}

type item struct {
	Case, Question, Source, Want string
	P                            map[string]float64
	Logits                       map[string]float64 // log-probabilities (model) for calibration fits
	MS                           float64
}

func runDecide(args []string) error {
	fs := flag.NewFlagSet("decide", flag.ExitOnError)
	ep := fs.String("endpoint", "", "OpenAI-compatible endpoint (empty: rules only)")
	model := fs.String("model", "", "model name")
	dir := fs.String("cases", "eval/cases", "directory of *.jsonl case files")
	out := fs.String("out", "", "per-item results (JSON lines)")
	label := fs.String("label", "", "label for the summary")
	folds := fs.Int("folds", 5, "cross-validation folds for the temperature fit")
	_ = fs.Parse(args)

	files, _ := filepath.Glob(filepath.Join(*dir, "*.jsonl"))
	var cases []Case
	for _, f := range files {
		if err := readJSONL(f, func(b []byte) error {
			var c Case
			if err := json.Unmarshal(b, &c); err != nil {
				return err
			}
			cases = append(cases, c)
			return nil
		}); err != nil {
			return err
		}
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases in %s", *dir)
	}
	for _, c := range cases {
		if !knownSchemas[c.Schema] {
			return fmt.Errorf("%s: schema %q", c.ID, c.Schema)
		}
	}
	var mb *decide.ModelBackend
	if *ep != "" {
		mb = decide.NewModelBackend(*ep, *model, false)
		mb.C.Timeout = 120 * time.Second
	}
	var w *bufio.Writer
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = bufio.NewWriter(f)
		defer w.Flush()
	}
	var rules, models []item
	for _, c := range cases {
		for _, cq := range c.Questions {
			qid := cq.ID
			b := builders[qid]
			if b == nil {
				return fmt.Errorf("%s: unknown question %s", c.ID, qid)
			}
			q := b(cq.Subject, cq.Features)
			q.Facts = cq.Facts
			want := cq.Want
			ra, err := decide.Rules{}.Answer(context.Background(), q)
			if err != nil {
				return fmt.Errorf("%s: %v", c.ID, err)
			}
			rlg := map[string]float64{}
			for k, v := range ra.Probabilities {
				rlg[k] = math.Log(math.Max(v, 1e-6))
			}
			rules = append(rules, item{Case: c.ID, Question: qid, Source: c.Provenance.Source, Want: want, P: ra.Probabilities, Logits: rlg})
			if mb != nil {
				t0 := time.Now()
				// Raw log-probabilities: temperature 1, kept for the fit.
				ma, err := mb.Answer(context.Background(), q)
				ms := float64(time.Since(t0).Milliseconds())
				if err != nil {
					return fmt.Errorf("%s %s: %v", c.ID, qid, err)
				}
				lg := map[string]float64{}
				for k, v := range ma.Probabilities {
					lg[k] = math.Log(math.Max(v, 1e-6))
				}
				models = append(models, item{Case: c.ID, Question: qid, Source: c.Provenance.Source, Want: want, P: ma.Probabilities, Logits: lg, MS: ms})
				if w != nil {
					b, _ := json.Marshal(map[string]any{"case": c.ID, "q": qid, "want": want, "rules": ra.Probabilities, "model": ma.Probabilities, "ms": ms})
					w.Write(append(b, '\n'))
				}
			} else if w != nil {
				b, _ := json.Marshal(map[string]any{"case": c.ID, "q": qid, "want": want, "rules": ra.Probabilities})
				w.Write(append(b, '\n'))
			}
		}
	}
	sum := map[string]any{"label": *label, "cases": len(cases), "instances": len(rules), "rules": metrics(rules)}
	// The rules' probabilities can be rescaled the same way.
	rcal, rtemps := crossValTemperature(rules, *folds)
	sum["rules_temperature_cv"], sum["rules_temperatures_full_fit"] = metrics(rcal), rtemps
	if mb != nil {
		sum["model"] = metrics(models)
		var lat []float64
		for _, it := range models {
			lat = append(lat, it.MS)
		}
		sum["model_latency_ms_p50"], sum["model_latency_ms_p95"] = pct(lat, 50), pct(lat, 95)
		cal, temps := crossValTemperature(models, *folds)
		sum["model_temperature_cv"] = metrics(cal)
		sum["temperatures_full_fit"] = temps
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sum)
}

// metrics: accuracy, expected calibration error (10 equal-width bins on
// the top probability), multi-class Brier score and negative
// log-likelihood, overall, per question and per source.
func metrics(items []item) map[string]any {
	group := func(xs []item) map[string]any {
		if len(xs) == 0 {
			return nil
		}
		var acc, brier, nll float64
		type bin struct{ n, ok, conf float64 }
		bins := make([]bin, 10)
		for _, it := range xs {
			top, pt := "", -1.0
			keys := make([]string, 0, len(it.P))
			for k := range it.P {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if it.P[k] > pt {
					top, pt = k, it.P[k]
				}
			}
			ok := 0.0
			if top == it.Want {
				ok = 1
			}
			acc += ok
			for _, k := range keys {
				y := 0.0
				if k == it.Want {
					y = 1
				}
				brier += (it.P[k] - y) * (it.P[k] - y)
			}
			nll += -math.Log(math.Max(it.P[it.Want], 1e-6))
			bi := int(pt * 10)
			if bi > 9 {
				bi = 9
			}
			bins[bi].n++
			bins[bi].ok += ok
			bins[bi].conf += pt
		}
		n := float64(len(xs))
		ece := 0.0
		var rel []map[string]any
		for i, b := range bins {
			if b.n == 0 {
				continue
			}
			ece += b.n / n * math.Abs(b.ok/b.n-b.conf/b.n)
			rel = append(rel, map[string]any{"bin": fmt.Sprintf("%.1f-%.1f", float64(i)/10, float64(i+1)/10), "n": b.n,
				"confidence": round(b.conf/b.n, 3), "accuracy": round(b.ok/b.n, 3)})
		}
		return map[string]any{"n": len(xs), "accuracy": round(acc/n, 3), "ece": round(ece, 3),
			"brier": round(brier/n, 3), "nll": round(nll/n, 3), "reliability": rel}
	}
	res := group(items)
	byQ, bySrc := map[string][]item{}, map[string][]item{}
	for _, it := range items {
		byQ[it.Question] = append(byQ[it.Question], it)
		bySrc[it.Source] = append(bySrc[it.Source], it)
	}
	q := map[string]any{}
	for k, v := range byQ {
		g := group(v)
		delete(g, "reliability")
		q[k] = g
	}
	s := map[string]any{}
	for k, v := range bySrc {
		g := group(v)
		delete(g, "reliability")
		s[k] = g
	}
	res["by_question"], res["by_source"] = q, s
	return res
}

func softmaxT(lg map[string]float64, t float64) map[string]float64 {
	mx := math.Inf(-1)
	for _, v := range lg {
		mx = math.Max(mx, v/t)
	}
	p, s := map[string]float64{}, 0.0
	for k, v := range lg {
		p[k] = math.Exp(v/t - mx)
		s += p[k]
	}
	for k := range p {
		p[k] /= s
	}
	return p
}

// fitT picks the temperature minimizing the NLL (grid search).
func fitT(xs []item) float64 {
	best, bestNLL := 1.0, math.Inf(1)
	for t := 0.25; t <= 8.0001; t *= 1.1 {
		nll := 0.0
		for _, it := range xs {
			nll += -math.Log(math.Max(softmaxT(it.Logits, t)[it.Want], 1e-9))
		}
		if nll < bestNLL {
			best, bestNLL = t, nll
		}
	}
	return round(best, 3)
}

// crossValTemperature fits one temperature per question on k-1 folds and
// applies it to the held-out fold, so the calibrated numbers are not
// measured on the data they were fitted on. It also returns the
// temperatures fitted on everything (what would go into the config).
func crossValTemperature(items []item, k int) ([]item, map[string]float64) {
	byQ := map[string][]item{}
	for _, it := range items {
		byQ[it.Question] = append(byQ[it.Question], it)
	}
	var out []item
	full := map[string]float64{}
	for q, xs := range byQ {
		full[q] = fitT(xs)
		for f := 0; f < k; f++ {
			var train, test []item
			for i, it := range xs {
				if i%k == f {
					test = append(test, it)
				} else {
					train = append(train, it)
				}
			}
			if len(test) == 0 {
				continue
			}
			t := 1.0
			if len(train) >= 5 {
				t = fitT(train)
			}
			for _, it := range test {
				it.P = softmaxT(it.Logits, t)
				out = append(out, it)
			}
		}
	}
	return out, full
}

// runDerive completes generated cases (stdin to stdout): a unit.cause
// question marked "derive" gets the journal features and facts from the
// same code `basalt why` uses (diag.JournalFeatures, decide.JournalFacts),
// so generated and real cases are read the same way.
func runDerive() error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for sc.Scan() {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
			return err
		}
		var ev struct {
			Journal []string `json:"journal"`
		}
		_ = json.Unmarshal(raw["evidence"], &ev)
		var qs []CaseQuestion
		if err := json.Unmarshal(raw["questions"], &qs); err != nil {
			return err
		}
		for i := range qs {
			if !qs[i].Derive {
				continue
			}
			qs[i].Derive = false
			if qs[i].Features == nil {
				qs[i].Features = map[string]bool{}
			}
			for k, v := range diag.JournalFeatures(ev.Journal) {
				if v {
					qs[i].Features[k] = true
				}
			}
			qs[i].Facts = decide.JournalFacts(ev.Journal)
		}
		b, err := json.Marshal(qs)
		if err != nil {
			return err
		}
		raw["questions"] = b
		// Keep the field order of the input for readable diffs.
		line, err := orderedJSON(sc.Bytes(), raw)
		if err != nil {
			return err
		}
		out.Write(append(line, '\n'))
	}
	return sc.Err()
}

// orderedJSON re-encodes an object with the key order of orig.
func orderedJSON(orig []byte, vals map[string]json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(orig)))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var buf strings.Builder
	buf.WriteByte('{')
	first := true
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k := t.(string)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vals[k])
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}
