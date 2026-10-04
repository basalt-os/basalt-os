// Package planner runs VSM's planner core for the Basalt OS domain: a
// gated linear recurrence over characters (one embedding, two gates, two
// heads, dimension 128, about 65,000 parameters). It holds only the
// recurrent state and decides the next action; tools, the knowledge index
// and the binder stay with the caller, so it needs no system access.
//
// The weights come from the package basalt-vsm-planner: weights.bin (raw
// little-endian float32 in a fixed tensor order) and config.txt (dimension,
// vocabulary as character codes, the action characters, the DSL version),
// the same export VSM's reference runtime reads. The arithmetic follows
// that runtime operation by operation (float32 recurrence and heads,
// float64 GELU with the Abramowitz and Stegun erf, float64 softmax) so the
// decisions are the same; the parity test replays its recorded decisions.
package planner

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Model is a loaded planner.
type Model struct {
	Dim     int
	V       int
	DSL     string // e.g. basalt-os-dsl/2
	SHA256  string // of weights.bin
	itos    []rune
	stoi    map[rune]int
	actions []rune
	isAct   map[rune]bool

	emb, wa, ba, wv, bv, wh, bh, wo, bo []float32
}

// Load reads config.txt and weights.bin from dir.
func Load(dir string) (*Model, error) {
	cfg, err := readConfig(filepath.Join(dir, "config.txt"))
	if err != nil {
		return nil, err
	}
	dim, err1 := strconv.Atoi(cfg["dim"])
	v, err2 := strconv.Atoi(cfg["vocab_size"])
	if err1 != nil || err2 != nil || dim <= 0 || v <= 0 || dim > 4096 || v > 4096 {
		return nil, errors.New("config.txt: bad dim or vocab_size")
	}
	itos, err := codes(cfg["vocab_codes"])
	if err != nil || len(itos) != v {
		return nil, errors.New("config.txt: vocab_codes does not match vocab_size")
	}
	acts, err := codes(cfg["actions"])
	if err != nil || len(acts) == 0 {
		return nil, errors.New("config.txt: bad actions")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "weights.bin"))
	if err != nil {
		return nil, err
	}
	want := v*dim + 3*(dim*dim+dim) + v*dim + v
	if len(raw) != 4*want {
		return nil, fmt.Errorf("weights.bin: %d bytes, the config needs %d", len(raw), 4*want)
	}
	all := make([]float32, want)
	for i := range all {
		all[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	sum := sha256.Sum256(raw)
	m := &Model{Dim: dim, V: v, DSL: cfg["dsl"], SHA256: hex.EncodeToString(sum[:]), itos: itos,
		stoi: map[rune]int{}, actions: acts, isAct: map[rune]bool{}}
	for i, c := range itos {
		m.stoi[c] = i
	}
	for _, a := range acts {
		if _, ok := m.stoi[a]; !ok {
			return nil, fmt.Errorf("config.txt: action %q is not in the vocabulary", a)
		}
		m.isAct[a] = true
	}
	off := 0
	take := func(n int) []float32 {
		s := all[off : off+n]
		off += n
		return s
	}
	m.emb = take(v * dim)
	m.wa, m.ba = take(dim*dim), take(dim)
	m.wv, m.bv = take(dim*dim), take(dim)
	m.wh, m.bh = take(dim*dim), take(dim)
	m.wo, m.bo = take(v*dim), take(v)
	return m, nil
}

func readConfig(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), " ")
		if ok {
			cfg[k] = strings.TrimSpace(rest)
		}
	}
	return cfg, sc.Err()
}

func codes(s string) ([]rune, error) {
	var out []rune
	for _, c := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(c))
		if err != nil || n < 0 || n > 0x10FFFF {
			return nil, fmt.Errorf("character code %q", c)
		}
		out = append(out, rune(n))
	}
	return out, nil
}

// Actions are the characters a decision can return.
func (m *Model) Actions() []rune { return append([]rune(nil), m.actions...) }

// State is one episode's recurrent state.
type State struct {
	m    *Model
	h    []float32
	last []float32 // logits after the last fed character; nil before any
	x, a []float32 // scratch
	hh   []float32
}

