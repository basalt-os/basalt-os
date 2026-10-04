// Package knowledge reads the assistant's knowledge index: the cases VSM
// (the diagnosis engine of ADR 0004) looks up by evidence pattern. The
// index is built elsewhere (the knowledge build, never on the machine) and
// shipped as data (package basalt-knowledge); this package only reads it.
//
// A case (schema basalt-knowledge/v1) records which goal it answers, the
// evidence pattern it requires and excludes (two-letter feature codes),
// the decision-layer answer it implies, a one-sentence diagnosis, typed
// action templates from the closed action set, the checks that confirm the
// fix (a case without them is only a hint), the version range where it
// applies, optional journal extractors and its provenance and license.
//
// On disk, one directory:
//
//	cases.jsonl    one canonical JSON record per case, sorted by id
//	index.bin      format BKIX0001: a case table (payload offset and
//	               length, goal, number of required codes), a sorted code
//	               table and the posting list of every code
//	manifest.json  schema, format, DSL version, counts, the SHA-256 of
//	               cases.jsonl (the content address) and the case ids
//
// The manifest's SHA-256 is checked when the index is opened; a mismatch,
// a truncated table or a payload that disagrees with its table entry is an
// error, so a damaged index is never used (the caller falls back to the
// rules).
package knowledge

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Schema and format this reader understands.
const (
	Schema = "basalt-knowledge/v1"
	Magic  = "BKIX0001"
)

// Goals in the order of their ids in index.bin (sorted names).
var Goals = []string{"check_disk", "diagnose", "explain_denial", "rollback_candidate"}

// K is the number of candidates a lookup returns.
const K = 3

var reCode = regexp.MustCompile(`^[a-z]{2}$`)

// Pattern is the evidence a case requires (All) and excludes (None).
type Pattern struct {
	All  []string `json:"all"`
	None []string `json:"none"`
}

// Template is a typed action with parameters; a value starting with "$"
// names a binding the binder fills in.
type Template struct {
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params"`
}

// Range limits where a case applies.
type Range struct {
	Fedora   *VersionRange            `json:"fedora,omitempty"`
	Packages map[string]VersionBounds `json:"packages,omitempty"`
}

// VersionRange is an integer range (Fedora releases).
type VersionRange struct {
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

// VersionBounds is a package version range ("1.24", "2.0.3-1").
type VersionBounds struct {
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
}

// Case is one knowledge record.
type Case struct {
	Schema       string            `json:"schema"`
	ID           string            `json:"id"`
	Goal         string            `json:"goal"`
	Pattern      Pattern           `json:"pattern"`
	Answers      map[string]string `json:"answers"`
	Diagnosis    string            `json:"diagnosis"`
	Actions      []Template        `json:"actions"`
	Verification []map[string]any  `json:"verification"`
	Applies      Range             `json:"applies"`
	Extract      map[string]string `json:"extract,omitempty"`
	Provenance   map[string]any    `json:"provenance"`
	License      string            `json:"license"`
}

// Actionable: the case has typed actions and the checks that verify them.
// Anything else is shown as a hint only.
func (c *Case) Actionable() bool { return len(c.Actions) > 0 && len(c.Verification) > 0 }

// Manifest is manifest.json.
type Manifest struct {
	Schema string   `json:"schema"`
	Format string   `json:"format"`
	DSL    string   `json:"dsl"`
	Cases  int      `json:"cases"`
	Codes  int      `json:"codes"`
	SHA256 string   `json:"sha256"`
	IDs    []string `json:"ids"`
	// IndexSHA256 is the SHA-256 of index.bin (newer builds); checked when present.
	IndexSHA256 string `json:"index_sha256,omitempty"`
	BuildID     string `json:"build_id,omitempty"`
}

// Context is what version gating needs: the Fedora release (0: unknown,
// no gating on it) and installed package versions.
type Context struct {
	Fedora   int
	Packages map[string]string
}

// Hit is one lookup result: a case and its match statistics.
type Hit struct {
	Case *Case
	R    int // required codes
	M    int // required codes present in the evidence
	X    int // excluded codes present in the evidence (contradictions)
}

// Full: the evidence supports the case completely (every required code,
// no contradiction). The deterministic guard accepts only full matches.
func (h Hit) Full() bool { return h.M == h.R && h.X == 0 }

// Index is an opened knowledge index.
type Index struct {
	Dir        string
	Manifest   Manifest
	Extractors map[string]*regexp.Regexp
	// ExtractErrors names extractors that do not compile here (they are
	// left out; the knowledge build checks them for RE2 syntax).
	ExtractErrors []string

	cases    []*Case
	goals    []uint8
	required []uint8
	postings map[string][]uint32
}

type posting struct{ off, n uint32 }

// Open reads and checks an index directory.
func Open(dir string) (*Index, error) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var man Manifest
	if err := json.Unmarshal(mb, &man); err != nil {
		return nil, fmt.Errorf("manifest.json: %v", err)
	}
	if man.Schema != Schema || man.Format != Magic {
		return nil, fmt.Errorf("manifest.json: schema %q format %q, want %s %s", man.Schema, man.Format, Schema, Magic)
	}
	body, err := os.ReadFile(filepath.Join(dir, "cases.jsonl"))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != man.SHA256 {
		return nil, errors.New("cases.jsonl does not match the manifest's SHA-256")
	}
	bin, err := os.ReadFile(filepath.Join(dir, "index.bin"))
	if err != nil {
		return nil, err
	}
	if man.IndexSHA256 != "" {
		s := sha256.Sum256(bin)
		if hex.EncodeToString(s[:]) != man.IndexSHA256 {
			return nil, errors.New("index.bin does not match the manifest's SHA-256")
		}
	}
	ix, err := parse(bin, body)
	if err != nil {
		return nil, fmt.Errorf("index.bin: %v", err)
	}
	ix.Dir, ix.Manifest = dir, man
	if len(ix.cases) != man.Cases || len(ix.postings) != man.Codes || len(man.IDs) != man.Cases {
		return nil, errors.New("index.bin and manifest.json disagree on the counts")
	}
	for i, c := range ix.cases {
		if c.ID != man.IDs[i] {
			return nil, fmt.Errorf("case %d is %q, the manifest says %q", i, c.ID, man.IDs[i])
		}
	}
	ix.Extractors = map[string]*regexp.Regexp{}
	for _, c := range ix.cases {
		codes := make([]string, 0, len(c.Extract))
		for code := range c.Extract {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			rx, err := regexp.Compile(c.Extract[code])
			if err != nil || !reCode.MatchString(code) {
				ix.ExtractErrors = append(ix.ExtractErrors, c.ID+"/"+code)
				continue
			}
			ix.Extractors[code] = rx
		}
	}
	return ix, nil
}

