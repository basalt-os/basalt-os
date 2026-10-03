package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Record types written by a rotation.
const (
	// TypeSeal closes a file: the last record of every rotated file.
	TypeSeal = "seal"
	// TypeContinue opens the next file, chained to the seal.
	TypeContinue = "continue"
)

// SealData is the data of a seal record.
type SealData struct {
	File     string `json:"file"`      // name the sealed file is kept under (same directory)
	FirstSeq int64  `json:"first_seq"` // first record in the file
	Records  int64  `json:"records"`   // records in the file before the seal
	SHA256   string `json:"sha256"`    // of the file's bytes before the seal record
}

// ContinueData is the data of a continue record.
type ContinueData struct {
	From     string `json:"from"`      // the sealed file this one continues
	SealSeq  int64  `json:"seal_seq"`  // its seal record
	SealHash string `json:"seal_hash"` // and that record's hash (also this record's prev)
}

// RotateOptions control Rotate.
type RotateOptions struct {
	// MinSize: rotate only when the file is at least this large (0: always,
	// unless the file holds no record besides its continue record).
	MinSize int64
	// Attrs sets the file attributes: the sealed file becomes immutable
	// (chattr +i), the new one append-only (+a). Needs root
	// (CAP_LINUX_IMMUTABLE); tests leave it off.
	Attrs bool
	// Now overrides the clock for the sealed file's name (tests).
	Now func() time.Time
}

// RotateResult says what Rotate did.
type RotateResult struct {
	Rotated  bool
	Reason   string // why nothing was done, when Rotated is false
	Sealed   string // path of the sealed file
	Seal     Record
	Continue Record
}

// Rotate seals the log at l.Path and starts a new file chained to the seal.
//
// Under the log's exclusive lock (the one Append takes):
//  1. append a seal record with the SHA-256 of everything before it;
//  2. hard-link the file to its sealed name (<stem>-<UTC time>.jsonl);
//  3. write the new file, holding only the continue record, under a
//     temporary name and rename it over l.Path (atomic: l.Path always
//     exists, so no writer ever starts a fresh chain);
//  4. make the sealed file immutable and the new one append-only.
//
// A rotation interrupted after step 1 leaves a log that ends with a seal;
// Append then refuses to write (ErrSealed) and the next Rotate finishes the
// job with the same seal.
func (l *Log) Rotate(o RotateOptions) (RotateResult, error) {
	var res RotateResult
	f, err := os.OpenFile(l.Path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		if os.IsNotExist(err) {
			res.Reason = "no audit log yet"
			return res, nil
		}
		return res, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return res, err
	}
	if !isCurrent(f, l.Path) {
		return res, fmt.Errorf("%s was replaced while waiting for its lock (another rotation?)", l.Path)
	}
	st, err := f.Stat()
	if err != nil {
		return res, err
	}
	if st.Size() == 0 {
		res.Reason = "audit log is empty"
		return res, nil
	}
	last, err := lastRecord(f)
	if err != nil {
		return res, err
	}
	seal := last
	if last.Type != TypeSeal {
		first, err := firstRecord(f)
		if err != nil {
			return res, err
		}
		if first.Seq == last.Seq && first.Type == TypeContinue && o.MinSize > 0 {
			res.Reason = "nothing logged since the last rotation"
			return res, nil
		}
		if st.Size() < o.MinSize {
			res.Reason = fmt.Sprintf("%d bytes, below the rotation size %d", st.Size(), o.MinSize)
			return res, nil
		}
		sum, err := fileSHA256(f, st.Size())
		if err != nil {
			return res, err
		}
		name, err := sealedName(l.Path, o.Now)
		if err != nil {
			return res, err
		}
		data, _ := json.Marshal(SealData{File: name, FirstSeq: first.Seq, Records: last.Seq - first.Seq + 1, SHA256: sum})
		seal, err = l.appendLocked(f, TypeSeal, fmt.Sprintf("audit log sealed after record %d; continues in a new file, this one kept as %s", last.Seq, name), data)
		if err != nil {
			return res, err
		}
		if l.Journal {
			_ = sendJournal(seal)
		}
	}
	var sd SealData
	if err := json.Unmarshal(seal.Data, &sd); err != nil || sd.File == "" || strings.ContainsRune(sd.File, '/') {
		return res, fmt.Errorf("seal record %d: unreadable file name", seal.Seq)
	}
	dir := filepath.Dir(l.Path)
	sealed := filepath.Join(dir, sd.File)

	// An append-only file can be neither linked nor renamed.
	if o.Attrs {
		if err := setAttr(f, attrAppend, false); err != nil {
			return res, fmt.Errorf("clearing the append-only attribute: %w", err)
		}
	}
	if fi, err := os.Stat(sealed); err == nil {
		if !os.SameFile(fi, st) {
			return res, fmt.Errorf("%s exists and is not this log", sealed)
		}
	} else if err := os.Link(l.Path, sealed); err != nil {
		return res, err
	}

	// The new file: the continue record only, then renamed over the old name.
	data, _ := json.Marshal(ContinueData{From: sd.File, SealSeq: seal.Seq, SealHash: seal.Hash})
	cont := Record{Seq: seal.Seq + 1, Time: l.clock().UTC().Truncate(time.Microsecond), Type: TypeContinue,
		Actor: l.actor(), Text: "audit log continues from " + sd.File, Data: data, Prev: seal.Hash}
	cont.Hash = hashOf(cont)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.new-%d", filepath.Base(l.Path), os.Getpid()))
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
		werr = os.Rename(tmp, l.Path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return res, werr
	}
	syncDir(dir)
	if l.Journal {
		_ = sendJournal(cont)
	}
	if o.Attrs {
		if err := setAttr(f, attrImmutable, true); err != nil {
			return res, fmt.Errorf("making %s immutable: %w", sealed, err)
		}
		if err := setAttrPath(l.Path, attrAppend, true); err != nil {
			return res, fmt.Errorf("making %s append-only: %w", l.Path, err)
		}
	}
	return RotateResult{Rotated: true, Sealed: sealed, Seal: seal, Continue: cont}, nil
}

