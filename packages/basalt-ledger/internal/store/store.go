// Package store is basalt-ledger's append-only, hash-chained storage, the
// same design as the system assistant's audit log: JSON lines, each record
// carrying the SHA-256 of the previous one; rotation seals a file (a final
// seal record holding the SHA-256 of everything before it), keeps it under
// a dated name, and starts the next file with a continue record chained to
// the seal, so one chain runs across all files and any edit, removal or
// reordering anywhere is detected.
//
// Only the ledger daemon writes (one writer, serialized by a mutex). With
// Attrs on (the service), the current file is append-only (chattr +a) and
// sealed files immutable (+i), so not even root rewrites them by accident;
// a determined root can clear the attributes, which the chain then
// reveals (tamper evidence, not tamper proofing; see docs/ledger.md).
package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
)

// SealData is the data of a seal record.
type SealData struct {
	File     string `json:"file"`
	FirstSeq int64  `json:"first_seq"`
	Records  int64  `json:"records"`
	SHA256   string `json:"sha256"`
}

// ContinueData is the data of a continue record.
type ContinueData struct {
	From     string `json:"from"`
	SealSeq  int64  `json:"seal_seq"`
	SealHash string `json:"seal_hash"`
}

// Store is the ledger's chain on disk.
type Store struct {
	Path  string // current file, e.g. /var/log/basalt-ledger/ledger.jsonl
	Attrs bool   // set append-only / immutable attributes (needs CAP_LINUX_IMMUTABLE)
	Now   func() time.Time

	mu    sync.Mutex
	f     *os.File
	last  record.Record
	first int64 // first seq of the current file
}

// Open opens (or creates) the current file and reads the chain head. A
// rotation interrupted after its seal record is finished here.
func Open(path string, attrs bool) (*Store, error) {
	s := &Store{Path: path, Attrs: attrs, Now: time.Now}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := s.open(); err != nil {
		return nil, err
	}
	if s.last.Event == record.EventSeal {
		if _, err := s.finishRotation(s.last); err != nil {
			return nil, fmt.Errorf("finishing an interrupted rotation: %w", err)
		}
	}
	return s, nil
}

func (s *Store) open() error {
	f, err := os.OpenFile(s.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	last, err := lastRecord(f)
	if err != nil {
		f.Close()
		return err
	}
	first, err := firstRecord(f)
	if err != nil {
		f.Close()
		return err
	}
	if s.Attrs {
		if err := setAttr(f, attrAppend, true); err != nil {
			f.Close()
			return fmt.Errorf("append-only attribute on %s: %w", s.Path, err)
		}
	}
	s.f, s.last, s.first = f, last, first.Seq
	return nil
}

// Close closes the current file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// Head returns the last record written.
func (s *Store) Head() record.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Append chains r after the head and writes it (fsync'ed). The caller
// fills everything but Seq, Prev and Hash.
func (s *Store) Append(r record.Record) (record.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(r)
}

func (s *Store) appendLocked(r record.Record) (record.Record, error) {
	if s.f == nil {
		return r, errors.New("ledger store is closed")
	}
	if s.last.Event == record.EventSeal && r.Event != record.EventContinue {
		return r, errors.New("ledger file is sealed; rotation did not finish")
	}
	r.V = record.Version
	r.Seq = s.last.Seq + 1
	r.Prev = s.last.Hash
	if r.Prev == "" {
		r.Prev = record.ZeroHash
	}
	if r.Received == "" {
		r.Received = s.Now().UTC().Format(time.RFC3339Nano)
	}
	r.Hash = record.HashOf(r)
	line, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return r, err
	}
	if err := s.f.Sync(); err != nil {
		return r, err
	}
	if s.first == 0 {
		s.first = r.Seq
	}
	s.last = r
	return r, nil
}

// Size of the current file.
func (s *Store) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return 0
	}
	st, err := s.f.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}

// RotateResult says what Rotate did.
type RotateResult struct {
	Rotated  bool          `json:"rotated"`
	Reason   string        `json:"reason,omitempty"`
	Sealed   string        `json:"sealed,omitempty"`
	Seal     record.Record `json:"seal"`
	Continue record.Record `json:"continue"`
}