func parse(bin, body []byte) (*Index, error) {
	if len(bin) < 16 || string(bin[:8]) != Magic {
		return nil, errors.New("not a BKIX0001 index")
	}
	le := binary.LittleEndian
	n, ncodes := int(le.Uint32(bin[8:])), int(le.Uint32(bin[12:]))
	ct := 16
	codet := ct + n*14
	post := codet + ncodes*10
	if n < 0 || ncodes < 0 || post > len(bin) {
		return nil, errors.New("truncated tables")
	}
	ix := &Index{postings: map[string][]uint32{}}
	for i := 0; i < n; i++ {
		o := ct + i*14
		off, ln := le.Uint64(bin[o:]), le.Uint32(bin[o+8:])
		g, r := bin[o+12], bin[o+13]
		if off+uint64(ln) > uint64(len(body)) || int(g) >= len(Goals) {
			return nil, fmt.Errorf("case %d: payload or goal out of range", i)
		}
		var c Case
		dec := json.NewDecoder(bytes.NewReader(body[off : off+uint64(ln)]))
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("case %d: %v", i, err)
		}
		if err := validate(&c); err != nil {
			return nil, fmt.Errorf("case %s: %v", c.ID, err)
		}
		if c.Goal != Goals[g] || len(c.Pattern.All) != int(r) {
			return nil, fmt.Errorf("case %s: payload disagrees with its table entry", c.ID)
		}
		ix.cases = append(ix.cases, &c)
		ix.goals = append(ix.goals, g)
		ix.required = append(ix.required, r)
	}
	for i := 0; i < ncodes; i++ {
		o := codet + i*10
		code := string(bin[o : o+2])
		p := posting{le.Uint32(bin[o+2:]), le.Uint32(bin[o+6:])}
		start := post + int(p.off)*4
		end := start + int(p.n)*4
		if !reCode.MatchString(code) || end > len(bin) {
			return nil, fmt.Errorf("code %q: bad entry", code)
		}
		lst := make([]uint32, p.n)
		for j := range lst {
			lst[j] = le.Uint32(bin[start+j*4:])
			if int(lst[j]) >= n {
				return nil, fmt.Errorf("code %q: posting out of range", code)
			}
		}
		ix.postings[code] = lst
	}
	return ix, nil
}

func validate(c *Case) error {
	if c.Schema != Schema {
		return fmt.Errorf("schema %q", c.Schema)
	}
	if len(c.Pattern.All) == 0 {
		return errors.New("empty pattern")
	}
	for _, code := range append(append([]string{}, c.Pattern.All...), c.Pattern.None...) {
		if !reCode.MatchString(code) {
			return fmt.Errorf("feature code %q", code)
		}
	}
	if c.Diagnosis == "" || c.License == "" {
		return errors.New("missing diagnosis or license")
	}
	return nil
}

// Len is the number of cases.
func (ix *Index) Len() int { return len(ix.cases) }

// Case returns case i (in id order).
func (ix *Index) Case(i int) *Case { return ix.cases[i] }

