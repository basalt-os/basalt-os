package audit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLog(t *testing.T) (*Log, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := New(p, "test")
	l.Journal = false
	return l, p
}

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append("decision", "record", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
}

// clockAt returns a clock that advances one minute per call, so every
// rotation gets its own sealed file name.
func clockAt(start time.Time) func() time.Time {
	var mu sync.Mutex
	t := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Minute)
		return t
	}
}

func TestRotateChainsAcrossFiles(t *testing.T) {
	l, p := testLog(t)
	now := clockAt(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	appendN(t, l, 3)
	res, err := l.Rotate(RotateOptions{Now: now})
	if err != nil || !res.Rotated {
		t.Fatalf("rotate: %+v %v", res, err)
	}
	if res.Seal.Seq != 4 || res.Seal.Type != TypeSeal || res.Continue.Seq != 5 || res.Continue.Prev != res.Seal.Hash {
		t.Fatalf("seal %+v continue %+v", res.Seal, res.Continue)
	}
	appendN(t, l, 2)
	if _, err := l.Rotate(RotateOptions{Now: now}); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)

	files, _ := Files(p)
	if len(files) != 3 || files[2] != p {
		t.Fatalf("files %v", files)
	}
	s, err := VerifyChain(p)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// 3 + seal + continue + 2 + seal + continue + 1
	if s.LastSeq != 10 || s.Records != 10 || s.Seals != 2 || s.Truncated || s.Unsealed {
		t.Fatalf("summary %+v", s)
	}
	rs, err := Tail(p, 4)
	if err != nil || len(rs) != 4 || rs[0].Type != "decision" || rs[1].Type != TypeSeal || rs[3].Seq != 10 {
		t.Fatalf("tail across files: %+v %v", rs, err)
	}
	// The current file starts with the continue record.
	cur, _ := readRecords(p)
	if cur[0].Type != TypeContinue || cur[0].Seq != 9 {
		t.Fatalf("current file starts with %+v", cur[0])
	}
}

func TestRotateMinSizeAndEmpty(t *testing.T) {
	l, _ := testLog(t)
	if res, err := l.Rotate(RotateOptions{}); err != nil || res.Rotated {
		t.Fatalf("missing log: %+v %v", res, err)
	}
	appendN(t, l, 2)
	res, err := l.Rotate(RotateOptions{MinSize: 1 << 20})
	if err != nil || res.Rotated || !strings.Contains(res.Reason, "below") {
		t.Fatalf("small log: %+v %v", res, err)
	}
	if res, err = l.Rotate(RotateOptions{MinSize: 1}); err != nil || !res.Rotated {
		t.Fatalf("rotate: %+v %v", res, err)
	}
	// Only the continue record since: nothing to seal.
	if res, err = l.Rotate(RotateOptions{MinSize: 1}); err != nil || res.Rotated {
		t.Fatalf("second rotate: %+v %v", res, err)
	}
}

