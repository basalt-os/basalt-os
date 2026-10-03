package diag

import (
	"context"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// Snapshot is one snapper snapshot of the root.
type Snapshot struct {
	Number      int               `json:"number"`
	Type        string            `json:"type"` // single, pre, post
	Pre         int               `json:"pre,omitempty"`
	Date        string            `json:"date"`
	Time        time.Time         `json:"time"`
	Description string            `json:"description"`
	Cleanup     string            `json:"cleanup,omitempty"`
	Userdata    map[string]string `json:"userdata,omitempty"`
}

type infoXML struct {
	Type        string `xml:"type"`
	Num         int    `xml:"num"`
	Date        string `xml:"date"`
	PreNum      int    `xml:"pre_num"`
	Description string `xml:"description"`
	Cleanup     string `xml:"cleanup"`
	Userdata    []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"userdata"`
}

// ParseInfoXML reads a snapper info.xml.
func ParseInfoXML(b []byte) (Snapshot, error) {
	var x infoXML
	if err := xml.Unmarshal(b, &x); err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{Number: x.Num, Type: x.Type, Pre: x.PreNum, Date: x.Date, Description: x.Description, Cleanup: x.Cleanup}
	if t, err := time.Parse("2006-01-02 15:04:05", x.Date); err == nil {
		s.Time = t.UTC()
	}
	if len(x.Userdata) > 0 {
		s.Userdata = map[string]string{}
		for _, u := range x.Userdata {
			s.Userdata[u.Key] = u.Value
		}
	}
	return s, nil
}

// Snapshots lists the root snapshots from their info.xml files (no D-Bus
// and no snapperd needed), oldest first.
func (e *Env) Snapshots(ctx context.Context) []Snapshot {
	var out []Snapshot
	for _, f := range e.Glob(filepath.Join(e.SnapshotDir, "*", "info.xml")) {
		b, err := e.ReadFile(f)
		if err != nil {
			continue
		}
		s, err := ParseInfoXML(b)
		if err != nil || s.Number == 0 {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// OrphanPre returns pre snapshots without a post partner, older than
// minAge (a transaction still running has no post yet).
func OrphanPre(snaps []Snapshot, now time.Time, minAge time.Duration) []Snapshot {
	hasPost := map[int]bool{}
	for _, s := range snaps {
		if s.Type == "post" {
			hasPost[s.Pre] = true
		}
	}
	var out []Snapshot
	for _, s := range snaps {
		if s.Type == "pre" && !hasPost[s.Number] && !s.Time.IsZero() && now.Sub(s.Time) >= minAge {
			out = append(out, s)
		}
	}
	return out
}

// PackageDiff compares the rpm databases of two snapshots (0 = the
// running root).
type PackageDiff struct {
	Added    []string `json:"added,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Changed  []string `json:"changed,omitempty"` // name: old -> new
	Error    string   `json:"error,omitempty"`
	FromSize int      `json:"from_count"`
	ToSize   int      `json:"to_count"`
}

func (e *Env) rpmdb(n int) string {
	if n == 0 {
		return "/usr/lib/sysimage/rpm"
	}
	return fmt.Sprintf("%s/%d/snapshot/usr/lib/sysimage/rpm", e.SnapshotDir, n)
}

func (e *Env) packages(ctx context.Context, n int) (map[string]string, error) {
	res := e.R.Read(ctx, "rpm", "-qa", "--dbpath", e.rpmdb(n), "--qf", `%{NAME}.%{ARCH} %{EPOCHNUM}:%{VERSION}-%{RELEASE}\n`)
	if res.Err != nil || res.Code != 0 {
		return nil, fmt.Errorf("rpm -qa on snapshot %d: %s", n, firstLine(res.Out))
	}
	m := map[string]string{}
	for _, l := range strings.Split(res.Out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			if old, dup := m[k]; dup {
				v = old + "," + v // installonly packages (kernels)
			}
			m[k] = v
		}
	}
	return m, nil
}

