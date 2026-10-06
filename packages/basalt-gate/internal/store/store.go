// Package store keeps the gate's state in /var/lib/basalt-gate (type
// basalt_gate_var_lib_t, written only by basalt_gate_t): the rule set
// (system rules and each user's), sealed with a key only the gate holds;
// the emergency stop; the rules paused by their circuit breaker; the
// counts limits need; and the queue of requests, so a restart keeps what
// waits for a person.
//
// The seal is an HMAC-SHA256 over the canonical rule set with a software
// key in the state directory; a TPM-bound key (the ledger's pattern) is a
// later step. A rule file that does not match its seal is not trusted:
// the gate loads only the careful preset and records a critical event.
package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/canon"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
)

// DefaultDir is the state directory.
const DefaultDir = "/var/lib/basalt-gate"

// ErrSeal is returned when the rule set does not match its seal.
var ErrSeal = errors.New("the rule set does not match its seal")

// Store is the state directory.
type Store struct {
	Dir string
	key []byte
	mu  sync.Mutex
}

// Open opens (and creates) the state directory and its seal key.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir}
	kf := filepath.Join(dir, "seal.key")
	b, err := os.ReadFile(kf)
	switch {
	case err == nil && len(b) == 32:
		s.key = b
	case err == nil:
		return nil, fmt.Errorf("%s: not a 32-byte key", kf)
	case os.IsNotExist(err):
		s.key = make([]byte, 32)
		if _, err := rand.Read(s.key); err != nil {
			return nil, err
		}
		if err := writeAtomic(kf, s.key); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return s, nil
}

// rulesFile is what rules.json holds.
type rulesFile struct {
	V      int           `json:"v"`
	Rules  []policy.Rule `json:"rules"`
	Saved  string        `json:"saved"`
	Seal   string        `json:"seal"`
	Preset string        `json:"preset,omitempty"` // the preset the person chose
}

func (s *Store) seal(rules []policy.Rule, preset, saved string) (string, error) {
	b, err := canon.Marshal(rulesFile{V: 1, Rules: rules, Saved: saved, Preset: preset})
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, s.key)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// LoadRules returns the stored rule set and the preset name. ok is false
// when there is no rule set yet. A seal mismatch returns ErrSeal.
func (s *Store) LoadRules() (rules []policy.Rule, preset string, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.Dir, "rules.json"))
	if os.IsNotExist(err) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	var f rulesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, "", true, fmt.Errorf("%w: %v", ErrSeal, err)
	}
	want, err := s.seal(f.Rules, f.Preset, f.Saved)
	if err != nil {
		return nil, "", true, err
	}
	if !hmac.Equal([]byte(want), []byte(f.Seal)) {
		return nil, "", true, ErrSeal
	}
	return f.Rules, f.Preset, true, nil
}

// SaveRules writes and seals the rule set.
func (s *Store) SaveRules(rules []policy.Rule, preset string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rules == nil {
		rules = []policy.Rule{}
	}
	saved := time.Now().UTC().Format(time.RFC3339)
	seal, err := s.seal(rules, preset, saved)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(rulesFile{V: 1, Rules: rules, Saved: saved, Seal: seal, Preset: preset}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.Dir, "rules.json"), b)
}

// QuarantineRules moves an untrusted rule file aside (rules.json.bad-TIME)
// so it can be inspected; nothing in it is used.
func (s *Store) QuarantineRules() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := filepath.Join(s.Dir, "rules.json.bad-"+time.Now().UTC().Format("20060102T150405Z"))
	return dst, os.Rename(filepath.Join(s.Dir, "rules.json"), dst)
}

// Stop is the emergency stop state.
type Stop struct {
	On   bool   `json:"on"`
	By   string `json:"by,omitempty"`
	From string `json:"from,omitempty"`
	At   string `json:"at,omitempty"`
	// NeedsAuth: resuming needs administrator authentication (the stop
	// came from the phone or froze agent sessions).
	NeedsAuth bool `json:"needs_auth,omitempty"`
}

// Use is one allowed request, for limits.
type Use struct {
	Rule  string    `json:"rule"`
	Chain string    `json:"chain"`
	Time  time.Time `json:"time"`
}

// State is the rest of the gate's state.
type State struct {
	Stop   Stop              `json:"stop"`
	Paused map[string]string `json:"paused,omitempty"` // rule key -> why
	Uses   []Use             `json:"uses,omitempty"`
}

// LoadState reads state.json (empty state when missing).
func (s *Store) LoadState() (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(s.Dir, "state.json"))
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// SaveState writes state.json.
func (s *Store) SaveState(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.Dir, "state.json"), b)
}

// LoadQueue reads queue.json into v.
func (s *Store) LoadQueue(v any) error {
	b, err := os.ReadFile(filepath.Join(s.Dir, "queue.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// SaveQueue writes v as queue.json.
func (s *Store) SaveQueue(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.Dir, "queue.json"), b)
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
