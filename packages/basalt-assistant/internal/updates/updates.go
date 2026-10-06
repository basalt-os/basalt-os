// Package updates reads what can be updated on the system and builds the
// proposals that install updates (update.install) or undo the last one
// (update.rollback). It only reads: dnf's cached repository metadata
// (refreshed by `basalt updates check`, the update.check action), the rpm
// database, the stored proposals and the snapshot list. Every change goes
// through a proposal, its confirmation and `basalt apply`, which takes a
// snapshot first.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// Groups of updates, in the order the desktop shows them.
const (
	GroupSecurity = "security" // fixes a security problem (a security advisory)
	GroupBasalt   = "basalt"   // Basalt OS's own components (the basalt channels)
	GroupApps     = "apps"     // programs with an entry in the app launcher
	GroupSystem   = "system"   // everything else: libraries, the kernel, services
)

// Update is one package that can be updated.
type Update struct {
	Name     string `json:"name"`
	NEVRA    string `json:"nevra"`
	Arch     string `json:"arch"`
	From     string `json:"from,omitempty"` // installed version (epoch:version-release)
	To       string `json:"to"`
	Repo     string `json:"repo"`
	Group    string `json:"group"`
	Advisory string `json:"advisory,omitempty"`
	Severity string `json:"severity,omitempty"`
	Download int64  `json:"download_size"`
	Install  int64  `json:"install_size"`
	Summary  string `json:"summary"`
}

// History is one update applied (or tried) from a proposal.
type History struct {
	Proposal    string `json:"proposal"`
	Kind        string `json:"kind"` // install or rollback
	Time        string `json:"time"`
	Scope       string `json:"scope,omitempty"`
	Count       int    `json:"count,omitempty"`
	OK          bool   `json:"ok"`
	Status      string `json:"status"`
	PreSnapshot int    `json:"pre_snapshot,omitempty"`
	Snapshot    int    `json:"snapshot,omitempty"` // rollback target
	Undoes      string `json:"undoes,omitempty"`
}

// Running is an apply in progress.
type Running struct {
	Proposal string `json:"proposal"`
	Kind     string `json:"kind"`
	Step     int    `json:"step"`
	Steps    int    `json:"steps"`
	What     string `json:"what"`
}

// Restart says whether the computer should restart, and why.
type Restart struct {
	Needed          bool     `json:"needed"`
	Reasons         []string `json:"reasons,omitempty"`
	RollbackPending bool     `json:"rollback_pending"`
}

// Undo is the last update that can be undone.
type Undo struct {
	Proposal string `json:"proposal"`
	Snapshot int    `json:"snapshot"`
	Time     string `json:"time"`
	Count    int    `json:"count"`
}

// Report is `basalt updates --json`.
type Report struct {
	Checked     string         `json:"checked,omitempty"` // when the metadata was last refreshed (RFC 3339)
	Updates     []Update       `json:"updates"`
	Counts      map[string]int `json:"counts"`
	Download    int64          `json:"download_size"`
	Restart     Restart        `json:"restart"`
	Running     *Running       `json:"running,omitempty"`
	Pending     string         `json:"pending,omitempty"` // a stored update proposal waiting for a decision
	History     []History      `json:"history"`
	Undo        *Undo          `json:"undo,omitempty"`
	AutoSecure  bool           `json:"auto_security"`
	AutoNote    string         `json:"auto_security_note"`
	Errors      []string       `json:"errors,omitempty"`
	CacheOnly   bool           `json:"cache_only"`
	Kernel      string         `json:"kernel,omitempty"`
	CheckedNote string         `json:"checked_note,omitempty"`
	// Offline: installing all of them replaces core parts of the system,
	// so they install at the next start (OfflineSecurity: the security
	// ones only). Core names the core packages among them.
	Offline         bool     `json:"offline"`
	OfflineSecurity bool     `json:"offline_security"`
	Core            []string `json:"core,omitempty"`
	// Scheduled: an update prepared to install at the next start.
	Scheduled string `json:"scheduled,omitempty"`
}

// Cache is what the last check found, kept by root in the assistant's
// state directory. Only root's processes that may run dnf write it (the
// check, an apply, the start after an offline update): the desktop and
// the read helper only read it, so nothing in the desktop's domain runs
// dnf or rpm.
type Cache struct {
	Checked string   `json:"checked"`
	Updates []Update `json:"updates"`
	Errors  []string `json:"errors,omitempty"`
	Kernel  string   `json:"kernel,omitempty"`
	// The restart state when it was written, valid for that boot only.
	BootID  string   `json:"boot_id"`
	Restart []string `json:"restart,omitempty"`
}

