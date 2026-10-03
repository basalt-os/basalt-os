// Package proposal stores findings and their proposed changes in the
// assistant's state directory. A proposal is a diagnosis (report and
// evidence) plus zero or more typed actions; one without actions is a
// report for a person to read. Nothing here executes anything.
package proposal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
)

// Statuses.
const (
	Pending  = "pending"
	Applied  = "applied"
	Failed   = "failed"
	Ignored  = "ignored"
	Resolved = "resolved" // the problem went away without an apply
)

// DefaultDir holds proposal files.
const DefaultDir = "/var/lib/basalt-assistant/proposals"

// Proposal is one finding with its proposed change.
type Proposal struct {
	ID          string            `json:"id"`
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
	Source      string            `json:"source"` // daemon, cli, mcp
	Kind        string            `json:"kind"`   // unit, selinux, disk, dnf, snapshot
	Subject     string            `json:"subject"`
	Key         string            `json:"key"` // dedup key
	Title       string            `json:"title"`
	Report      string            `json:"report"`
	Evidence    []string          `json:"evidence,omitempty"`
	Actions     []action.Action   `json:"actions,omitempty"`
	Decisions   []decide.Decision `json:"decisions,omitempty"`
	NeedsReview bool              `json:"needs_review"`
	Severity    float64           `json:"severity"`
	Status      string            `json:"status"`
	Seen        int               `json:"seen"`
	LastSeen    time.Time         `json:"last_seen"`
	Result      *Result           `json:"result,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// Result of an apply.
type Result struct {
	Time         time.Time `json:"time"`
	OK           bool      `json:"ok"`
	Fingerprint  string    `json:"fingerprint"`
	PreSnapshot  int       `json:"pre_snapshot,omitempty"`
	PostSnapshot int       `json:"post_snapshot,omitempty"`
	Steps        []Step    `json:"steps"`
	Checks       []Step    `json:"checks"`
	AuditSeq     int64     `json:"audit_seq,omitempty"`
}

// Step is one command or check outcome.
type Step struct {
	What   string `json:"what"`
	OK     bool   `json:"ok"`
	Code   int    `json:"code,omitempty"`
	Output string `json:"output,omitempty"`
}

var reID = regexp.MustCompile(`^[a-z0-9-]{4,40}$`)

// NewID returns a short random id, e.g. "p-4f2a9c".
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "p-" + hex.EncodeToString(b)
}

// Commands lists every command of the proposal, rebuilt from its actions.
func (p *Proposal) Commands() ([]string, error) {
	var out []string
	for _, a := range p.Actions {
		cmds, err := a.Commands()
		if err != nil {
			return nil, err
		}
		for _, c := range cmds {
			out = append(out, c.String())
		}
	}
	return out, nil
}

// Fingerprint binds a confirmation to the exact commands: the first 8 hex
// digits of the SHA-256 of the proposal id and its command lines. A person
// confirming non-interactively types it (`--confirm`), so a changed
// proposal cannot be applied with an old confirmation.
func (p *Proposal) Fingerprint() (string, error) {
	cmds, err := p.Commands()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(p.ID + "\n" + strings.Join(cmds, "\n")))
	return hex.EncodeToString(sum[:])[:8], nil
}

// Store is a directory of proposal files.
type Store struct{ Dir string }

func (s Store) path(id string) (string, error) {
	if !reID.MatchString(id) {
		return "", fmt.Errorf("invalid proposal id %q", id)
	}
	return filepath.Join(s.Dir, id+".json"), nil
}

// Save writes atomically.
func (s Store) Save(p *Proposal) error {
	fn, err := s.path(p.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	p.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := fn + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fn)
}

// Load reads one proposal.
func (s Store) Load(id string) (*Proposal, error) {
	fn, err := s.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(fn)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no proposal %s", id)
		}
		return nil, err
	}
	var p Proposal
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("proposal %s: %w", id, err)
	}
	return &p, nil
}

// List returns proposals, newest first; status "" means all.
func (s Store) List(status string) ([]*Proposal, error) {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Proposal
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p, err := s.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if status == "" || p.Status == status {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// FindOpen returns the pending proposal with this dedup key, if any.
func (s Store) FindOpen(key string) *Proposal {
	ps, _ := s.List(Pending)
	for _, p := range ps {
		if p.Key == key {
			return p
		}
	}
	return nil
}
