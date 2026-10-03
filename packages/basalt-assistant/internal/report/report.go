// Package report turns diagnoses into proposals and renders them as text
// from templates: what is wrong, the evidence, the proposed change with its
// exact commands, the decision behind it, and how to apply or ignore it.
// No language model is involved.
package report

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

func base(source, kind, subject, key string) *proposal.Proposal {
	now := time.Now().UTC()
	return &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1,
		Source: source, Kind: kind, Subject: subject, Key: key, Status: proposal.Pending}
}

// needsReview: below threshold, or nothing to apply.
func finish(p *proposal.Proposal, ds ...decide.Decision) *proposal.Proposal {
	p.Decisions = append(p.Decisions, ds...)
	for _, d := range ds {
		if !d.Confident {
			p.NeedsReview = true
		}
	}
	if len(p.Actions) == 0 {
		p.NeedsReview = true
	}
	return p
}

// FromUnit builds a proposal from a unit diagnosis.
func FromUnit(source string, r *diag.UnitReport) *proposal.Proposal {
	p := base(source, "unit", r.Unit, "unit:"+r.Unit+":"+r.Cause)
	p.Title = fmt.Sprintf("%s failed: %s", r.Unit, strings.ReplaceAll(r.Cause, "_", " "))
	if r.Healthy {
		p.Title = r.Unit + " is running"
	}
	p.Report = r.Explanation
	p.Evidence = append(p.Evidence, r.Evidence...)
	for _, l := range r.Journal {
		p.Evidence = append(p.Evidence, "journal: "+l)
	}
	if r.ConfigCheck != nil {
		p.Evidence = append(p.Evidence, fmt.Sprintf("config check `%s`: %s", r.ConfigCheck.Command, okFail(r.ConfigCheck.OK)))
		for _, l := range strings.Split(r.ConfigCheck.Output, "\n") {
			if strings.TrimSpace(l) != "" {
				p.Evidence = append(p.Evidence, "  "+l)
			}
		}
	}
	for _, f := range r.AVCs {
		p.Evidence = append(p.Evidence, f.Evidence...)
	}
	for _, d := range r.Deps {
		p.Evidence = append(p.Evidence, fmt.Sprintf("dependency %s: %s (%s, load %s)", d.Unit, d.Active, d.Result, d.Load))
	}
	for _, pi := range r.Ports {
		p.Evidence = append(p.Evidence, fmt.Sprintf("port %d: %s", pi.Port, orDash(pi.Owner)))
	}
	for _, d := range r.Disks {
		p.Evidence = append(p.Evidence, fmt.Sprintf("%s is %.0f %% full", d.Path, d.UsedPct()))
	}
	if r.Restore != nil && r.Restore.Diff != "" {
		p.Evidence = append(p.Evidence, "difference from snapshot "+strconv.Itoa(r.Restore.Snapshot)+":")
		for _, l := range strings.Split(r.Restore.Diff, "\n") {
			p.Evidence = append(p.Evidence, "  "+l)
		}
	}
	for _, s := range r.Skipped {
		p.Evidence = append(p.Evidence, "not checked here: "+s)
	}
	p.Actions = r.Actions
	p.Severity = 4
	return finish(p, append([]decide.Decision{r.Decision}, r.AVCDecision...)...)
}

// FromSELinux builds a proposal from one denial group.
func FromSELinux(source string, it diag.SELinuxItem) *proposal.Proposal {
	a := it.Fix.Group.AVC
	p := base(source, "selinux", a.SType()+" -> "+a.TType(), "avc:"+a.Key())
	p.Title = fmt.Sprintf("SELinux denied %s { %s } on %s: %s", a.SType(), strings.Join(a.Perms, " "),
		objectOf(it.Fix), strings.ReplaceAll(it.Decision.Answer.Top, "_", " "))
	p.Report = it.Fix.Explanation + "."
	switch it.Decision.Answer.Top {
	case selinux.ClassSuspicious:
		p.Report += " The target is security sensitive: this looks like the policy doing its job. No change is proposed; find out why the program tried this."
	case selinux.ClassUnknown:
		p.Report += " No known fix; a local policy module is not generated automatically."
	}
	p.Evidence = append(p.Evidence, it.Fix.Evidence...)
	p.Evidence = append(p.Evidence, "raw: "+a.Raw)
	if it.Decision.Confident && it.Decision.Answer.Top != selinux.ClassUnknown && it.Decision.Answer.Top != selinux.ClassSuspicious {
		p.Actions = it.Fix.Actions
	}
	p.Severity = 3
	return finish(p, it.Decision)
}

func objectOf(f selinux.Fix) string {
	a := f.Group.AVC
	if f.Path != "" {
		return f.Path
	}
	if a.IsPort() {
		return a.Proto() + " port " + strconv.Itoa(a.Port)
	}
	if a.Name != "" {
		return a.Name
	}
	return a.Class
}

// FromDisk builds a proposal from a disk report.
func FromDisk(source string, r *diag.DiskReport) *proposal.Proposal {
	p := base(source, "disk", "/", "disk:/")
	p.Title = fmt.Sprintf("root file system %.0f %% full: %s", r.UsedPct, strings.ReplaceAll(r.Decision.Answer.Top, "_", " "))
	p.Report = r.Explanation
	p.Evidence = append(p.Evidence, r.Evidence...)
	for i, s := range r.Snapshots {
		if i >= 5 {
			break
		}
		p.Evidence = append(p.Evidence, fmt.Sprintf("snapshot %d (%s %q) holds %s exclusively", s.Snapshot.Number, s.Snapshot.Date, s.Snapshot.Description, diag.HumanBytes(s.Exclusive)))
	}
	for _, s := range r.Skipped {
		p.Evidence = append(p.Evidence, "not checked here: "+s)
	}
	p.Actions = r.Actions
	p.Severity = 3
	if r.Features["disk_crit"] {
		p.Severity = 5
	}
	return finish(p, r.Decision)
}