// NewState starts an episode (zero state).
func (m *Model) NewState() *State {
	return &State{m: m, h: make([]float32, m.Dim), x: make([]float32, m.Dim), a: make([]float32, m.Dim),
		hh: make([]float32, m.Dim)}
}

// Reset zeroes the state.
func (s *State) Reset() {
	for i := range s.h {
		s.h[i] = 0
	}
	s.last = nil
}

// linear: y[o] = b[o] + sum_i w[o*in+i]*x[i], accumulated in float32 in
// index order. The explicit conversion rounds every product, so the
// compiler never fuses a multiply and an add (Go allows that otherwise).
func linear(w, b, x, y []float32, in, out int) {
	for o := 0; o < out; o++ {
		acc := b[o]
		row := w[o*in : o*in+in]
		for i := 0; i < in; i++ {
			acc += float32(row[i] * x[i])
		}
		y[o] = acc
	}
}

func sigmoid(x float32) float32 {
	e := float32(math.Exp(float64(-x)))
	return float32(1) / float32(float32(1)+e)
}

// erf: Abramowitz and Stegun 7.1.26, evaluated as the reference does.
func erf(x float64) float64 {
	s := 1.0
	if x < 0 {
		s = -1
	}
	x = math.Abs(x)
	t := 1.0 / float64(1.0+float64(0.3275911*x))
	p := float64(1.061405429*t) - 1.453152027
	p = float64(p*t) + 1.421413741
	p = float64(p*t) - 0.284496736
	p = float64(p*t) + 0.254829592
	y := 1.0 - float64(float64(p*t)*math.Exp(float64(-x*x)))
	return s * y
}

func gelu(x float32) float32 {
	xd := float64(x)
	return float32(float64(0.5*xd) * float64(1.0+erf(xd/math.Sqrt2)))
}

func (s *State) step(tok int) {
	m := s.m
	x := m.emb[tok*m.Dim : tok*m.Dim+m.Dim]
	linear(m.wa, m.ba, x, s.a, m.Dim, m.Dim)
	linear(m.wv, m.bv, x, s.x, m.Dim, m.Dim)
	for d := 0; d < m.Dim; d++ {
		ad := sigmoid(s.a[d])
		s.h[d] = float32(ad*s.h[d]) + float32(float32(1-ad)*s.x[d])
	}
}

func (s *State) logits() []float32 {
	m := s.m
	linear(m.wh, m.bh, s.h, s.hh, m.Dim, m.Dim)
	for i, z := range s.hh {
		s.hh[i] = gelu(z)
	}
	out := make([]float32, m.V)
	linear(m.wo, m.bo, s.hh, out, m.Dim, m.V)
	return out
}

// Feed runs the characters of text through the recurrence; characters
// outside the vocabulary are skipped.
func (s *State) Feed(text string) {
	fed := false
	for _, c := range text {
		if t, ok := s.m.stoi[c]; ok {
			s.step(t)
			fed = true
		}
	}
	if fed {
		s.last = s.logits()
	}
}

func softmax(l []float32) []float64 {
	mx := math.Inf(-1)
	for _, v := range l {
		if float64(v) > mx {
			mx = float64(v)
		}
	}
	e := make([]float64, len(l))
	z := 0.0
	for i, v := range l {
		e[i] = math.Exp(float64(v) - mx)
		z += e[i]
	}
	for i := range e {
		e[i] /= z
	}
	return e
}

func argmax(l []float32) int {
	bi, bv := 0, l[0]
	for i := 1; i < len(l); i++ {
		if l[i] > bv {
			bi, bv = i, l[i]
		}
	}
	return bi
}

// Decide decodes greedily, up to three characters (each fed back); the
// first action character is the decision, returned with the softmax
// probability of every action character at that position. ok is false
// when no action came (or nothing was fed yet).
func (s *State) Decide() (a rune, probs map[rune]float64, ok bool) {
	for k := 0; k < 3; k++ {
		if s.last == nil {
			return 0, nil, false
		}
		lg := s.last
		c := s.m.itos[argmax(lg)]
		p := softmax(lg)
		s.Feed(string(c))
		if s.m.isAct[c] {
			probs = make(map[rune]float64, len(s.m.actions))
			for _, x := range s.m.actions {
				probs[x] = p[s.m.stoi[x]]
			}
			return c, probs, true
		}
	}
	return 0, nil, false
}
