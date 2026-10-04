package audit

import (
	"path/filepath"
	"testing"
	"time"
)

// Rotations within the same second get the names <stem>-TS.jsonl,
// <stem>-TS-2.jsonl, ...; the chain must still be read in sealing order.
func TestRotateSameSecond(t *testing.T) {
	l, p := testLog(t)
	fixed := func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }
	for i := 0; i < 11; i++ {
		appendN(t, l, 2)
		res, err := l.Rotate(RotateOptions{Now: fixed})
		if err != nil || !res.Rotated {
			t.Fatalf("rotate %d: %+v %v", i, res, err)
		}
	}
	appendN(t, l, 1)
	files, err := Files(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"audit-20261004T120000Z.jsonl", "audit-20261004T120000Z-2.jsonl", "audit-20261004T120000Z-3.jsonl"}
	for i, w := range want {
		if filepath.Base(files[i]) != w {
			t.Fatalf("file %d is %s, want %s (all: %v)", i, filepath.Base(files[i]), w, files)
		}
	}
	if filepath.Base(files[10]) != "audit-20261004T120000Z-11.jsonl" || files[11] != p {
		t.Fatalf("order: %v", files)
	}
	s, err := VerifyChain(p)
	if err != nil || s.Seals != 11 || s.Truncated {
		t.Fatalf("verify: %+v %v", s, err)
	}
}
