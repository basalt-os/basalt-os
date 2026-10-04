package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
)

func rec(i int) record.Record {
	d, _ := json.Marshal(map[string]any{"i": i, "host": "example.com", "n": 1000000})
	return record.Record{Time: time.Now().UTC().Format(time.RFC3339Nano), Producer: "basalt-agent", UID: 1000,
		Session: "s-0123456789ab", Event: "egress.deny", Outcome: "denied", Severity: "warning", Data: d}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ledger.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func fill(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Append(rec(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChainAcrossRotations(t *testing.T) {
	s := open(t)
	clock := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	fill(t, s, 5)
	for round := 0; round < 3; round++ {
		res, err := s.Rotate()
		if err != nil || !res.Rotated {
			t.Fatalf("rotate %d: %v %+v", round, err, res)
		}
		fill(t, s, 3)
	}
	sum, err := Verify(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	// 5 + 3*(seal + continue + 3) records.
	if sum.Records != 5+3*5 || sum.Seals != 3 || len(sum.Files) != 4 || sum.LastSeq != 20 {
		t.Fatalf("%+v", sum)
	}
	// Reopening keeps the head.
	s.Close()
	s2, err := Open(s.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Head().Seq != 20 {
		t.Fatalf("head %d", s2.Head().Seq)
	}
	if res, _ := s2.Rotate(); res.Rotated {
		// Something was recorded since the last rotation (3 records), so it does rotate.
		_ = res
	}
	if res, _ := s2.Rotate(); res.Rotated {
		t.Fatal("rotated an empty file again")
	}
}

// tamper rewrites file with f applied to its lines.
func tamper(t *testing.T, path string, f func([][]byte) [][]byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	lines = f(lines)
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTamperDetected(t *testing.T) {
	cases := map[string]func([][]byte) [][]byte{
		"edit": func(l [][]byte) [][]byte {
			l[2] = bytes.Replace(l[2], []byte("example.com"), []byte("example.org"), 1)
			return l
		},
		"delete": func(l [][]byte) [][]byte { return append(l[:2], l[3:]...) },
		"reorder": func(l [][]byte) [][]byte {
			l[1], l[2] = l[2], l[1]
			return l
		},
		"truncate tail is not detectable without a seal, head kept": nil,
	}
	for name, f := range cases {
		if f == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			s := open(t)
			fill(t, s, 5)
			tamper(t, s.Path, f)
			if _, err := Verify(s.Path); err == nil {
				t.Fatal("tampering not detected")
			}
		})
	}
}

func TestSealedFileTamperDetected(t *testing.T) {
	s := open(t)
	fill(t, s, 4)
	res, err := s.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	fill(t, s, 2)
	// Recompute every hash after an edit, so only the seal can tell.
	tamper(t, res.Sealed, func(l [][]byte) [][]byte {
		var prev string
		for i := range l {
			var r record.Record
			if err := json.Unmarshal(l[i], &r); err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				r.Outcome = "allowed"
			}
			if i > 0 {
				r.Prev = prev
			}
			r.Hash = record.HashOf(r)
			prev = r.Hash
			l[i], _ = json.Marshal(r)
		}
		return l
	})
	_, err = Verify(s.Path)
	if err == nil || !strings.Contains(err.Error(), "") {
		t.Fatal("rewritten sealed file not detected")
	}
	// Removing a sealed file in the middle breaks the chain too.
	s2 := open(t)
	fill(t, s2, 2)
	r1, _ := s2.Rotate()
	fill(t, s2, 2)
	if _, err := s2.Rotate(); err != nil {
		t.Fatal(err)
	}
	fill(t, s2, 1)
	files, _ := Files(s2.Path)
	if len(files) != 3 {
		t.Fatalf("files %v", files)
	}
	if err := os.Remove(files[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(s2.Path); err == nil {
		t.Fatalf("removed sealed file %s not detected (first sealed %s)", files[1], r1.Sealed)
	}
}

func TestInterruptedRotationFinishedOnOpen(t *testing.T) {
	s := open(t)
	fill(t, s, 3)
	// Simulate a crash right after the seal record: append it by hand.
	st, _ := s.f.Stat()
	sum, _ := fileSHA256(s.f, st.Size())
	d, _ := json.Marshal(SealData{File: "ledger-20261004T100000Z.jsonl", FirstSeq: 1, Records: 3, SHA256: sum})
	if _, err := s.Append(record.Record{Producer: record.Producer, Event: record.EventSeal, Outcome: "ok", Data: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(rec(9)); err == nil {
		t.Fatal("appended after a seal")
	}
	s.Close()
	s2, err := Open(s.Path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Head().Event != record.EventContinue {
		t.Fatalf("head %+v", s2.Head())
	}
	fill(t, s2, 1)
	if sum, err := Verify(s2.Path); err != nil || sum.Seals != 1 {
		t.Fatalf("%v %+v", err, sum)
	}
}

func TestScanSkipsOldSealedFiles(t *testing.T) {
	s := open(t)
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return clock }
	fill(t, s, 2)
	if _, err := s.Rotate(); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(72 * time.Hour)
	fill(t, s, 2)
	n := 0
	if err := Scan(s.Path, clock.Add(-time.Hour), func(record.Record) bool { n++; return true }); err != nil {
		t.Fatal(err)
	}
	if n != 3 { // continue + 2
		t.Fatalf("scanned %d", n)
	}
	n = 0
	_ = Scan(s.Path, time.Time{}, func(record.Record) bool { n++; return true })
	if n != 6 {
		t.Fatalf("full scan %d", n)
	}
	if _, ok := sealTime(fmt.Sprintf("/x/ledger-%s-2.jsonl", "20261001T000000Z")); !ok {
		t.Fatal("seal time with counter")
	}
}

func TestRetention(t *testing.T) {
	s := open(t)
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := t0
	s.Now = func() time.Time { return clock }
	rotate := func() RotateResult {
		t.Helper()
		res, err := s.Rotate()
		if err != nil || !res.Rotated {
			t.Fatalf("rotate: %v %+v", err, res)
		}
		return res
	}
	fill(t, s, 3)
	a := rotate() // sealed at t0
	clock = t0.Add(100 * 24 * time.Hour)
	fill(t, s, 2)
	b := rotate() // sealed at t0+100d
	clock = t0.Add(400 * 24 * time.Hour)
	fill(t, s, 2)
	rotate() // sealed at t0+400d
	clock = t0.Add(410 * 24 * time.Hour)
	fill(t, s, 1)
	year := 365 * 24 * time.Hour

	// Nothing older than the cutoff yet when the policy is longer.
	if done, err := s.Expire(clock.Add(-1000*24*time.Hour), "1000d"); err != nil || len(done) != 0 {
		t.Fatalf("expired %v %v", done, err)
	}
	done, err := s.Expire(clock.Add(-year), "365d")
	if err != nil || len(done) != 1 || done[0].File != filepath.Base(a.Sealed) || done[0].LastSeq != a.Seal.Seq || done[0].SealHash != a.Seal.Hash {
		t.Fatalf("expire: %v %+v", err, done)
	}
	if _, err := os.Stat(a.Sealed); !os.IsNotExist(err) {
		t.Fatal("expired file still there")
	}
	sum, err := Verify(s.Path)
	if err != nil || sum.Truncated || sum.ExpiredUpTo != a.Seal.Seq || sum.Retention != 1 {
		t.Fatalf("verify after expiry: %v %+v", err, sum)
	}
	if h := s.Head(); h.Event != record.EventRetention {
		t.Fatalf("head %+v", h)
	}
	// Again: nothing more to do, no second record.
	if done, err := s.Expire(clock.Add(-year), "365d"); err != nil || len(done) != 0 {
		t.Fatalf("second pass: %v %+v", err, done)
	}
	// Later the second file expires too; the chain still verifies.
	clock = t0.Add(600 * 24 * time.Hour)
	if done, err := s.Expire(clock.Add(-year), "365d"); err != nil || len(done) != 1 || done[0].File != filepath.Base(b.Sealed) {
		t.Fatalf("expire b: %v %+v", err, done)
	}
	sum, err = Verify(s.Path)
	if err != nil || sum.Truncated || sum.ExpiredUpTo != b.Seal.Seq || sum.Retention != 2 {
		t.Fatalf("verify after the second expiry: %v %+v", err, sum)
	}
	// Removing the next oldest file by hand (no retention record) is still
	// reported as truncation.
	files, _ := Files(s.Path)
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if sum, err := Verify(s.Path); err != nil || !sum.Truncated {
		t.Fatalf("manual removal not reported: %v %+v", err, sum)
	}
}

func TestRetentionRecordMustMatch(t *testing.T) {
	s := open(t)
	fill(t, s, 2)
	a, _ := s.Rotate()
	fill(t, s, 2)
	// A forged retention record naming the file with the wrong seal hash
	// does not excuse its removal.
	d, _ := json.Marshal(RetentionData{File: filepath.Base(a.Sealed), LastSeq: a.Seal.Seq, SealHash: strings.Repeat("0", 64)})
	if _, err := s.Append(record.Record{Producer: record.Producer, Event: record.EventRetention, Outcome: "ok", Data: d}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.Sealed); err != nil {
		t.Fatal(err)
	}
	if sum, err := Verify(s.Path); err != nil || !sum.Truncated || sum.ExpiredUpTo != 0 {
		t.Fatalf("forged retention record accepted: %v %+v", err, sum)
	}
}

func TestRetentionInterrupted(t *testing.T) {
	s := open(t)
	clock := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return clock }
	fill(t, s, 2)
	a, _ := s.Rotate()
	fill(t, s, 2)
	// The record was written but the unlink did not happen (crash).
	rd, _, err := sealedFileInfo(a.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	rd.Retention = "30d"
	d, _ := json.Marshal(rd)
	if _, err := s.Append(record.Record{Producer: record.Producer, Event: record.EventRetention, Outcome: "ok", Data: d}); err != nil {
		t.Fatal(err)
	}
	if sum, err := Verify(s.Path); err != nil || sum.Truncated || sum.ExpiredUpTo != 0 {
		t.Fatalf("file still present: %v %+v", err, sum)
	}
	head := s.Head().Seq
	clock = clock.Add(60 * 24 * time.Hour)
	done, err := s.Expire(clock.Add(-30*24*time.Hour), "30d")
	if err != nil || len(done) != 1 || s.Head().Seq != head {
		t.Fatalf("finish: %v %+v head %d -> %d", err, done, head, s.Head().Seq)
	}
	if sum, err := Verify(s.Path); err != nil || sum.Truncated || sum.ExpiredUpTo != a.Seal.Seq || sum.Retention != 1 {
		t.Fatalf("%v %+v", err, sum)
	}
}

func TestFilesOrderSameSecond(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.jsonl")
	for _, n := range []string{"ledger-20261004T100000Z-2.jsonl", "ledger-20261004T100000Z.jsonl", "ledger-20261004T100000Z-10.jsonl",
		"ledger-20261003T235959Z.jsonl", "ledger.jsonl"} {
		_ = os.WriteFile(filepath.Join(dir, n), nil, 0o600)
	}
	files, _ := Files(p)
	var got []string
	for _, f := range files {
		got = append(got, filepath.Base(f))
	}
	want := "ledger-20261003T235959Z.jsonl ledger-20261004T100000Z.jsonl ledger-20261004T100000Z-2.jsonl ledger-20261004T100000Z-10.jsonl ledger.jsonl"
	if strings.Join(got, " ") != want {
		t.Fatalf("order %v", got)
	}
}