// Sys is what the report reads; tests feed fixtures.
type Sys struct {
	R runner.Reader
	// AuditPath: the assistant's audit log. It lives on /var/log, outside
	// the root snapshots, so the history it gives survives an undo (the
	// proposals themselves go back with the root).
	AuditPath string
	Store     proposal.Store
	CachePath string // the Cache
	BootID    func() string
	AppsDir   string // desktop entries (apps group)
	BootTime  func() time.Time
	// PreSnapshots: the "pre" snapshots basalt apply took, by proposal
	// (snapper userdata basalt=apply, proposal=ID). An apply that never
	// finished (the computer stopped, a command hung) has one but no
	// result; it can still be undone.
	PreSnapshots func() map[string]int
}

// RepoqueryFormat is the line format of the updates query: the summary is
// last because it may hold the separator.
const RepoqueryFormat = `%{full_nevra}|%{name}|%{evr}|%{arch}|%{repoid}|%{downloadsize}|%{installsize}|%{summary}\n`

// ParseRepoquery reads the lines of the updates query.
func ParseRepoquery(out string) []Update {
	var us []Update
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 8)
		if len(f) < 8 || f[0] == "" || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		dl, _ := strconv.ParseInt(f[5], 10, 64)
		in, _ := strconv.ParseInt(f[6], 10, 64)
		us = append(us, Update{NEVRA: normNEVRA(f[0]), Name: f[1], To: f[2], Arch: f[3], Repo: f[4], Download: dl, Install: in, Summary: f[7]})
	}
	sort.Slice(us, func(i, j int) bool {
		return us[i].Name < us[j].Name || us[i].Name == us[j].Name && us[i].Arch < us[j].Arch
	})
	return us
}

// normNEVRA drops an explicit zero epoch ("name-0:1.0-1.x86_64").
func normNEVRA(n string) string {
	if i := strings.Index(n, "-0:"); i >= 0 {
		return n[:i+1] + n[i+3:]
	}
	return n
}

// Advisory is one entry of `dnf advisory list --updates --json`.
type Advisory struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Severity string `json:"severity"`
	NEVRA    string `json:"nevra"`
}

// ParseAdvisories reads `dnf advisory list --json`.
func ParseAdvisories(out string) (map[string]Advisory, error) {
	m := map[string]Advisory{}
	i := strings.Index(out, "[")
	if i < 0 {
		return m, nil
	}
	var as []Advisory
	if err := json.Unmarshal([]byte(out[i:]), &as); err != nil {
		return m, fmt.Errorf("advisories: %v", err)
	}
	for _, a := range as {
		n := normNEVRA(a.NEVRA)
		if old, ok := m[n]; ok && old.Type == "security" && a.Type != "security" {
			continue
		}
		m[n] = a
	}
	return m, nil
}

// classify puts each update in its group.
func classify(us []Update, adv map[string]Advisory, apps map[string]bool) {
	for i := range us {
		u := &us[i]
		if a, ok := adv[u.NEVRA]; ok {
			u.Advisory, u.Severity = a.Name, a.Severity
			if a.Type == "security" {
				u.Group = GroupSecurity
				continue
			}
		}
		switch {
		case strings.HasPrefix(u.Repo, "basalt"):
			u.Group = GroupBasalt
		case apps[u.Name]:
			u.Group = GroupApps
		default:
			u.Group = GroupSystem
		}
	}
}

// restartPackages are the packages whose update needs a restart: the list
// dnf's needs-restarting uses (the SELinux policy is not on it: it reloads
// live).
var restartPackages = []string{"kernel-core", "kernel", "glibc", "linux-firmware", "systemd", "systemd-udev", "dbus-broker",
	"dbus-daemon", "openssl-libs", "gnutls", "microcode_ctl"}

// restartReasons compares the install time of those packages with the
// boot time (root, with the cache).
func restartReasons(ctx context.Context, s Sys) []string {
	var reasons []string
	boot := s.BootTime()
	res := s.R.Read(ctx, append([]string{"rpm", "-q", "--qf", `%{NAME} %{INSTALLTIME} %{VERSION}-%{RELEASE}.%{ARCH}\n`}, restartPackages...)...)
	seen := map[string]bool{}
	for _, line := range strings.Split(res.Out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		t, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || boot.IsZero() || !time.Unix(t, 0).After(boot) || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		reasons = append(reasons, f[0]+" "+f[2])
	}
	return reasons
}