// DiffPackages compares two snapshots' package sets.
func (e *Env) DiffPackages(ctx context.Context, from, to int) PackageDiff {
	var d PackageDiff
	if e.Confined {
		// rpm opens its database read-write (it sets attributes on the
		// sqlite files); the confined domain may not.
		d.Error = "package comparison needs `basalt snapshots diff` as root"
		return d
	}
	a, err := e.packages(ctx, from)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	b, err := e.packages(ctx, to)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.FromSize, d.ToSize = len(a), len(b)
	for k, v := range b {
		if old, ok := a[k]; !ok {
			d.Added = append(d.Added, k+" "+v)
		} else if old != v {
			d.Changed = append(d.Changed, k+": "+old+" -> "+v)
		}
	}
	for k, v := range a {
		if _, ok := b[k]; !ok {
			d.Removed = append(d.Removed, k+" "+v)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Strings(d.Changed)
	return d
}

// FileDiff summarizes `snapper status` between two snapshots.
type FileDiff struct {
	Total   int            `json:"total"`
	ByTop   map[string]int `json:"by_top"`
	Etc     []string       `json:"etc,omitempty"` // changed files under /etc (first 30)
	Skipped string         `json:"skipped,omitempty"`
}

// DiffFiles runs snapper status (not in the confined daemon).
func (e *Env) DiffFiles(ctx context.Context, from, to int) FileDiff {
	fd := FileDiff{ByTop: map[string]int{}}
	if e.Confined {
		fd.Skipped = "file comparison needs root (snapper status)"
		return fd
	}
	res := e.R.Read(ctx, "snapper", "--no-dbus", "-c", "root", "status", fmt.Sprintf("%d..%d", from, to))
	if res.Code != 0 {
		fd.Skipped = "snapper status: " + firstLine(res.Out)
		return fd
	}
	for _, l := range strings.Split(res.Out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		p := f[len(f)-1]
		fd.Total++
		top := "/" + strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)[0]
		if strings.HasPrefix(p, "/usr/") {
			top = "/usr/" + strings.SplitN(strings.TrimPrefix(p, "/usr/"), "/", 2)[0]
		}
		fd.ByTop[top]++
		if strings.HasPrefix(p, "/etc/") && len(fd.Etc) < 30 {
			fd.Etc = append(fd.Etc, f[0]+" "+p)
		}
	}
	return fd
}

// RollbackPlan proposes making a snapshot the new root.
type RollbackPlan struct {
	Target   Snapshot        `json:"target"`
	Packages PackageDiff     `json:"packages"`
	Files    FileDiff        `json:"files"`
	Actions  []action.Action `json:"actions"`
	Notes    []string        `json:"notes"`
}

// PlanRollback builds the proposal for rolling back to snapshot n.
func (e *Env) PlanRollback(ctx context.Context, n int) (*RollbackPlan, error) {
	var target *Snapshot
	for _, s := range e.Snapshots(ctx) {
		if s.Number == n {
			s := s
			target = &s
		}
	}
	if target == nil {
		return nil, fmt.Errorf("no snapshot %d", n)
	}
	p := &RollbackPlan{Target: *target}
	// From the current root to the target: what the rollback changes.
	p.Packages = e.DiffPackages(ctx, 0, n)
	p.Files = e.DiffFiles(ctx, 0, n)
	p.Actions = []action.Action{{Kind: action.SnapshotRollback, Params: map[string]string{"snapshot": strconv.Itoa(n)}}}
	p.Notes = []string{
		"the rollback takes effect at the next boot; reboot when ready",
		"data subvolumes (/home, /srv, /var/log, /var/lib/containers, databases, ...) are not rolled back",
		"snapper keeps a read-only snapshot of the current root, so this can be undone",
	}
	return p, nil
}

// FindApplySnapshots returns the pre and post snapshots `basalt apply` took
// for a proposal.
func (e *Env) FindApplySnapshots(ctx context.Context, proposalID string) (pre, post int) {
	for _, s := range e.Snapshots(ctx) {
		if s.Userdata["basalt"] == "apply" && s.Userdata["proposal"] == proposalID {
			if s.Type == "pre" {
				pre = s.Number
			} else if s.Type == "post" {
				post = s.Number
			}
		}
	}
	return pre, post
}