// CaseByID finds a case.
func (ix *Index) CaseByID(id string) *Case {
	for _, c := range ix.cases {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// Version is a short identity of the index for logs and the audit trail.
func (ix *Index) Version() string {
	s := ix.Manifest.SHA256
	if len(s) > 12 {
		s = s[:12]
	}
	v := fmt.Sprintf("knowledge %s (%d cases)", s, len(ix.cases))
	if ix.Manifest.BuildID != "" {
		v = fmt.Sprintf("knowledge %s %s (%d cases)", ix.Manifest.BuildID, s, len(ix.cases))
	}
	return v
}

// Lookup returns the cases of goal that share at least one code with the
// evidence and apply in ctx, ranked by matched/required minus
// contradictions, then by specificity (more required codes first), then
// by id; the first K.
func (ix *Index) Lookup(goal string, evidence map[string]bool, ctx Context) []Hit {
	gid := -1
	for i, g := range Goals {
		if g == goal {
			gid = i
		}
	}
	if gid < 0 {
		return nil
	}
	hits := map[uint32]int{}
	for code, on := range evidence {
		if !on {
			continue
		}
		for _, j := range ix.postings[code] {
			hits[j]++
		}
	}
	var res []Hit
	for j, m := range hits {
		if int(ix.goals[j]) != gid {
			continue
		}
		c := ix.cases[j]
		if !Applies(c, ctx) {
			continue // retired outside its version range
		}
		x := 0
		for _, code := range c.Pattern.None {
			if evidence[code] {
				x++
			}
		}
		res = append(res, Hit{Case: c, R: int(ix.required[j]), M: m, X: x})
	}
	sort.Slice(res, func(a, b int) bool { return Less(res[a], res[b]) })
	if len(res) > K {
		res = res[:K]
	}
	return res
}

// Score is matched/required minus contradictions.
func (h Hit) Score() float64 { return float64(h.M)/float64(h.R) - float64(h.X) }

// Less is the rank order: higher score, then more required codes, then id.
func Less(a, b Hit) bool {
	sa, sb := a.Score(), b.Score()
	if sa != sb {
		return sa > sb
	}
	if a.R != b.R {
		return a.R > b.R
	}
	return a.Case.ID < b.Case.ID
}

// Format renders candidates as the planner sees them:
// "0:3/3-0a 1:2/4-1h 2:-" (matched/required-contradicted, a: actionable,
// h: hint).
func Format(hits []Hit) string {
	parts := make([]string, 0, K)
	for i := 0; i < K; i++ {
		if i < len(hits) {
			h := hits[i]
			kind := "h"
			if h.Case.Actionable() {
				kind = "a"
			}
			parts = append(parts, fmt.Sprintf("%d:%d/%d-%d%s", i, h.M, h.R, h.X, kind))
		} else {
			parts = append(parts, fmt.Sprintf("%d:-", i))
		}
	}
	return strings.Join(parts, " ")
}

// OraclePick is the reference decision: the most specific full match,
// else -1 (abstain).
func OraclePick(hits []Hit) int {
	best := -1
	for i, h := range hits {
		if h.Full() && (best < 0 || h.R > hits[best].R) {
			best = i
		}
	}
	return best
}

// Applies reports whether the case is inside its version range for ctx.
func Applies(c *Case, ctx Context) bool {
	if fr := c.Applies.Fedora; fr != nil && ctx.Fedora != 0 {
		if fr.Min != nil && ctx.Fedora < *fr.Min {
			return false
		}
		if fr.Max != nil && ctx.Fedora > *fr.Max {
			return false
		}
	}
	for pkg, rng := range c.Applies.Packages {
		have, ok := ctx.Packages[pkg]
		if !ok || have == "" {
			continue
		}
		v := vtuple(have)
		if rng.Min != "" && cmpTuple(v, vtuple(rng.Min)) < 0 {
			return false
		}
		if rng.Max != "" && cmpTuple(v, vtuple(rng.Max)) > 0 {
			return false
		}
	}
	return true
}

var reDigits = regexp.MustCompile(`\d+`)

func vtuple(s string) []int {
	var out []int
	for _, d := range reDigits.FindAllString(s, -1) {
		n, err := strconv.Atoi(d)
		if err != nil {
			n = 1 << 30
		}
		out = append(out, n)
	}
	return out
}

// cmpTuple compares like Python tuples.
func cmpTuple(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

var rePlaceholder = regexp.MustCompile(`\{[a-z_]+\}`)

// Render fills the diagnosis's {binding} placeholders; when one is
// missing every placeholder becomes "?".
func Render(c *Case, bindings map[string]string) string {
	missing := false
	out := rePlaceholder.ReplaceAllStringFunc(c.Diagnosis, func(m string) string {
		v, ok := bindings[m[1:len(m)-1]]
		if !ok {
			missing = true
		}
		return v
	})
	if missing {
		return rePlaceholder.ReplaceAllString(c.Diagnosis, "?")
	}
	return out
}