// restartNeeded is the restart state: the reasons the cache recorded in
// this boot, and a rollback waiting for the next one.
func restartNeeded(ctx context.Context, s Sys, c Cache) Restart {
	var r Restart
	if c.BootID != "" && s.BootID != nil && c.BootID == s.BootID() && len(c.Restart) > 0 {
		r.Needed, r.Reasons = true, append(r.Reasons, c.Restart...)
	}
	// A rollback waits for the next boot: the default subvolume is not the
	// running root.
	def := strings.Fields(s.R.Read(ctx, "btrfs", "subvolume", "get-default", "/").Out)
	root := strings.TrimSpace(s.R.Read(ctx, "btrfs", "inspect-internal", "rootid", "/").Out)
	if len(def) >= 2 && root != "" && def[1] != root {
		r.RollbackPending, r.Needed = true, true
		r.Reasons = append(r.Reasons, "rollback")
	}
	return r
}

// appPackages: packages that own an entry of the app launcher.
func appPackages(ctx context.Context, s Sys) map[string]bool {
	m := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(s.AppsDir, "*.desktop"))
	if len(files) == 0 {
		return m
	}
	res := s.R.Read(ctx, append([]string{"rpm", "-qf", "--qf", `%{NAME}\n`}, files...)...)
	for _, l := range strings.Split(res.Out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.Contains(l, " ") {
			m[l] = true
		}
	}
	return m
}

// installed reads the installed version of each package name.arch.
func installed(ctx context.Context, s Sys) map[string]string {
	m := map[string]string{}
	res := s.R.Read(ctx, "rpm", "-qa", "--qf", `%{NAME}.%{ARCH} %{EPOCHNUM}:%{VERSION}-%{RELEASE}\n`)
	for _, l := range strings.Split(res.Out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			v := strings.TrimPrefix(f[1], "0:")
			if old, ok := m[f[0]]; !ok || old < v {
				m[f[0]] = v
			}
		}
	}
	return m
}

// Query lists the updates from dnf's cache (cacheOnly) or after loading
// the metadata. Errors are reported, not fatal.
func Query(ctx context.Context, s Sys, cacheOnly bool) ([]Update, []string) {
	var errs []string
	args := []string{"dnf", "repoquery", "-q", "--upgrades", "--latest-limit=1", "--queryformat", RepoqueryFormat}
	adv := []string{"dnf", "advisory", "list", "-q", "--updates", "--json"}
	if cacheOnly {
		args = append(args, "--cacheonly")
		adv = append(adv, "--cacheonly")
	}
	res := s.R.Read(ctx, args...)
	if res.Err != nil || res.Code != 0 {
		errs = append(errs, "dnf repoquery: "+firstLine(res.Out, res.Err))
	}
	us := ParseRepoquery(res.Out)
	ares := s.R.Read(ctx, adv...)
	advs, err := ParseAdvisories(ares.Out)
	if err != nil {
		errs = append(errs, err.Error())
	}
	classify(us, advs, appPackages(ctx, s))
	inst := installed(ctx, s)
	for i := range us {
		us[i].From = inst[us[i].Name+"."+us[i].Arch]
	}
	return us, errs
}

func firstLine(out string, err error) string {
	out = strings.TrimSpace(out)
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	if out == "" && err != nil {
		return err.Error()
	}
	return out
}

// IsUpdate reports a proposal of this package's kinds.
func IsUpdate(p *proposal.Proposal) (kind string, a action.Action, ok bool) {
	for _, x := range p.Actions {
		switch x.Kind {
		case action.UpdateInstall:
			return "install", x, true
		case action.UpdateRollback:
			return "rollback", x, true
		}
	}
	return "", action.Action{}, false
}

// applied is one finished apply, from the audit log or a stored proposal.
type applied struct {
	id     string
	time   time.Time
	ok     bool
	acts   []action.Action
	result *proposal.Result
	// interrupted: the apply started (its snapshot exists) and never
	// recorded a result.
	interrupted bool
}

func preOf(a applied) int {
	if a.result == nil {
		return 0
	}
	return a.result.PreSnapshot
}

