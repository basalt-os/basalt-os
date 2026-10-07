// Package auditlog writes the install log: one JSON record per line, each
// carrying the SHA-256 of the previous record, so a copy of the log can be
// checked for edits or truncation in the middle (`basalt-installer log
// verify`). Secrets never reach it: callers pass redacted text only.
package auditlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Record is one line of the log.
type Record struct {
	Seq    int            `json:"seq"`
	Time   string         `json:"time"`
	Type   string         `json:"type"`
	Step   string         `json:"step,omitempty"`
	Text   string         `json:"text,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
	Prev   string         `json:"prev"`
	Hash   string         `json:"hash"`
}

// Genesis is the "previous hash" of the first record.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Log appends records to a file.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  int
	prev string
	now  func() time.Time
	// noSync skips the fsync after each record (SetSync).
	noSync bool
}

// Create starts a new log file (mode 0600). It fails if the file exists.
func Create(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, path: path, prev: Genesis, now: time.Now}, nil
}

// SetSync turns the fsync after each record on (the default) or off. An
// installation keeps it on, so the log survives a power cut; unit tests
// turn it off, so a busy disk cannot stretch them into timeouts.
func (l *Log) SetSync(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.noSync = !on
}

// Path returns the file the log is written to.
func (l *Log) Path() string { return l.path }

// Append writes one record.
func (l *Log) Append(typ, step, text string, fields map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	r := Record{Seq: l.seq, Time: l.now().UTC().Format(time.RFC3339Nano), Type: typ, Step: step, Text: text, Fields: fields, Prev: l.prev}
	r.Hash = hashOf(r)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	l.prev = r.Hash
	if l.noSync {
		return nil
	}
	return l.f.Sync()
}

// Close closes the file.
func (l *Log) Close() error { return l.f.Close() }

// hashOf is SHA-256 over the record's JSON with an empty Hash field.
func hashOf(r Record) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Verify reads a log and checks the chain. It returns the number of records.
func Verify(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	prev, n := Genesis, 0
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return n, fmt.Errorf("record %d: %w", n+1, err)
		}
		n++
		if rec.Seq != n {
			return n, fmt.Errorf("record %d: sequence %d (records missing or reordered)", n, rec.Seq)
		}
		if rec.Prev != prev {
			return n, fmt.Errorf("record %d: previous hash does not match (chain broken)", n)
		}
		if h := hashOf(rec); h != rec.Hash {
			return n, fmt.Errorf("record %d: hash does not match its content (edited)", n)
		}
		prev = rec.Hash
	}
	return n, sc.Err()
}
