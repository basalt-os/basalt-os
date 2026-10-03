package audit

import (
	"os"
	"strings"
	"testing"
)

func TestChainAppendVerifyTamper(t *testing.T) {
	p := t.TempDir() + "/audit.jsonl"
	l := New(p, "test")
	l.Journal = false
	for i, typ := range []string{"decision", "proposal", "apply"} {
		r, err := l.Append(typ, "record", map[string]int{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		if r.Seq != int64(i+1) || len(r.Hash) != 64 {
			t.Fatalf("record %+v", r)
		}
	}
	if n, err := Verify(p); err != nil || n != 3 {
		t.Fatalf("verify: %d %v", n, err)
	}
	rs, _ := Tail(p, 2)
	if len(rs) != 2 || rs[1].Type != "apply" || rs[1].Prev != rs[0].Hash {
		t.Fatalf("tail %+v", rs)
	}
	b, _ := os.ReadFile(p)
	// Edit a record: the hash no longer matches.
	edited := strings.Replace(string(b), `"type":"proposal"`, `"type":"ignore"`, 1)
	_ = os.WriteFile(p, []byte(edited), 0o600)
	if _, err := Verify(p); err == nil || !strings.Contains(err.Error(), "seq 2") {
		t.Fatalf("edit not detected: %v", err)
	}
	// Remove a record: the sequence breaks.
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	_ = os.WriteFile(p, []byte(lines[0]+"\n"+lines[2]+"\n"), 0o600)
	if _, err := Verify(p); err == nil {
		t.Fatal("removal not detected")
	}
}
