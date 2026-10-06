package canon

import (
	"errors"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	cases := map[string]string{
		`{"b":1,"a":[true,false,null],"c":{"z":"x","y":""}}`: `{"a":[true,false,null],"b":1,"c":{"y":"","z":"x"}}`,
		`  { "k" : "<&> é" }  `:                              "{\"k\":\"<&> é\"}",
		`{"s":"tab\tnl\nq\"bs\\ctl\u0001"}`:                  `{"s":"tab\tnl\nq\"bs\\ctl\u0001"}`,
		`[-9007199254740991,0,9007199254740991]`:             `[-9007199254740991,0,9007199254740991]`,
		`"é"`:                                                `"é"`,
	}
	for in, want := range cases {
		got, err := Canonicalize([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %s (%v), want %s", in, got, err, want)
		}
	}
}

func TestRefused(t *testing.T) {
	for _, in := range []string{`1.5`, `1e3`, `9007199254740992`, `{"é":1}`, `[1] [2]`} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		} else if in != `[1] [2]` && !errors.Is(err, ErrNotCanonical) {
			t.Errorf("%s: %v", in, err)
		}
	}
}

func TestMarshalStruct(t *testing.T) {
	type s struct {
		Z string `json:"z"`
		A int    `json:"a"`
	}
	b, err := Marshal(s{Z: "x", A: 2})
	if err != nil || string(b) != `{"a":2,"z":"x"}` {
		t.Fatalf("%s %v", b, err)
	}
}