func (l *Log) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// sealedName picks <stem>-<UTC time>.jsonl next to path, unused so far.
func sealedName(path string, now func() time.Time) (string, error) {
	if now == nil {
		now = time.Now
	}
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	ts := now().UTC().Format("20060102T150405Z")
	for i := 1; i < 100; i++ {
		name := fmt.Sprintf("%s-%s.jsonl", stem, ts)
		if i > 1 {
			name = fmt.Sprintf("%s-%s-%d.jsonl", stem, ts, i)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(path), name)); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", errors.New("no free name for the sealed audit file")
}

// fileSHA256 hashes the first n bytes of f.
func fileSHA256(f *os.File, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// firstRecord reads the first line of f.
func firstRecord(f *os.File) (Record, error) {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for off := int64(0); ; {
		n, err := f.ReadAt(chunk, off)
		buf = append(buf, chunk[:n]...)
		if i := strings.IndexByte(string(buf), '\n'); i >= 0 {
			buf = buf[:i]
			break
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Record{}, err
		}
		off += int64(n)
	}
	var r Record
	if err := json.Unmarshal(buf, &r); err != nil {
		return Record{}, fmt.Errorf("audit log %s: first line unreadable: %w", f.Name(), err)
	}
	return r, nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Inode flags (linux/fs.h), set with the FS_IOC_SETFLAGS ioctl like chattr.
const (
	attrImmutable = 0x00000010 // FS_IMMUTABLE_FL
	attrAppend    = 0x00000020 // FS_APPEND_FL

	fsIocGetflags = 0x80086601
	fsIocSetflags = 0x40086602
)

func setAttrPath(path string, flag int32, on bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return setAttr(f, flag, on)
}

// setAttr turns one inode flag on or off; no change, no ioctl.
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
