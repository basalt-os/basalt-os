package knowledge

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// golden lookups recorded from the reference reader (VSM's Python index)
// on the same index: evidence -> ranked candidates with their statistics,
// the planner's view of them and the reference pick.
type lookupRow struct {
	Goal     string   `json:"goal"`
	Evidence []string `json:"evidence"`
	Ctx      struct {
		Fedora *int `json:"fedora"`
	} `json:"ctx"`
	Hits []struct {
		ID      string `json:"id"`
		M, R, X int
	} `json:"hits"`
	Fmt    string `json:"fmt"`
	Oracle *int   `json:"oracle"`
}

func checkLookups(t *testing.T, ix *Index, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	n := 0
	for sc.Scan() {
		var row lookupRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		ev := map[string]bool{}
		for _, c := range row.Evidence {
			ev[c] = true
		}
		ctx := Context{}
		if row.Ctx.Fedora != nil {
			ctx.Fedora = *row.Ctx.Fedora
		}
		hits := ix.Lookup(row.Goal, ev, ctx)
		var got, want []string
		for _, h := range hits {
			got = append(got, h.Case.ID+" "+itoa(h.M)+"/"+itoa(h.R)+"-"+itoa(h.X))
		}
		for _, h := range row.Hits {
			want = append(want, h.ID+" "+itoa(h.M)+"/"+itoa(h.R)+"-"+itoa(h.X))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s %v %+v: got %v, want %v", row.Goal, row.Evidence, ctx, got, want)
		}
		if f := Format(hits); f != row.Fmt {
			t.Errorf("%s %v: format %q, want %q", row.Goal, row.Evidence, f, row.Fmt)
		}
		wantPick := -1
		if row.Oracle != nil {
			wantPick = *row.Oracle
		}
		if p := OraclePick(hits); p != wantPick {
			t.Errorf("%s %v: oracle pick %d, want %d", row.Goal, row.Evidence, p, wantPick)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestLookupGolden(t *testing.T) {
	ix, err := Open("testdata/index")
	if err != nil {
		t.Fatal(err)
	}
	if ix.Len() != 18 || ix.Manifest.DSL != "basalt-os-dsl/3" || ix.Manifest.IndexSHA256 == "" {
		t.Fatalf("index: %d cases, dsl %q", ix.Len(), ix.Manifest.DSL)
	}
	if ix.Extractors["cx"] == nil {
		t.Errorf("extractor cx missing: %v", ix.ExtractErrors)
	}
	if n := checkLookups(t, ix, "testdata/lookup.jsonl"); n < 100 {
		t.Errorf("only %d lookups", n)
	}
}

// TestLookupShipped checks the shipped index against the reference when
// it is available (BASALT_VSM_KNOWLEDGE: index directory,
// BASALT_VSM_GOLDEN: directory with lookup.jsonl recorded on it).
func TestLookupShipped(t *testing.T) {
	dir, gold := os.Getenv("BASALT_VSM_KNOWLEDGE"), os.Getenv("BASALT_VSM_GOLDEN")
	if dir == "" || gold == "" {
		t.Skip("BASALT_VSM_KNOWLEDGE and BASALT_VSM_GOLDEN not set")
	}
	ix, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := checkLookups(t, ix, filepath.Join(gold, "lookup.jsonl"))
	t.Logf("%d lookups on %s match the reference", n, ix.Version())
}

func copyIndex(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, n := range []string{"cases.jsonl", "index.bin", "manifest.json"} {
		b, err := os.ReadFile(filepath.Join("testdata/index", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestDamagedIndexRefused(t *testing.T) {
	// A changed case (the manifest's SHA-256 no longer matches).
	d := copyIndex(t)
	p := filepath.Join(d, "cases.jsonl")
	b, _ := os.ReadFile(p)
	b = []byte(strings.Replace(string(b), `"selinux.boolean"`, `"selinux.boolean "`, 1))
	_ = os.WriteFile(p, b, 0o644)
	if _, err := Open(d); err == nil {
		t.Error("an index whose cases do not match the manifest was opened")
	}
	// A truncated index.bin.
	d = copyIndex(t)
	p = filepath.Join(d, "index.bin")
	b, _ = os.ReadFile(p)
	_ = os.WriteFile(p, b[:len(b)-7], 0o644)
	if _, err := Open(d); err == nil {
		t.Error("a truncated index.bin was opened")
	}
	// A wrong magic.
	d = copyIndex(t)
	p = filepath.Join(d, "index.bin")
	b, _ = os.ReadFile(p)
	copy(b, "BKIX0002")
	_ = os.WriteFile(p, b, 0o644)
	if _, err := Open(d); err == nil {
		t.Error("an index with another format was opened")
	}
	// The manifest names an index.bin hash that does not match.
	d = copyIndex(t)
	p = filepath.Join(d, "manifest.json")
	var m map[string]any
	b, _ = os.ReadFile(p)
	_ = json.Unmarshal(b, &m)
	m["index_sha256"] = strings.Repeat("0", 64)
	b, _ = json.Marshal(m)
	_ = os.WriteFile(p, b, 0o644)
	if _, err := Open(d); err == nil {
		t.Error("an index.bin that does not match index_sha256 was opened")
	}
}

func TestVersionGating(t *testing.T) {
	ix, err := Open("testdata/index")
	if err != nil {
		t.Fatal(err)
	}
	ev := map[string]bool{"cx": true, "uf": true}
	if h := ix.Lookup("diagnose", ev, Context{Fedora: 44}); len(h) == 0 || h[0].Case.ID != "kb-add-unit-cert-expired" {
		t.Errorf("Fedora 44: %v", h)
	}
	for _, h := range ix.Lookup("diagnose", ev, Context{Fedora: 46}) {
		if h.Case.ID == "kb-add-unit-cert-expired" {
			t.Error("a case outside its Fedora range was returned")
		}
	}
	c := &Case{Applies: Range{Packages: map[string]VersionBounds{"nginx": {Min: "1.24", Max: "1.26.9"}}}}
	for v, want := range map[string]bool{"1.24.0-1.fc44": true, "1.20.1": false, "1.27.0": false, "": true} {
		if got := Applies(c, Context{Packages: map[string]string{"nginx": v}}); got != want {
			t.Errorf("nginx %q: applies %v, want %v", v, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	c := &Case{Diagnosis: "{unit} cannot bind port {port}"}
	if s := Render(c, map[string]string{"unit": "nginx.service", "port": "8181"}); s != "nginx.service cannot bind port 8181" {
		t.Error(s)
	}
	if s := Render(c, map[string]string{"unit": "nginx.service"}); s != "? cannot bind port ?" {
		t.Error(s)
	}
}