// fromAudit reads the apply records of update proposals, newest first.
func fromAudit(path string) ([]applied, bool) {
	if path == "" {
		return nil, false
	}
	recs, err := audit.Tail(path, 5000)
	if err != nil {
		return nil, false
	}
	var out []applied
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.Type != "apply" {
			continue
		}
		var d struct {
			Proposal string           `json:"proposal"`
			Actions  []action.Action  `json:"actions"`
			Result   *proposal.Result `json:"result"`
		}
		if json.Unmarshal(r.Data, &d) != nil || d.Result == nil {
			continue
		}
		if _, _, ok := IsUpdate(&proposal.Proposal{Actions: d.Actions}); !ok {
			continue
		}
		out = append(out, applied{id: d.Proposal, time: d.Result.Time, ok: d.Result.OK, acts: d.Actions, result: d.Result})
	}
	return out, true
}

// history reads the update applies (the audit log when it can be read,
// else the stored proposals), newest first, the update being applied, a
// stored update waiting for a decision, and the last update that can
// still be undone.
func history(s Sys) ([]History, *Running, string, *Undo) {
	ps, _ := s.Store.List("")
	var run *Running
	pending := ""
	for _, p := range ps {
		k, _, ok := IsUpdate(p)
		if !ok || (p.Status != proposal.Pending && p.Status != proposal.Failed) {
			continue
		}
		if pr, live := proposal.ReadProgress(p.ID); live {
			run = &Running{Proposal: p.ID, Kind: k, Step: pr.Step, Steps: pr.Steps, What: pr.What}
		} else if p.Status == proposal.Pending && pending == "" && k == "install" {
			pending = p.ID
		}
	}
	done, ok := fromAudit(s.AuditPath)
	// Offline updates waiting for the next start.
	var scheduled []History
	for _, p := range ps {
		if _, a, isUpd := IsUpdate(p); isUpd && p.Status == proposal.Scheduled {
			h := History{Proposal: p.ID, Kind: "install", Status: proposal.Scheduled, OK: true, Time: p.Updated.UTC().Format(time.RFC3339),
				Scope: a.Params["scope"]}
			h.Count, _ = strconv.Atoi(a.Params["count"])
			if p.Result != nil {
				h.PreSnapshot = p.Result.PreSnapshot
			}
			scheduled = append(scheduled, h)
		}
	}
	if !ok {
		for _, p := range ps {
			if _, _, isUpd := IsUpdate(p); !isUpd || (p.Status != proposal.Applied && p.Status != proposal.Failed) {
				continue
			}
			a := applied{id: p.ID, time: p.Updated, ok: p.Status == proposal.Applied, acts: p.Actions, result: p.Result}
			if p.Result != nil {
				a.time = p.Result.Time
			}
			done = append(done, a)
		}
	}
	// An update is undone by a rollback to the snapshot taken before it
	// (the snapshot, not the proposal id: after a rollback the stored
	// proposals go back too, and an id can come back for a new try).
	// Interrupted applies: a stored update proposal that is not running,
	// has no recorded result, and has the snapshot basalt apply took
	// before it.
	if s.PreSnapshots != nil {
		seen := map[string]bool{}
		for _, d := range done {
			seen[d.id+"@"+strconv.Itoa(preOf(d))] = true
		}
		pres := s.PreSnapshots()
		for _, p := range ps {
			k, _, ok := IsUpdate(p)
			n := pres[p.ID]
			if !ok || k != "install" || n == 0 || p.Status != proposal.Pending || (run != nil && run.Proposal == p.ID) || seen[p.ID+"@"+strconv.Itoa(n)] {
				continue
			}
			done = append(done, applied{id: p.ID, time: p.Updated, ok: false, acts: p.Actions,
				result: &proposal.Result{Time: p.Updated, PreSnapshot: n}, interrupted: true})
			if pending == p.ID {
				pending = ""
			}
		}
		sort.SliceStable(done, func(i, j int) bool { return done[i].time.After(done[j].time) })
	}
	undone := map[string]bool{}
	for _, d := range done {
		if k, a, _ := IsUpdate(&proposal.Proposal{Actions: d.acts}); k == "rollback" && d.ok {
			undone[a.Params["snapshot"]] = true
		}
	}
	hs := scheduled
	var undo *Undo
	for _, d := range done {
		k, a, _ := IsUpdate(&proposal.Proposal{Actions: d.acts})
		h := History{Proposal: d.id, Kind: k, OK: d.ok, Time: d.time.UTC().Format(time.RFC3339), Status: map[bool]string{true: proposal.Applied, false: proposal.Failed}[d.ok]}
		if d.interrupted {
			h.Status = "interrupted"
		}
		if d.result != nil {
			h.PreSnapshot = d.result.PreSnapshot
		}
		if k == "install" {
			h.Scope = a.Params["scope"]
			h.Count, _ = strconv.Atoi(a.Params["count"])
			if undone[strconv.Itoa(h.PreSnapshot)] {
				h.Status = "undone"
			} else if undo == nil && h.PreSnapshot > 0 {
				undo = &Undo{Proposal: d.id, Snapshot: h.PreSnapshot, Time: h.Time, Count: h.Count}
			}
		} else {
			h.Snapshot, _ = strconv.Atoi(a.Params["snapshot"])
			h.Undoes = a.Params["proposal"]
		}
		hs = append(hs, h)
		if len(hs) >= 10 {
			break
		}
	}
	return hs, run, pending, undo
}

