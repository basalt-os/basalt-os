package report

import (
	"regexp"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// The assistant's own executor units apply a proposal a person confirmed:
// basalt-apply@ID_CODE.service (confirmed in Settings, with polkit) and
// basalt-gate-exec@REQUEST.service (approved through the gate). When one
// of them fails, the proposal's own record says how the apply ended. A
// unit proposal named after the executor ("basalt-apply@p-..._....service
// stopped: cause unknown") tells a person nothing, so these units never
// get one: the daemon either stays quiet (the record speaks, or the apply
// was interrupted and the proposal still waits) or files a report in plain
// words about the change that did not finish.
var (
	reApplyUnit    = regexp.MustCompile(`^basalt-apply@(p-[0-9a-f]{6})_[0-9a-f]{8}\.service$`)
	reGateExecUnit = regexp.MustCompile(`^basalt-gate-exec@g-[0-9a-f]{12}\.service$`)
	reProposalID   = regexp.MustCompile(`\bp-[0-9a-f]{6}\b`)
)

// ExecutorUnit reports whether unit is one of the assistant's executor
// units.
func ExecutorUnit(unit string) bool {
	return reApplyUnit.MatchString(unit) || reGateExecUnit.MatchString(unit)
}

// ExecutorProposalID returns the proposal an executor unit applied: from
// the unit's name (basalt-apply@ID_CODE), else the first proposal id in
// its journal lines (basalt-gate-exec names the gate request, and basalt
// apply logs the proposal it works on). "" when neither says.
func ExecutorProposalID(r *diag.UnitReport) string {
	if m := reApplyUnit.FindStringSubmatch(r.Unit); m != nil {
		return m[1]
	}
	for _, l := range r.Journal {
		if id := reProposalID.FindString(l); id != "" {
			return id
		}
	}
	return ""
}

// ExecutorInterrupted reports an executor stopped from outside rather than
// ended by its own error: killed by a signal (a package upgrade that
// restarts or re-executes the service manager, a shutdown, a reboot, a
// stop by hand) or cut by its time limit. Nothing reached the end, so
// the proposal still waits and can be applied again.
func ExecutorInterrupted(r *diag.UnitReport) bool {
	switch strings.TrimSpace(r.State["Result"]) {
	case "signal", "timeout", "core-dump", "watchdog":
		return true
	}
	// ExecMainCode 2 (killed) or 3 (dumped): the process did not exit by
	// itself.
	code := strings.TrimSpace(r.State["ExecMainCode"])
	return code == "2" || code == "3"
}

// ExecutorRecorded reports that the proposal's record already tells how
// its apply ended (applied, failed, scheduled, or closed otherwise): the
// person sees that outcome on the proposal itself.
func ExecutorRecorded(p *proposal.Proposal) bool {
	return p != nil && p.Status != proposal.Pending
}

// FromExecutor is the report for an executor that ended with an error the
// proposal's record does not show: which change did not finish, why when
// the log says it, and what the person can do. It changes nothing (no
// actions); the change itself stays waiting.
func FromExecutor(source string, r *diag.UnitReport, p *proposal.Proposal) *proposal.Proposal {
	id := ExecutorProposalID(r)
	what := "a change you confirmed"
	switch {
	case p != nil && strings.TrimSpace(p.Title) != "":
		what = "\"" + strings.TrimSpace(p.Title) + "\""
	case id != "":
		what = "the change " + id
	}
	key := "apply:" + r.Unit
	if id != "" {
		key = "apply:" + id
	}
	out := base(source, "apply", r.Unit, key)
	out.Title = "Applying " + what + " did not finish"
	why := relevantLine(r.Journal)
	var b strings.Builder
	b.WriteString("Applying " + what + " stopped before it was done")
	if why != "" {
		b.WriteString(". The log says: " + why + ".")
	} else {
		b.WriteString(", and its log does not say why.")
	}
	if p != nil && p.Status == proposal.Pending {
		b.WriteString(" The change itself (" + p.ID + ") still waits for you: open it to try again, or dismiss it.")
	}
	b.WriteString(" Nothing will be changed by this report.")
	out.Report = b.String()
	out.Evidence = append(out.Evidence, "executor unit: "+r.Unit)
	if st := r.State; st != nil {
		out.Evidence = append(out.Evidence, "result: "+orDash(st["Result"])+", exit status "+orDash(st["ExecMainStatus"]))
	}
	for _, l := range r.Journal {
		out.Evidence = append(out.Evidence, "journal: "+l)
	}
	out.Severity = 2
	return finish(out)
}
