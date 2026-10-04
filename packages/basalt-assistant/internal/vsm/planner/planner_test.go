package planner

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Op is one recorded step of the reference runtime: a feed, or a decision
// with the probability of every action character (printed with 9
// decimals, so equal probabilities differ by at most 5e-10 plus float
// noise).
type Op struct {
	Feed   *string            `json:"feed"`
	Decide *string            `json:"decide"`
	P      map[string]float64 `json:"p"`
	HasDec bool               `json:"-"`
}

func (o *Op) UnmarshalJSON(b []byte) error {
	type plain Op
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	_, o.HasDec = raw["decide"]
	return json.Unmarshal(b, (*plain)(o))
}

// Parity is the result of replaying recorded transcripts.
type Parity struct {
	Decisions, Same int
	MaxDiff         float64
}

// Replay feeds every recorded text and compares every decision.
func Replay(t *testing.T, m *Model, ops []Op, par *Parity) {
	t.Helper()
	st := m.NewState()
	for _, o := range ops {
		switch {
		case o.Feed != nil:
			st.Feed(*o.Feed)
		case o.HasDec:
			a, p, ok := st.Decide()
			par.Decisions++
			want := ""
			if o.Decide != nil {
				want = *o.Decide
			}
			got := ""
			if ok {
				got = string(a)
			}
			if got == want {
				par.Same++
			} else if par.Decisions-par.Same <= 5 {
				t.Errorf("decision %q, reference %q", got, want)
			}
			for k, v := range o.P {
				r := []rune(k)[0]
				if d := math.Abs(p[r] - v); d > par.MaxDiff {
					par.MaxDiff = d
				}
			}
		}
	}
}

func readOps(t *testing.T, path string, each func(ops []Op)) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var row struct {
			Ops []Op `json:"ops"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		each(row.Ops)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestTinyParity: a small random model (dimension 8, output biased toward
// the action characters) run by the reference runtime; same decisions,
// same probabilities.
func TestTinyParity(t *testing.T) {
	m, err := Load("testdata/tiny")
	if err != nil {
		t.Fatal(err)
	}
	var par Parity
	readOps(t, "testdata/tiny/transcript.jsonl", func(ops []Op) { Replay(t, m, ops, &par) })
	if par.Decisions < 50 || par.Same != par.Decisions || par.MaxDiff > 1e-6 {
		t.Errorf("parity: %d/%d decisions, max probability difference %.3g", par.Same, par.Decisions, par.MaxDiff)
	}
	t.Logf("tiny model: %d/%d decisions, max |dp| %.3g", par.Same, par.Decisions, par.MaxDiff)
}

// TestShippedParity replays every decision the reference runtime made
// with the shipped weights (BASALT_VSM_PLANNER: weights directory,
// BASALT_VSM_GOLDEN: directory with episodes.jsonl and decide.jsonl).
func TestShippedParity(t *testing.T) {
	dir, gold := os.Getenv("BASALT_VSM_PLANNER"), os.Getenv("BASALT_VSM_GOLDEN")
	if dir == "" || gold == "" {
		t.Skip("BASALT_VSM_PLANNER and BASALT_VSM_GOLDEN not set")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var par Parity
	for _, n := range []string{"episodes.jsonl", "decide.jsonl"} {
		readOps(t, filepath.Join(gold, n), func(ops []Op) { Replay(t, m, ops, &par) })
	}
	if par.Same != par.Decisions || par.MaxDiff > 1e-6 {
		t.Errorf("parity: %d/%d decisions, max probability difference %.3g", par.Same, par.Decisions, par.MaxDiff)
	}
	t.Logf("weights %s: %d/%d decisions identical, max |dp| %.3g", m.SHA256[:12], par.Same, par.Decisions, par.MaxDiff)
}

func TestLoadRefusesMismatch(t *testing.T) {
	d := t.TempDir()
	cfg, _ := os.ReadFile("testdata/tiny/config.txt")
	w, _ := os.ReadFile("testdata/tiny/weights.bin")
	_ = os.WriteFile(filepath.Join(d, "config.txt"), cfg, 0o644)
	_ = os.WriteFile(filepath.Join(d, "weights.bin"), w[:len(w)-4], 0o644)
	if _, err := Load(d); err == nil {
		t.Error("weights of the wrong size were loaded")
	}
}

func BenchmarkDecide(b *testing.B) {
	dir := os.Getenv("BASALT_VSM_PLANNER")
	if dir == "" {
		dir = "testdata/tiny"
	}
	m, err := Load(dir)
	if err != nil {
		b.Fatal(err)
	}
	st := m.NewState()
	for i := 0; i < b.N; i++ {
		st.Reset()
		st.Feed("os diagnose | > U U:uf > J J:pd > A A:av pc pt >")
		st.Decide()
	}
}