// ReadCache reads the last check's result (empty when there is none).
func ReadCache(s Sys) Cache {
	var c Cache
	if b, err := os.ReadFile(s.CachePath); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

// Refresh queries dnf's cached metadata and the rpm database and writes
// the Cache. Root only: the check, an apply and the start after an
// offline update call it, never the desktop.
func Refresh(ctx context.Context, s Sys, checked time.Time) Cache {
	old := ReadCache(s)
	us, errs := Query(ctx, s, true)
	c := Cache{Checked: old.Checked, Updates: us, Errors: errs, Kernel: strings.TrimSpace(s.R.Read(ctx, "uname", "-r").Out),
		Restart: restartReasons(ctx, s)}
	if !checked.IsZero() {
		c.Checked = checked.UTC().Format(time.RFC3339)
	}
	if s.BootID != nil {
		c.BootID = s.BootID()
	}
	if c.Updates == nil {
		c.Updates = []Update{}
	}
	b, _ := json.MarshalIndent(c, "", " ")
	if err := os.MkdirAll(filepath.Dir(s.CachePath), 0o700); err == nil {
		tmp := s.CachePath + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, s.CachePath)
		}
	}
	return c
}

// Build is the report: the updates the last check found (the Cache; no
// dnf query), the restart state, the history.
func Build(ctx context.Context, s Sys) Report {
	c := ReadCache(s)
	r := Progress(ctx, s)
	r.CacheOnly = true
	r.AutoNote = "automatic security updates come with the approval gate's schedules (docs/gate.md); until then they are installed from here"
	r.Updates, r.Errors, r.Kernel = c.Updates, c.Errors, c.Kernel
	if r.Updates == nil {
		r.Updates = []Update{}
	}
	var all, sec []string
	for _, u := range r.Updates {
		r.Counts[u.Group]++
		r.Counts["total"]++
		r.Download += u.Download
		if action.IsCore(u.Name) {
			all = append(all, u.Name)
			if u.Group == GroupSecurity {
				sec = append(sec, u.Name)
			}
		}
	}
	r.Offline, r.OfflineSecurity = len(all) > 0, len(sec) > 0
	r.Core = all
	if len(r.Core) > 12 {
		r.Core = r.Core[:12]
	}
	return r
}

// Progress is the report without the list of updates: the running step,
// the restart state, the history.
func Progress(ctx context.Context, s Sys) Report {
	c := ReadCache(s)
	r := Report{Counts: map[string]int{}, Updates: []Update{}, History: []History{}, Checked: c.Checked}
	hs, run, pending, undo := history(s)
	if hs != nil {
		r.History = hs
	}
	r.Running, r.Pending, r.Undo = run, pending, undo
	for _, h := range r.History {
		if h.Status == proposal.Scheduled {
			r.Scheduled = h.Proposal
			break
		}
	}
	if run == nil {
		r.Restart = restartNeeded(ctx, s, c)
	}
	return r
}

// Packages is the canonical package list of an update.install: the
// NEVRAs, sorted, joined by single spaces.
func Packages(us []Update, scope string) string {
	var ns []string
	for _, u := range us {
		if scope == "security" && u.Group != GroupSecurity {
			continue
		}
		ns = append(ns, u.NEVRA)
	}
	sort.Strings(ns)
	return strings.Join(ns, " ")
}

