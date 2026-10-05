// Package audit writes basalt-agent's session audit log: JSON lines, one
// record per event, each chained to the previous one by SHA-256, so an
// edit or a removed line breaks the chain (basalt-agent audit --verify).
//
// The record schema (version 1) is meant to be accepted unchanged by a
// future system audit service; every producer fills the same envelope:
//
//	v         schema version (1)
//	seq       position in this log, from 1
//	time      RFC 3339 UTC, nanoseconds
//	producer  "basalt-agent"
//	uid       user the session runs for
//	session   session id ("s-" and 12 hex digits), empty for log-wide events
//	event     dotted name, see the Event constants
//	outcome   ok, allowed, denied or error
//	subject   who acted: profile, mode, SELinux level, project
//	data      event-specific fields (flat, JSON scalars or string lists)
//	prev      hash of the previous record ("" for the first)
//	hash      SHA-256 (hex) of the record serialized with hash = ""
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Version of the record schema.
const Version = 1

// Events written by basalt-agent.
const (
	SessionStart = "session.start" // data: command, image, egress (list), secrets (names only), credentials (name, host, port, header), secrets_withheld, relabeled (count)
	SessionEnd   = "session.end"   // data: exit_code, duration_s, egress_allowed, egress_denied, credential_uses (count by name), credential_stripped
	EgressAllow  = "egress.allow"  // data: host, port (first connection per host:port in a session)
	EgressDeny   = "egress.deny"   // data: host, port, reason
	GrantRequest = "grant.request" // data: kind (host, path), value, by_uid
	GrantApply   = "grant.apply"   // outcome ok or error; data: kind, value
	Relabel      = "relabel"       // data: path, type, level, count
	Install      = "install"       // data: method, package, target
	// The session proxy used a key for the agent (first request per key
	// and host in a session); data: credential (the name, never the value), host, port.
	CredentialUse = "credential.use"
	// Credential headers were removed from a request to a host they do
	// not belong to; data: host, port, headers (names), reason.
	CredentialStrip = "credential.strip"
)

// Subject identifies the confined session an event belongs to.
type Subject struct {
	Profile string `json:"profile,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Level   string `json:"level,omitempty"`
	Project string `json:"project,omitempty"`
}

// Record is one audit line.
type Record struct {
	V        int            `json:"v"`
	Seq      int64          `json:"seq"`
	Time     string         `json:"time"`
	Producer string         `json:"producer"`
	UID      int            `json:"uid"`
	Session  string         `json:"session,omitempty"`
	Event    string         `json:"event"`
	Outcome  string         `json:"outcome"`
	Subject  Subject        `json:"subject"`
	Data     map[string]any `json:"data,omitempty"`
	Prev     string         `json:"prev"`
	Hash     string         `json:"hash"`
}

func (r Record) digest() (string, error) {
	r.Hash = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Log is an append-only audit file.
type Log struct {
	Path string
	now  func() time.Time
}

// Open returns the log at path; the file is created on the first Append.
func Open(path string) *Log { return &Log{Path: path, now: time.Now} }

// Append fills the envelope of r (version, sequence, time, producer, chain)
// and appends it under an exclusive lock.
func (l *Log) Append(r Record) (Record, error) {
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return r, err
	}
	f, err := os.OpenFile(l.Path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return r, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return r, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	last, err := lastRecord(f)
	if err != nil {
		return r, err
	}
	r.V, r.Producer = Version, "basalt-agent"
	r.Seq, r.Prev = last.Seq+1, last.Hash
	r.Time = l.now().UTC().Format(time.RFC3339Nano)
	if r.Outcome == "" {
		r.Outcome = "ok"
	}
	if r.Hash, err = r.digest(); err != nil {
		return r, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	_, err = f.Write(append(b, '\n'))
	return r, err
}

// lastRecord reads the final line of f (zero Record for an empty file).
func lastRecord(f *os.File) (Record, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return Record{}, err
	}
	const tail = 64 << 10
	off := st.Size() - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return Record{}, err
	}
	buf = bytes.TrimRight(buf, "\n")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	var r Record
	if err := json.Unmarshal(buf, &r); err != nil {
		return Record{}, fmt.Errorf("audit log %s: last record unreadable: %w", f.Name(), err)
	}
	return r, nil
}

// Read returns the records of the log (all, or those of one session).
func Read(path, session string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return out, fmt.Errorf("unreadable record after seq %d: %w", len(out), err)
		}
		if session == "" || r.Session == session {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Verify checks the sequence and the hash chain of the whole log and
// returns the number of records.
func Verify(path string) (int, error) {
	rs, err := Read(path, "")
	if err != nil {
		return 0, err
	}
	prev := ""
	for i, r := range rs {
		if r.Seq != int64(i+1) {
			return i, fmt.Errorf("record %d: sequence %d, want %d", i+1, r.Seq, i+1)
		}
		if r.Prev != prev {
			return i, fmt.Errorf("record %d: chain broken (prev does not match)", r.Seq)
		}
		d, err := r.digest()
		if err != nil {
			return i, err
		}
		if d != r.Hash {
			return i, fmt.Errorf("record %d: hash mismatch (record changed)", r.Seq)
		}
		prev = r.Hash
	}
	return len(rs), nil
}