// Rotate seals the current file and continues in a new one: (1) a seal
// record with the SHA-256 of everything before it; (2) a hard link of the
// file under its sealed name; (3) the new file, holding only the continue
// record, renamed over the current name (atomic); (4) attributes.
func (s *Store) Rotate() (RotateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var res RotateResult
	if s.last.Seq == 0 {
		res.Reason = "the ledger is empty"
		return res, nil
	}
	if s.last.Event == record.EventContinue && s.first == s.last.Seq {
		res.Reason = "nothing recorded since the last rotation"
		return res, nil
	}
	st, err := s.f.Stat()
	if err != nil {
		return res, err
	}
	sum, err := fileSHA256(s.f, st.Size())
	if err != nil {
		return res, err
	}
	name, err := s.sealedName()
	if err != nil {
		return res, err
	}
	data, _ := json.Marshal(SealData{File: name, FirstSeq: s.first, Records: s.last.Seq - s.first + 1, SHA256: sum})
	seal, err := s.appendLocked(record.Record{Time: s.Now().UTC().Format(time.RFC3339Nano), Producer: record.Producer,
		Event: record.EventSeal, Outcome: "ok", Severity: record.Severity(record.EventSeal, "ok"), Data: data})
	if err != nil {
		return res, err
	}
	return s.finishRotation(seal)
}

// finishRotation does steps 2 to 4 for the seal record at the end of the
// current file. Called with s.mu held (or from Open).
func (s *Store) finishRotation(seal record.Record) (RotateResult, error) {
	var res RotateResult
	var sd SealData
	if err := json.Unmarshal(seal.Data, &sd); err != nil || sd.File == "" || strings.ContainsRune(sd.File, '/') {
		return res, fmt.Errorf("seal record %d: unreadable file name", seal.Seq)
	}
	dir := filepath.Dir(s.Path)
	sealed := filepath.Join(dir, sd.File)
	if s.Attrs {
		if err := setAttr(s.f, attrAppend, false); err != nil {
			return res, fmt.Errorf("clearing the append-only attribute: %w", err)
		}
	}
	st, err := s.f.Stat()
	if err != nil {
		return res, err
	}
	if fi, err := os.Stat(sealed); err == nil {
		if !os.SameFile(fi, st) {
			return res, fmt.Errorf("%s exists and is not this file", sealed)
		}
	} else if err := os.Link(s.Path, sealed); err != nil {
		return res, err
	}
	cd, _ := json.Marshal(ContinueData{From: sd.File, SealSeq: seal.Seq, SealHash: seal.Hash})
	cont := record.Record{V: record.Version, Seq: seal.Seq + 1, Time: s.Now().UTC().Format(time.RFC3339Nano),
		Producer: record.Producer, Event: record.EventContinue, Outcome: "ok",
		Severity: record.Severity(record.EventContinue, "ok"), Data: cd, Prev: seal.Hash}
	cont.Received = cont.Time
	cont.Hash = record.HashOf(cont)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.new-%d", filepath.Base(s.Path), os.Getpid()))
	nf, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return res, err
	}
	line, _ := json.Marshal(cont)
	_, werr := nf.Write(append(line, '\n'))
	if werr == nil {
		werr = nf.Sync()
	}
	if cerr := nf.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, s.Path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return res, werr
	}
	syncDir(dir)
	old := s.f
	if s.Attrs {
		if err := setAttr(old, attrImmutable, true); err != nil {
			return res, fmt.Errorf("making %s immutable: %w", sealed, err)
		}
	}
	old.Close()
	s.f = nil
	if err := s.open(); err != nil {
		return res, err
	}
	return RotateResult{Rotated: true, Sealed: sealed, Seal: seal, Continue: cont}, nil
}