// ErrNone: nothing to update.
type ErrNone struct{ Why string }

func (e ErrNone) Error() string { return e.Why }

// InstallProposal builds the update.install proposal for the updates the
// person sees (all of them, or only the security ones).
func InstallProposal(us []Update, source, scope string) (*proposal.Proposal, error) {
	if scope == "" {
		scope = "all"
	}
	pk := Packages(us, scope)
	if pk == "" {
		if scope == "security" {
			return nil, ErrNone{"no security updates are available"}
		}
		return nil, ErrNone{"the system is up to date"}
	}
	n := len(action.UpdatePackages(pk))
	if n > action.MaxUpdatePackages {
		return nil, fmt.Errorf("%d updates are more than one proposal holds (%d); run sudo dnf upgrade", n, action.MaxUpdatePackages)
	}
	mode := "live"
	core := action.CoreIn(pk)
	if len(core) > 0 {
		mode = "offline"
	}
	a := action.Action{Kind: action.UpdateInstall, Params: map[string]string{"scope": scope, "count": strconv.Itoa(n),
		"packages": pk, "digest": action.UpdateDigest(pk), "mode": mode}}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	var dl int64
	counts := map[string]int{}
	for _, u := range us {
		if scope == "security" && u.Group != GroupSecurity {
			continue
		}
		dl += u.Download
		counts[u.Group]++
	}
	now := time.Now().UTC()
	title := fmt.Sprintf("install %d updates (%s to download)", n, human(dl))
	if scope == "security" {
		title = fmt.Sprintf("install %d security updates (%s to download)", n, human(dl))
	}
	p := &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1, Source: source,
		Kind: "update", Subject: scope, Key: "update:install:" + scope + ":" + a.Params["digest"], Status: proposal.Pending,
		Title: title, Actions: []action.Action{a}, Severity: 1, NeedsReview: source != "cli"}
	p.Report = fmt.Sprintf("%d updates are ready: %d security, %d Basalt OS components, %d apps, %d system. "+
		"A snapshot is taken first, so the update can be undone from Settings, Updates and channels, or with "+
		"sudo basalt updates rollback.", n, counts[GroupSecurity], counts[GroupBasalt], counts[GroupApps], counts[GroupSystem])
	if mode == "offline" {
		p.Title = strings.Replace(p.Title, "install ", "restart and install ", 1)
		p.Report += fmt.Sprintf(" Some of them replace core parts of the system (%s), so they install while the computer restarts, "+
			"before the desktop starts; a snapshot is taken before and after.", strings.Join(uniq(core, 8), ", "))
	}
	for _, u := range us {
		if scope == "security" && u.Group != GroupSecurity {
			continue
		}
		line := fmt.Sprintf("%s %s -> %s (%s, %s, %s)", u.Name, orDash(u.From), u.To, u.Repo, u.Group, human(u.Download))
		if u.Advisory != "" {
			line += " " + u.Advisory
		}
		p.Evidence = append(p.Evidence, line)
	}
	return p, nil
}

// RollbackProposal builds the update.rollback proposal: back to the
// snapshot taken just before the update u.
func RollbackProposal(u Undo, source string) (*proposal.Proposal, error) {
	a := action.Action{Kind: action.UpdateRollback, Params: map[string]string{"snapshot": strconv.Itoa(u.Snapshot), "proposal": u.Proposal}}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p := &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1, Source: source,
		Kind: "update", Subject: "rollback", Key: "update:rollback:" + u.Proposal, Status: proposal.Pending,
		Title:   fmt.Sprintf("undo the update %s: back to snapshot %d", u.Proposal, u.Snapshot),
		Actions: []action.Action{a}, Severity: 2, NeedsReview: source != "cli"}
	p.Report = fmt.Sprintf("Undo the last update (%s, %d packages, %s): the system goes back to snapshot %d, taken just before it, "+
		"at the next start. Your files in /home and the other data volumes stay as they are.", u.Proposal, u.Count, u.Time, u.Snapshot)
	return p, nil
}

func orDash(s string) string {
	if s == "" {
		return "new"
	}
	return s
}

func human(n int64) string {
	switch {
	// Decimal units, as the desktop shows them.
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%d kB", n/1000)
	}
	return fmt.Sprintf("%d B", n)
}

func uniq(xs []string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	if len(out) > max {
		out = append(out[:max], fmt.Sprintf("%d more", len(out)-max))
	}
	return out
}
