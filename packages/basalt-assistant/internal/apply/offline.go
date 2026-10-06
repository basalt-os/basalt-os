package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// Pending is an offline update waiting for the next start: the proposal
// and the snapshot taken before it. basalt apply writes it once the
// update is prepared; basalt-offline-finish.service reads it at the next
// start (FinishOffline). It lives in the root, like the proposals: a
// rollback to a snapshot from before the update takes it away too.
type Pending struct {
	Proposal    string    `json:"proposal"`
	PreSnapshot int       `json:"pre_snapshot"`
	Time        time.Time `json:"time"`
}

// PendingFile is the name of the record in the state directory.
const PendingFile = "offline-pending.json"

func (a *Applier) pendingPath() string {
	dir := a.StateDir
	if dir == "" {
		dir = filepath.Dir(a.Store.Dir)
	}
	return filepath.Join(dir, PendingFile)
}

// ReadPending returns the offline update waiting for the next start.
func (a *Applier) ReadPending() (Pending, error) {
	var p Pending
	b, err := os.ReadFile(a.pendingPath())
	if os.IsNotExist(err) {
		return p, ErrNoPending
	}
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

func writePending(path string, p Pending) error {
	b, _ := json.MarshalIndent(p, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ErrStillStaged: the offline update did not run at this start.
var ErrStillStaged = errors.New("the offline update is still staged")

// ErrNoPending: no offline update waits.
var ErrNoPending = errors.New("no offline update waits for this start")

// FinishOffline completes the record of an offline update at the start
// after it ran: the "after" snapshot, the checks that every package is
// installed, the proposal's result and the audit record. When the
// offline transaction did not run or failed, the checks say so and the
// update can be undone with the snapshot taken before it.
func (a *Applier) FinishOffline(ctx context.Context) error {
	path := a.pendingPath()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ErrNoPending
	}
	if err != nil {
		return err
	}
	var pend Pending
	if err := json.Unmarshal(b, &pend); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("%s: %v", path, err)
	}
	p, err := a.Store.Load(pend.Proposal)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	// A start without the offline transaction (the person cancelled the
	// restart into it and restarted later another way): it is still
	// staged, and waits for "Restart and update".
	if st := a.Exec.Read(ctx, "dnf", "offline", "status"); strings.Contains(st.Out, "offline transaction was initiated") {
		a.printf("%s is still staged: it installs when the computer restarts into it.\n", p.ID)
		return ErrStillStaged
	}
	res := p.Result
	if res == nil {
		res = &proposal.Result{Time: pend.Time, PreSnapshot: pend.PreSnapshot}
	}
	res.Time = time.Now().UTC()
	if pend.PreSnapshot > 0 && a.Snapshots && snapperReady() {
		if n, err := a.snapper(ctx, "post", pend.PreSnapshot, "basalt apply "+p.ID+" (installed at start)", p.ID); err == nil {
			res.PostSnapshot = n
			a.printf("Snapshot %d taken (after the update).\n", n)
		}
	}
	// dnf's own record of the offline transaction, for the report.
	log := a.Exec.Read(ctx, "dnf", "offline", "log", "--number=-1")
	res.Steps = append(res.Steps, proposal.Step{What: "the offline transaction at the start", OK: log.OK(), Output: clip(log.Out, 4000)})
	ok := true
	res.Checks = nil
	for _, act := range p.Actions {
		for _, chk := range act.InstalledChecks() {
			good, detail := chk.Run(ctx, a.Exec)
			res.Checks = append(res.Checks, proposal.Step{What: chk.Description, OK: good, Output: detail})
			a.printf("  [%s] %s: %s\n", mark(good), chk.Description, detail)
			ok = ok && good
		}
	}
	res.OK = ok
	p.Result = res
	p.Status = proposal.Failed
	if ok {
		p.Status = proposal.Applied
	}
	rec := a.audit("apply", fmt.Sprintf("%s %s at the start: %s", p.ID, p.Status, p.Title), map[string]any{
		"proposal": p.ID, "title": p.Title, "actions": p.Actions, "result": res, "offline": true})
	res.AuditSeq = rec.Seq
	if err := a.Store.Save(p); err != nil {
		a.printf("Warning: could not update the proposal: %v\n", err)
	}
	_ = os.Remove(path)
	if ok && a.OnUpdated != nil {
		a.OnUpdated()
	}
	a.printf("%s: %s (snapshots %s before, %s after)\n", p.ID, p.Status, strconv.Itoa(res.PreSnapshot), strconv.Itoa(res.PostSnapshot))
	if !ok {
		return fmt.Errorf("%s did not install at the start; undo it with the snapshot %d taken before it", p.ID, res.PreSnapshot)
	}
	return nil
}