func (s *Store) sealedName() (string, error) {
	base := filepath.Base(s.Path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	ts := s.Now().UTC().Format("20060102T150405Z")
	for i := 1; i < 100; i++ {
		name := fmt.Sprintf("%s-%s.jsonl", stem, ts)
		if i > 1 {
			name = fmt.Sprintf("%s-%s-%d.jsonl", stem, ts, i)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(s.Path), name)); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", errors.New("no free name for the sealed file")
}

// Files returns the chain's files in order: sealed files oldest first,
// then the current file.
func Files(path string) ([]string, error) {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	sealed, err := filepath.Glob(filepath.Join(filepath.Dir(path), stem+"-*.jsonl"))
	if err != nil {
		return nil, err
	}
	// Order by seal time, then by the counter of files sealed in the same
	// second (name-TS.jsonl, name-TS-2.jsonl, ...).
	key := func(p string) string {
		base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		rest := strings.TrimPrefix(base, stem+"-")
		ts, n, _ := strings.Cut(rest, "-")
		if n == "" {
			n = "1"
		}
		return fmt.Sprintf("%s-%06s", ts, n)
	}
	sort.Slice(sealed, func(i, j int) bool { return key(sealed[i]) < key(sealed[j]) })
	out := make([]string, 0, len(sealed)+1)
	for _, p := range sealed {
		if p != path {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

// Summary describes a verified chain.
type Summary struct {
	Files     []string `json:"files"`
	FirstSeq  int64    `json:"first_seq"`
	LastSeq   int64    `json:"last_seq"`
	LastHash  string   `json:"last_hash"`
	Records   int64    `json:"records"`
	Seals     int      `json:"seals"`
	Truncated bool     `json:"truncated"`
}

// Verify checks every file: sequence, prev links and hashes inside each
// file; seal records against the file content; continue records against
// the previous seal.
func Verify(path string) (Summary, error) {
	var s Summary
	files, err := Files(path)
	if err != nil {
		return s, err
	}
	s.Files = files
	var prev *record.Record
	for i, file := range files {
		isLast := i == len(files)-1
		recs, offsets, data, err := readFile(file)
		if err != nil {
			return s, err
		}
		name := filepath.Base(file)
		if len(recs) == 0 {
			if prev != nil || !isLast {
				return s, fmt.Errorf("%s: empty", name)
			}
			continue
		}
		first := recs[0]
		switch {
		case prev != nil:
			if first.Event != record.EventContinue {
				return s, fmt.Errorf("%s: does not start with a continue record after the previous seal", name)
			}
			var cd ContinueData
			_ = json.Unmarshal(first.Data, &cd)
			if first.Prev != prev.Hash || first.Seq != prev.Seq+1 || cd.SealHash != prev.Hash || cd.SealSeq != prev.Seq {
				return s, fmt.Errorf("%s: continue record %d is not chained to seal %d", name, first.Seq, prev.Seq)
			}
			if cd.From != filepath.Base(files[i-1]) {
				return s, fmt.Errorf("%s: continues from %q, but the previous file is %s", name, cd.From, filepath.Base(files[i-1]))
			}
		case first.Event == record.EventContinue:
			s.Truncated = true
		default:
			if first.Seq != 1 || first.Prev != record.ZeroHash {
				return s, fmt.Errorf("%s: the chain does not start at record 1", name)
			}
		}
		if s.FirstSeq == 0 {
			s.FirstSeq = first.Seq
		}
		for j, r := range recs {
			if j > 0 {
				p := recs[j-1]
				if r.Seq != p.Seq+1 {
					return s, fmt.Errorf("%s: record %d follows record %d (records missing or reordered)", name, r.Seq, p.Seq)
				}
				if r.Prev != p.Hash {
					return s, fmt.Errorf("%s: record %d: prev does not match the previous record", name, r.Seq)
				}
			}
			if h := record.HashOf(r); h != r.Hash {
				return s, fmt.Errorf("%s: record %d: content does not match its hash (edited)", name, r.Seq)
			}
			if r.Event == record.EventSeal {
				if j != len(recs)-1 {
					return s, fmt.Errorf("%s: seal record %d is not the last record of its file", name, r.Seq)
				}
				var sd SealData
				if err := json.Unmarshal(r.Data, &sd); err != nil {
					return s, fmt.Errorf("%s: seal record %d: %v", name, r.Seq, err)
				}
				sum := sha256.Sum256(data[:offsets[j]])
				if hex.EncodeToString(sum[:]) != sd.SHA256 {
					return s, fmt.Errorf("%s: content before seal %d does not match its SHA-256", name, r.Seq)
				}
				if sd.FirstSeq != first.Seq || sd.Records != r.Seq-first.Seq {
					return s, fmt.Errorf("%s: seal %d counts %d+%d records, the file holds %d+%d", name, r.Seq, sd.FirstSeq, sd.Records, first.Seq, r.Seq-first.Seq)
				}
				if !isLast && sd.File != name {
					return s, fmt.Errorf("%s: sealed as %q", name, sd.File)
				}
				s.Seals++
			}
			s.Records++
			s.LastSeq, s.LastHash = r.Seq, r.Hash
		}
		last := recs[len(recs)-1]
		if !isLast && last.Event != record.EventSeal {
			return s, fmt.Errorf("%s: a rotated file that does not end with a seal record", name)
		}
		prev = &last
	}
	return s, nil
}

// Scan calls fn for every record in chain order, from the files that may
// hold records at or after since (zero: all). fn returns false to stop.
func Scan(path string, since time.Time, fn func(record.Record) bool) error {
	files, err := Files(path)
	if err != nil {
		return err
	}
	for i, file := range files {
		if !since.IsZero() && i < len(files)-1 {
			// A sealed file's name carries its seal time: skip files sealed
			// before since.
			if t, ok := sealTime(file); ok && t.Before(since) {
				continue
			}
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var r record.Record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			if !fn(r) {
				f.Close()
				return nil
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return err
		}
	}
	return nil
}

func sealTime(file string) (time.Time, bool) {
	base := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	i := strings.LastIndexByte(base, '-')
	if i < 0 {
		return time.Time{}, false
	}
	ts := base[i+1:]
	if len(ts) != len("20060102T150405Z") {
		// name-TS-N: the counter follows the time
		j := strings.LastIndexByte(base[:i], '-')
		if j < 0 {
			return time.Time{}, false
		}
		ts = base[j+1 : i]
	}
	t, err := time.Parse("20060102T150405Z", ts)
	return t, err == nil
}

func readFile(path string) ([]record.Record, []int, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	var recs []record.Record
	var offs []int
	off := 0
	for off < len(data) {
		end := bytes.IndexByte(data[off:], '\n')
		line := data[off:]
		next := len(data)
		if end >= 0 {
			line, next = data[off:off+end], off+end+1
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var r record.Record
			if err := json.Unmarshal(line, &r); err != nil {
				after := int64(0)
				if len(recs) > 0 {
					after = recs[len(recs)-1].Seq
				}
				return nil, nil, nil, fmt.Errorf("%s: record after %d is not JSON: %w", filepath.Base(path), after, err)
			}
			recs = append(recs, r)
			offs = append(offs, off)
		}
		off = next
	}
	return recs, offs, data, nil
}

func lastRecord(f *os.File) (record.Record, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return record.Record{}, err
	}
	size := st.Size()
	chunk := int64(1 << 16)
	for {
		off := max(size-chunk, 0)
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return record.Record{}, err
		}
		buf = bytes.TrimRight(buf, "\n")
		i := bytes.LastIndexByte(buf, '\n')
		if i >= 0 || off == 0 {
			var r record.Record
			if err := json.Unmarshal(buf[i+1:], &r); err != nil {
				return record.Record{}, fmt.Errorf("%s: last line unreadable: %w", f.Name(), err)
			}
			return r, nil
		}
		chunk *= 4
	}
}

func firstRecord(f *os.File) (record.Record, error) {
	br := bufio.NewReader(io.NewSectionReader(f, 0, 1<<24))
	line, err := br.ReadBytes('\n')
	if len(bytes.TrimSpace(line)) == 0 {
		if err == io.EOF || err == nil {
			return record.Record{}, nil
		}
		return record.Record{}, err
	}
	var r record.Record
	if err := json.Unmarshal(line, &r); err != nil {
		return record.Record{}, fmt.Errorf("%s: first line unreadable: %w", f.Name(), err)
	}
	return r, nil
}

func fileSHA256(f *os.File, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Inode flags (linux/fs.h), set with FS_IOC_SETFLAGS like chattr.
const (
	attrImmutable = 0x00000010
	attrAppend    = 0x00000020
	fsIocGetflags = 0x80086601
	fsIocSetflags = 0x40086602
)

func setAttr(f *os.File, flag int32, on bool) error {
	var flags int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetflags, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return e
	}
	want := flags &^ flag
	if on {
		want = flags | flag
	}
	if want == flags {
		return nil
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocSetflags, uintptr(unsafe.Pointer(&want))); e != 0 {
		return e
	}
	return nil
}