// FromDnf builds a proposal from a failed transaction.
func FromDnf(source string, r *diag.DnfReport) *proposal.Proposal {
	key := "dnf:"
	if r.Pre != nil {
		key += strconv.Itoa(r.Pre.Number)
	}
	p := base(source, "dnf", r.Command, key)
	p.Title = fmt.Sprintf("package transaction failed: %s", orDash(r.Command))
	p.Report = r.Explanation
	p.Evidence = append(p.Evidence, r.Evidence...)
	for _, l := range r.LogLines {
		p.Evidence = append(p.Evidence, "dnf5.log: "+l)
	}
	for i, x := range r.Packages.Changed {
		if i >= 10 {
			p.Evidence = append(p.Evidence, fmt.Sprintf("(%d more changed)", len(r.Packages.Changed)-10))
			break
		}
		p.Evidence = append(p.Evidence, "changed: "+x)
	}
	for i, x := range r.Packages.Added {
		if i >= 10 {
			break
		}
		p.Evidence = append(p.Evidence, "added: "+x)
	}
	p.Actions = r.Actions
	p.Severity = 4
	return finish(p, r.Decision)
}

// FromRollback builds a proposal to roll back to a snapshot.
func FromRollback(source string, r *diag.RollbackPlan, why string) *proposal.Proposal {
	p := base(source, "snapshot", strconv.Itoa(r.Target.Number), "rollback:"+strconv.Itoa(r.Target.Number))
	p.Title = fmt.Sprintf("roll back the root to snapshot %d (%s, %q)", r.Target.Number, r.Target.Date, r.Target.Description)
	p.Report = why
	pd := r.Packages
	if pd.Error != "" {
		p.Evidence = append(p.Evidence, "packages: "+pd.Error)
	} else {
		p.Evidence = append(p.Evidence, fmt.Sprintf("packages after rollback: %d added, %d removed, %d changed (relative to now)", len(pd.Added), len(pd.Removed), len(pd.Changed)))
		for _, l := range limit(pd.Changed, 15) {
			p.Evidence = append(p.Evidence, "  "+l)
		}
		for _, l := range limit(pd.Removed, 10) {
			p.Evidence = append(p.Evidence, "  removed: "+l)
		}
		for _, l := range limit(pd.Added, 10) {
			p.Evidence = append(p.Evidence, "  back: "+l)
		}
	}
	if r.Files.Skipped != "" {
		p.Evidence = append(p.Evidence, "files: "+r.Files.Skipped)
	} else {
		p.Evidence = append(p.Evidence, fmt.Sprintf("files that differ: %d", r.Files.Total))
		for _, l := range r.Files.Etc {
			p.Evidence = append(p.Evidence, "  "+l)
		}
	}
	p.Evidence = append(p.Evidence, r.Notes...)
	p.Actions = r.Actions
	p.Severity = 3
	return p
}

func limit(xs []string, n int) []string {
	if len(xs) > n {
		return append(xs[:n:n], fmt.Sprintf("(%d more)", len(xs)-n))
	}
	return xs
}

func okFail(ok bool) string {
	if ok {
		return "passed"
	}
	return "FAILED"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var tmpl = template.Must(template.New("p").Funcs(template.FuncMap{
	"cmds": func(a action.Action) []string {
		cs, err := a.Commands()
		if err != nil {
			return []string{"(invalid action: " + err.Error() + ")"}
		}
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.String() + "    # " + c.Description
		}
		return out
	},
	"pct": func(f float64) string { return fmt.Sprintf("%.2f", f) },
}).Parse(`[{{.P.ID}}] {{.P.Title}}
  status: {{.P.Status}}{{if .P.NeedsReview}} (needs review){{end}}, from {{.P.Source}}, {{.P.Created.Format "2006-01-02 15:04:05"}} UTC{{if gt .P.Seen 1}}, seen {{.P.Seen}} times{{end}}

What is wrong
  {{.P.Report}}
{{if .P.Evidence}}
Evidence
{{range .P.Evidence}}  - {{.}}
{{end}}{{end}}{{if .P.Decisions}}
Decision
{{range .P.Decisions}}  - {{.Question.ID}}: {{.Answer.Top}} (p={{pct .Answer.Confidence}}, threshold {{pct .Threshold}}, {{.Answer.Backend}}); {{.Answer.String}}
{{end}}{{end}}
Proposed change
{{if .P.Actions}}{{range .P.Actions}}{{range cmds .}}  $ {{.}}
{{end}}{{end}}{{if .Fingerprint}}
  Apply:  sudo basalt apply {{.P.ID}}      (non-interactive: sudo basalt apply {{.P.ID}} --yes --confirm {{.Fingerprint}})
  Ignore: sudo basalt ignore {{.P.ID}}
{{end}}{{else}}  none: this is a report; nothing will be changed.
{{end}}`))

// Render prints a proposal.
func Render(p *proposal.Proposal) string {
	fp, _ := p.Fingerprint()
	if len(p.Actions) == 0 || p.Status != proposal.Pending {
		fp = ""
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, map[string]any{"P": p, "Fingerprint": fp}); err != nil {
		return "render error: " + err.Error()
	}
	return b.String()
}

// Line is a one-line listing.
func Line(p *proposal.Proposal) string {
	flag := ""
	if p.NeedsReview {
		flag = " [review]"
	}
	if len(p.Actions) == 0 {
		flag += " [report]"
	}
	return fmt.Sprintf("%-9s %-8s %-8s %s%s", p.ID, p.Status, p.Kind, p.Title, flag)
}
