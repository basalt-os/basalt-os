package diag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
)

// ConfirmHint checks, from the root view, the snapshot a hint of the
// confined view rests on (see action.Hint), before it may become a
// proposal:
//
//   - file.restore: the snapshot exists and holds a copy that differs from
//     the current file; when the unit has a config checker, the copy must
//     pass it in place of the current file (in the sandbox);
//   - snapshot.rollback: the snapshot exists; what the rollback changes
//     (packages, files) becomes evidence.
//
// unit is the unit the hint is about ("" when unknown). review is true when
// the check could not prove the change works (no checker for the file).
func (e *Env) ConfirmHint(ctx context.Context, unit string, acts []action.Action) (evidence []string, review bool, err error) {
	if e.Confined {
		return nil, false, errors.New("a hint can only be confirmed from the root view (sudo basalt confirm)")
	}
	for _, a := range acts {
		if err := a.Validate(); err != nil {
			return nil, false, fmt.Errorf("%s: %w", a.Kind, err)
		}
		if a.Kind == action.UnitRestart && unit == "" {
			unit = a.Params["unit"]
		}
	}
	for _, a := range acts {
		switch a.Kind {
		case action.FileRestore:
			ev, rv, err := e.confirmRestore(ctx, unit, a.Params["path"], a.Params["snapshot"])
			if err != nil {
				return nil, false, err
			}
			evidence, review = append(evidence, ev...), review || rv
		case action.SnapshotRollback:
			n, _ := strconv.Atoi(a.Params["snapshot"])
			plan, err := e.PlanRollback(ctx, n)
			if err != nil {
				return nil, false, err
			}
			pd := plan.Packages
			if pd.Error != "" {
				evidence = append(evidence, "packages: "+pd.Error)
				review = true
			} else {
				evidence = append(evidence, fmt.Sprintf("confirmed as root: rolling back to snapshot %d (%s, %q) changes %d package(s): %d added, %d removed, %d changed",
					n, plan.Target.Date, plan.Target.Description, len(pd.Added)+len(pd.Removed)+len(pd.Changed), len(pd.Added), len(pd.Removed), len(pd.Changed)))
				for _, l := range limitLines(pd.Changed, 10) {
					evidence = append(evidence, "  "+l)
				}
				for _, l := range limitLines(pd.Removed, 10) {
					evidence = append(evidence, "  removed by the rollback: "+l)
				}
				for _, l := range limitLines(pd.Added, 10) {
					evidence = append(evidence, "  back after the rollback: "+l)
				}
			}
			evidence = append(evidence, plan.Notes...)
		}
	}
	return evidence, review, nil
}

func (e *Env) confirmRestore(ctx context.Context, unit, file, snap string) ([]string, bool, error) {
	n, _ := strconv.Atoi(snap)
	found := false
	for _, s := range e.Snapshots(ctx) {
		if s.Number == n {
			found = true
		}
	}
	if !found {
		return nil, false, fmt.Errorf("snapshot %d does not exist (any more)", n)
	}
	cand := fmt.Sprintf("%s/%d/snapshot%s", e.SnapshotDir, n, file)
	old, err := e.ReadFile(cand)
	if err != nil {
		return nil, false, fmt.Errorf("snapshot %d has no copy of %s: %v", n, file, err)
	}
	cur, err := e.ReadFile(file)
	if err == nil && bytes.Equal(old, cur) {
		return nil, false, fmt.Errorf("snapshot %d holds the same %s as now: restoring it changes nothing", n, file)
	}
	ev := []string{}
	name := strings.TrimSuffix(unit, path.Ext(unit))
	name, _, _ = strings.Cut(name, "@")
	argv := checkers[name]
	review := false
	if argv == nil {
		ev = append(ev, fmt.Sprintf("confirmed as root: snapshot %d holds a different copy of %s; %s has no config checker, so review the difference before applying",
			n, file, orDash(unit)))
		review = true
	} else {
		cmd := strings.Join(argv, " ")
		res, skip := e.runChecker(ctx, argv, map[string]string{file: cand})
		switch {
		case skip != "":
			return nil, false, fmt.Errorf("cannot test snapshot %d's copy of %s with `%s`: %s", n, file, cmd, skip)
		case res.Err != nil || res.Code != 0:
			return nil, false, fmt.Errorf("snapshot %d's copy of %s fails `%s` too (%s): not a fix; run `basalt why %s` for another candidate",
				n, file, cmd, lastLine(res.Out), unit)
		}
		ev = append(ev, fmt.Sprintf("confirmed as root: snapshot %d's copy of %s passes `%s` in place of the current file", n, file, cmd))
	}
	if d := trimLines(e.R.Read(ctx, "diff", "-u", cand, file).Out, 30); d != "" {
		ev = append(ev, "difference from snapshot "+snap+":")
		for _, l := range strings.Split(d, "\n") {
			ev = append(ev, "  "+l)
		}
	}
	return ev, review, nil
}

func limitLines(xs []string, n int) []string {
	if len(xs) > n {
		return append(xs[:n:n], fmt.Sprintf("(%d more)", len(xs)-n))
	}
	return xs
}
