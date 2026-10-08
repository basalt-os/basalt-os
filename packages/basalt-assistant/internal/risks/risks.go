// Package risks keeps the security risks an administrator accepted on
// this computer (Security and Activity: "I accept this risk"). The file is
// system-wide, written only by the assistant's executor after the approval
// gate allowed a risk.accept or risk.review, and read by everyone (the
// status service shows the accepted state to every person).
//
// Accepting a risk never hides a problem: the desktop shows an accepted
// item in its own state only while it is a warning or a note, never when
// it becomes a problem.
package risks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// DefaultPath is the system-wide file.
const DefaultPath = "/etc/basalt/security/accepted-risks.json"

// Path is the file (tests point it elsewhere).
var Path = DefaultPath

// Items are the security items whose risk may be accepted. SELinux, the
// updates and the snapshots are not among them: Basalt OS never treats
// those as a choice to live with.
var Items = []string{"encryption", "secure_boot", "tpm", "audit", "ledger"}

// ValidItem reports an item that may be accepted.
func ValidItem(item string) bool {
	for _, i := range Items {
		if i == item {
			return true
		}
	}
	return false
}

// reUser is a local account name.
var reUser = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}\$?$`)

// ValidUser reports a plausible account name.
func ValidUser(u string) bool { return reUser.MatchString(u) }

// Accepted is one accepted risk.
type Accepted struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// File is the document on disk.
type File struct {
	Version int                 `json:"version"`
	Risks   map[string]Accepted `json:"risks"`
}

// Load reads the file; a missing file is an empty list.
func Load() (File, error) {
	f := File{Version: 1, Risks: map[string]Accepted{}}
	b, err := os.ReadFile(Path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %v", Path, err)
	}
	if f.Risks == nil {
		f.Risks = map[string]Accepted{}
	}
	return f, nil
}

// List returns the accepted items in a stable order.
func (f File) List() []string {
	out := make([]string, 0, len(f.Risks))
	for k := range f.Risks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func save(f File) error {
	if err := os.MkdirAll(filepath.Dir(Path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, Path)
}

// Accept records that `by` accepted the risk of `item` now.
func Accept(item, by string, now time.Time) error {
	if !ValidItem(item) {
		return fmt.Errorf("%q is not a risk that can be accepted", item)
	}
	if !ValidUser(by) {
		return fmt.Errorf("%q is not an account name", by)
	}
	f, err := Load()
	if err != nil {
		return err
	}
	f.Version = 1
	f.Risks[item] = Accepted{By: by, At: now.UTC().Truncate(time.Second)}
	return save(f)
}

// Clear removes an accepted risk ("Review again"); clearing one that is
// not accepted is not an error.
func Clear(item string) error {
	if !ValidItem(item) {
		return fmt.Errorf("%q is not a risk that can be accepted", item)
	}
	f, err := Load()
	if err != nil {
		return err
	}
	if _, ok := f.Risks[item]; !ok {
		return nil
	}
	delete(f.Risks, item)
	return save(f)
}