func TestVerifyDetectsTamperingAcrossFiles(t *testing.T) {
	setup := func(t *testing.T) (*Log, string, []string) {
		l, p := testLog(t)
		now := clockAt(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
		appendN(t, l, 3)
		if _, err := l.Rotate(RotateOptions{Now: now}); err != nil {
			t.Fatal(err)
		}
		appendN(t, l, 2)
		files, _ := Files(p)
		if _, err := VerifyChain(p); err != nil {
			t.Fatal(err)
		}
		return l, p, files
	}
	lines := func(path string) []string {
		b, _ := os.ReadFile(path)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	write := func(path string, ls []string) { _ = os.WriteFile(path, []byte(strings.Join(ls, "\n")+"\n"), 0o600) }

	t.Run("record removed from a sealed file", func(t *testing.T) {
		_, p, files := setup(t)
		ls := lines(files[0])
		write(files[0], append(ls[:1], ls[2:]...))
		if _, err := VerifyChain(p); err == nil {
			t.Fatal("not detected")
		}
	})
	t.Run("sealed file truncated with a re-chained tail", func(t *testing.T) {
		// Drop the last data record and re-hash the seal so the in-file chain
		// holds: the seal's SHA-256 and counts still give it away.
		_, p, files := setup(t)
		recs, _ := readRecords(files[0])
		seal := recs[3]
		seal.Seq, seal.Prev = 3, recs[1].Hash
		seal.Hash = hashOf(seal)
		ls := lines(files[0])
		write(files[0], []string{ls[0], ls[1], mustJSON(seal)})
		if _, err := VerifyChain(p); err == nil {
			t.Fatal("not detected")
		}
	})
	t.Run("sealed file deleted in the middle", func(t *testing.T) {
		l, p, _ := setup(t)
		now := clockAt(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
		if _, err := l.Rotate(RotateOptions{Now: now}); err != nil {
			t.Fatal(err)
		}
		files, _ := Files(p)
		if len(files) != 3 {
			t.Fatalf("files %v", files)
		}
		_ = os.Remove(files[1])
		_, err := VerifyChain(p)
		if err == nil || !strings.Contains(err.Error(), "not chained") {
			t.Fatalf("not detected: %v", err)
		}
	})
	t.Run("oldest files removed", func(t *testing.T) {
		_, p, files := setup(t)
		_ = os.Remove(files[0])
		s, err := VerifyChain(p)
		if err != nil || !s.Truncated || s.FirstSeq != 5 {
			t.Fatalf("summary %+v %v", s, err)
		}
	})
	t.Run("continue record edited", func(t *testing.T) {
		_, p, _ := setup(t)
		ls := lines(p)
		ls[0] = strings.Replace(ls[0], `"type":"continue"`, `"type":"decision"`, 1)
		write(p, ls)
		if _, err := VerifyChain(p); err == nil {
			t.Fatal("not detected")
		}
	})
}

func TestUnfinishedRotationResumes(t *testing.T) {
	l, p := testLog(t)
	now := clockAt(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	appendN(t, l, 2)
	// Simulate a crash right after the seal was written.
	name, _ := sealedName(p, now)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(f, mustSize(t, f))
	if _, err := l.appendLocked(f, TypeSeal, "sealed", mustRaw(SealData{File: name, FirstSeq: 1, Records: 2, SHA256: sum})); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := l.Append("decision", "after the seal", nil); !errors.Is(err, ErrSealed) {
		t.Fatalf("append after a seal: %v", err)
	}
	if s, err := VerifyChain(p); err != nil || !s.Unsealed {
		t.Fatalf("verify unfinished: %+v %v", s, err)
	}
	res, err := l.Rotate(RotateOptions{MinSize: 1 << 30, Now: now})
	if err != nil || !res.Rotated || filepath.Base(res.Sealed) != name || res.Seal.Seq != 3 {
		t.Fatalf("resume: %+v %v", res, err)
	}
	appendN(t, l, 1)
	if s, err := VerifyChain(p); err != nil || s.LastSeq != 5 || s.Unsealed {
		t.Fatalf("verify: %+v %v", s, err)
	}
}

func TestConcurrentAppendDuringRotation(t *testing.T) {
	l, p := testLog(t)
	now := clockAt(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	appendN(t, l, 1)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wl := New(p, "writer")
			wl.Journal = false
			for i := 0; i < 50; i++ {
				if _, err := wl.Append("decision", "concurrent", nil); err != nil {
					errs <- err
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		if _, err := l.Rotate(RotateOptions{Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	s, err := VerifyChain(p)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if s.Records-2*int64(s.Seals) != 201 {
		t.Fatalf("records %d with %d seals, want 201 data records", s.Records, s.Seals)
	}
}

func mustJSON(r Record) string {
	b := mustRaw(r)
	return string(b)
}

func mustRaw(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func mustSize(t *testing.T, f *os.File) int64 {
	t.Helper()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}
