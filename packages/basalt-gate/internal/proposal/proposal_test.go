package proposal

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDigestStable(t *testing.T) {
	calls := []Call{{Action: "files.trash", Args: map[string]any{"paths": []any{"/home/ana/Downloads/a.iso"}, "bytes": int64(42)}}}
	pv := Preview{TitleKey: "files.trash.title", TitleArgs: map[string]any{"count": int64(1)}}
	res := []Resource{{Kind: "path", Value: "/home/ana/Downloads/a.iso"}}
	d1, err := DigestOf(calls, pv, res, "")
	if err != nil {
		t.Fatal(err)
	}
	// The same content in another map order, numbers as float64 (as JSON
	// decoding gives them) gives the same digest.
	calls2 := []Call{{Action: "files.trash", Args: map[string]any{"bytes": float64(42), "paths": []any{"/home/ana/Downloads/a.iso"}}}}
	d2, _ := DigestOf(calls2, pv, res, "")
	if d1 != d2 || !strings.HasPrefix(d1, "sha256:") || len(d1) != 71 {
		t.Fatalf("%s %s", d1, d2)
	}
	// Any change to what runs or what is shown changes it.
	for name, mut := range map[string]func() (string, error){
		"args": func() (string, error) {
			c := cloneCalls(calls)
			c[0].Args["bytes"] = int64(43)
			return DigestOf(c, pv, res, "")
		},
		"action": func() (string, error) {
			c := cloneCalls(calls)
			c[0].Action = "files.move"
			return DigestOf(c, pv, res, "")
		},
		"preview":   func() (string, error) { p := pv; p.Diff = "x"; return DigestOf(calls, p, res, "") },
		"resources": func() (string, error) { return DigestOf(calls, pv, []Resource{{Kind: "path", Value: "/"}}, "") },
		"ref":       func() (string, error) { return DigestOf(calls, pv, res, "p-123456") },
	} {
		d, err := mut()
		if err != nil || d == d1 {
			t.Errorf("%s: digest unchanged (%v)", name, err)
		}
	}
	// Fractions are not canonical: refused.
	bad := []Call{{Action: "x.y", Args: map[string]any{"n": 1.5}}}
	if _, err := DigestOf(bad, pv, res, ""); err == nil {
		t.Error("a fraction was accepted")
	}
}

func cloneCalls(c []Call) []Call {
	out := make([]Call, len(c))
	for i, x := range c {
		args := map[string]any{}
		for k, v := range x.Args {
			args[k] = v
		}
		out[i] = Call{Action: x.Action, Args: args}
	}
	return out
}

// The short code of a system assistant proposal is the fingerprint
// `basalt show` prints: the first 8 hex digits of SHA-256 over the
// proposal id and its command lines.
func TestShortCodeCompatibility(t *testing.T) {
	if c := ShortCode("sha256:ffff", "p-4f2a9c", []string{"systemctl restart nginx.service"}); c != "2fcf772d" {
		t.Errorf("fingerprint code %s, want 2fcf772d", c)
	}
	if c := ShortCode("sha256:3c1fa02b99", "", nil); c != "3c1fa02b" {
		t.Errorf("digest code %s", c)
	}
	p := &Proposal{Ref: "p-4f2a9c", Calls: []Call{{Action: "unit.restart", Args: map[string]any{"unit": "nginx.service"}}},
		Preview: Preview{TitleKey: "unit.restart.title", Commands: []string{"systemctl restart nginx.service"}}}
	if err := p.Seal(); err != nil || p.Code != "2fcf772d" {
		t.Errorf("%v %s", err, p.Code)
	}
}

func TestClasses(t *testing.T) {
	if MaxClass(C1, C3) != C3 || MaxClass(C4, C2) != C4 || MaxClass("C9", C0) != C5 {
		t.Error("MaxClass")
	}
	if TaintRank("web") <= TaintRank("system") || TaintRank("bogus") != TaintRank("web") || TaintRank("") != 0 {
		t.Error("TaintRank")
	}
	if !ValidID(NewID()) || ValidID("g-xyz") {
		t.Error("ids")
	}
}

func TestWho(t *testing.T) {
	r := Requester{Kind: KindAgent, Name: "claude", Session: "s-0123456789ab", Via: []Party{{Kind: KindAssistant}}}
	if w := r.Who(); w != "Agent claude (session s-0123456789ab) through assistant" {
		t.Error(w)
	}
}

// The gate and the reference client agree on the independent vectors.
func TestDigestVectors(t *testing.T) {
	b, err := os.ReadFile("../../testdata/protocol/digest.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name  string `json:"name"`
			Input struct {
				Calls     []Call     `json:"calls"`
				Preview   Preview    `json:"preview"`
				Resources []Resource `json:"resources"`
				Ref       string     `json:"ref"`
			} `json:"input"`
			Digest string `json:"digest"`
			Code   string `json:"code"`
		} `json:"cases"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.Cases {
		d, err := DigestOf(v.Input.Calls, v.Input.Preview, v.Input.Resources, v.Input.Ref)
		if err != nil || d != v.Digest {
			t.Errorf("%s: %s (%v), want %s", v.Name, d, err, v.Digest)
		}
		if c := ShortCode(d, v.Input.Ref, v.Input.Preview.Commands); c != v.Code {
			t.Errorf("%s: code %s", v.Name, c)
		}
	}
}
