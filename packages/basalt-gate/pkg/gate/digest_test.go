// SPDX-License-Identifier: MIT OR Apache-2.0

package gate

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vector struct {
	Name  string `json:"name"`
	Input struct {
		Calls     []Call     `json:"calls"`
		Preview   Preview    `json:"preview"`
		Resources []Resource `json:"resources"`
		Ref       string     `json:"ref"`
	} `json:"input"`
	Canonical string `json:"canonical"`
	Digest    string `json:"digest"`
	Code      string `json:"code"`
}

// LoadVectors reads testdata/protocol/digest.json (shared with the
// gate's own tests).
func loadVectors(t *testing.T) []vector {
	t.Helper()
	b, err := os.ReadFile("../../testdata/protocol/digest.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []vector `json:"cases"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f.Cases
}

// The digest vectors were computed independently (Python's json module
// with sorted keys): this client gets the same bytes.
func TestDigestVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		d, err := Digest(v.Input.Calls, v.Input.Preview, v.Input.Resources, v.Input.Ref)
		if err != nil || d != v.Digest {
			t.Errorf("%s: %s (%v), want %s", v.Name, d, err, v.Digest)
		}
		if c := ShortCode(d, v.Input.Ref, v.Input.Preview.Commands); c != v.Code {
			t.Errorf("%s: code %s, want %s", v.Name, c, v.Code)
		}
		c, err := canonicalize([]byte(v.Canonical))
		if err != nil || string(c) != v.Canonical {
			t.Errorf("%s: the canonical form is not canonical: %s", v.Name, c)
		}
	}
}
