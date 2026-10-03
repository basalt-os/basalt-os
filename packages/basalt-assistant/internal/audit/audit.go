// Package audit is the assistant's append-only, hash-chained record of
// decisions, proposals and changes. Each record carries the SHA-256 of the
// previous one, so any edit or removal breaks the chain (`basalt audit
// verify`). The file is root-owned, mode 0600, and the package sets it
// append-only (chattr +a) through tmpfiles.d; every record is also sent to
// the journal (SYSLOG_IDENTIFIER=basalt-assistant) with its hash, so a copy
// exists outside the file.
//
// The log is rotated with seals (rotate.go): the old file ends with a seal
// record that hashes its content, and the new file starts with a continue
// record chained to that seal, so the chain runs across files and `basalt
// audit verify` checks all of them.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultPath of the audit log (in /var/log, a separate subvolume, so it
// survives a rollback of the root).
const DefaultPath = "/var/log/basalt-assistant/audit.jsonl"

// zeroHash is the prev of the first record of a chain.
var zeroHash = strings.Repeat("0", 64)

// Record is one audit entry.
type Record struct {
	Seq   int64           `json:"seq"`
	Time  time.Time       `json:"time"`
	Type  string          `json:"type"` // decision, finding, proposal, confirm, apply, verify, ignore, refuse, seal, continue
	Actor Actor           `json:"actor"`
	Text  string          `json:"text"`
	Data  json.RawMessage `json:"data,omitempty"`
	Prev  string          `json:"prev"`
	Hash  string          `json:"hash,omitempty"`
}

// Actor is who caused the record.
type Actor struct {
	Program  string `json:"program"`
	UID      int    `json:"uid"`
	LoginUID int    `json:"loginuid"`
	User     string `json:"user,omitempty"`
	SudoUser string `json:"sudo_user,omitempty"`
}

// Log appends to the chain.
type Log struct {
	Path    string
	Program string
	Journal bool // also send to journald
	now     func() time.Time
}

// New returns a log at path for program.
func New(path, program string) *Log {
	return &Log{Path: path, Program: program, Journal: true, now: time.Now}
}

func (l *Log) actor() Actor {
	a := Actor{Program: l.Program, UID: os.Getuid(), LoginUID: -1, SudoUser: os.Getenv("SUDO_USER")}
	if b, err := os.ReadFile("/proc/self/loginuid"); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n != 4294967295 {
			a.LoginUID = int(n)
		}
	}
	if u, err := user.LookupId(strconv.Itoa(a.UID)); err == nil {
		a.User = u.Username
	}
	return a
}

// hashOf computes a record's hash: SHA-256 over its JSON without Hash.
func hashOf(r Record) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrSealed is returned by Append when the log ends with a seal record:
// a rotation started and did not finish (`basalt audit rotate` completes it).
var ErrSealed = errors.New("audit log is sealed; a rotation did not finish (run basalt audit rotate)")

// Append writes one record under an exclusive lock and returns it.
func (l *Log) Append(typ, text string, data any) (Record, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return Record{}, err
		}
		raw = b
	}
	// A rotation replaces the file at l.Path while holding the lock on the
	// old one. A writer that opened the old file and waited for the lock
	// finds that it no longer is the file at l.Path, and opens it again.
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(l.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return Record{}, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return Record{}, err
		}
		if !isCurrent(f, l.Path) {
			f.Close()
			if attempt >= 10 {
				return Record{}, fmt.Errorf("audit log %s keeps being replaced", l.Path)
			}
			continue
		}
		r, err := l.appendLocked(f, typ, text, raw)
		f.Close() // also releases the lock
		if err == nil && l.Journal {
			_ = sendJournal(r)
		}
		return r, err
	}
}

// appendLocked adds one record to f, which the caller holds locked.
func (l *Log) appendLocked(f *os.File, typ, text string, raw json.RawMessage) (Record, error) {
	last, err := lastRecord(f)
	if err != nil {
		return Record{}, err
	}
	if last.Type == TypeSeal && typ != TypeSeal {
		return Record{}, ErrSealed
	}
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	r := Record{Seq: last.Seq + 1, Time: now().UTC().Truncate(time.Microsecond), Type: typ,
		Actor: l.actor(), Text: text, Data: raw, Prev: last.Hash}
	if r.Prev == "" {
		r.Prev = zeroHash
	}
	r.Hash = hashOf(r)
	line, _ := json.Marshal(r)
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Record{}, err
	}
	return r, nil
}

// isCurrent reports whether the open file is still the one at path.
func isCurrent(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	return err == nil && os.SameFile(a, b)
}

// lastRecord reads the final line of the file.
func lastRecord(f *os.File) (Record, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return Record{}, err
	}
	size := st.Size()
	chunk := int64(1 << 16)
	for {
		off := max(size-chunk, 0)
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return Record{}, err
		}
		buf = bytes.TrimRight(buf, "\n")
		i := bytes.LastIndexByte(buf, '\n')
		if i >= 0 || off == 0 {
			var r Record
			if err := json.Unmarshal(buf[i+1:], &r); err != nil {
				return Record{}, fmt.Errorf("audit log %s: last line unreadable: %w", f.Name(), err)
			}
			return r, nil
		}
		chunk *= 4
	}
}

// JournalSocket is journald's native protocol socket.
var JournalSocket = "/run/systemd/journal/socket"

// sendJournal writes a record to the journal with the native protocol.
func sendJournal(r Record) error {
	prio := "6"
	switch r.Type {
	case "apply", "refuse", TypeSeal, TypeContinue:
		prio = "5"
	}
	fields := [][2]string{
		{"MESSAGE", fmt.Sprintf("basalt-assistant audit #%d %s: %s", r.Seq, r.Type, r.Text)},
		{"PRIORITY", prio},
		{"SYSLOG_IDENTIFIER", "basalt-assistant"},
		{"BASALT_AUDIT_SEQ", strconv.FormatInt(r.Seq, 10)},
		{"BASALT_AUDIT_TYPE", r.Type},
		{"BASALT_AUDIT_HASH", r.Hash},
		{"BASALT_AUDIT_PREV", r.Prev},
	}
	if len(r.Data) > 0 && len(r.Data) < 48*1024 {
		fields = append(fields, [2]string{"BASALT_AUDIT_DATA", string(r.Data)})
	}
	var buf bytes.Buffer
	for _, kv := range fields {
		if strings.ContainsRune(kv[1], '\n') {
			buf.WriteString(kv[0])
			buf.WriteByte('\n')
			_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kv[1])))
			buf.WriteString(kv[1])
			buf.WriteByte('\n')
		} else {
			buf.WriteString(kv[0] + "=" + kv[1] + "\n")
		}
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: JournalSocket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write(buf.Bytes())
	return err
}
