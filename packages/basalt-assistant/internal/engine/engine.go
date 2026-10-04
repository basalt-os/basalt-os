// Package engine is the event loop of basalt-assistantd. It follows the
// journal (unit failures, SELinux denials from the audit transport,
// ENOSPC messages, rpm audit records of failed package operations), polls
// disk usage and the snapshot list (a pre snapshot without its post is an
// unfinished dnf transaction), and turns each event into a diagnosis and a
// proposal with the same diagnosers the CLI uses. Proposals wait in the
// state directory until a person applies or ignores them; the daemon never
// changes the system.
package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/journal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// Event kinds.
const (
	KindUnit    = "unit"
	KindAVC     = "selinux"
	KindDisk    = "disk"
	KindDnf     = "dnf"
	KindNoSpace = "nospace"
)

// Event is one thing that happened.
type Event struct {
	Kind    string
	Subject string
	Time    time.Time
	AVC     *selinux.AVC
	Pre     int
}

// Engine holds the daemon's state.
type Engine struct {
	Cfg    config.Config
	Env    *diag.Env
	Store  proposal.Store
	Audit  *audit.Log
	Decide *decide.Layer
	Out    io.Writer

	mu        sync.Mutex
	created   []time.Time     // rate limit: proposal creation times
	limited   map[string]bool // rate-limit notices already logged
	batch     []Event
	timer     *time.Timer
	covered   map[string]time.Time // SELinux domains already covered by a unit proposal
	stats     map[string]int
	dnfMu     sync.Mutex // one dnf diagnosis at a time (several rpm records per transaction)
	dnfOffset int64      // how far dnf5.log has been read
	dnfSeen   bool
}

// New builds an engine.
func New(cfg config.Config, env *diag.Env, st proposal.Store, al *audit.Log, dl *decide.Layer, out io.Writer) *Engine {
	return &Engine{Cfg: cfg, Env: env, Store: st, Audit: al, Decide: dl, Out: out,
		limited: map[string]bool{}, covered: map[string]time.Time{}, stats: map[string]int{}}
}

func (g *Engine) logf(format string, args ...any) {
	fmt.Fprintf(g.Out, format+"\n", args...)
}

// Run starts the watchers and blocks until ctx ends.
func (g *Engine) Run(ctx context.Context) error {
	g.logf("basalt-assistantd: watching the journal, disk usage every %s, snapshots every minute (decision backend %s)",
		g.Cfg.DiskInterval, g.Decide.Backend.Name())
	g.initialScan(ctx)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); g.followJournal(ctx) }()
	go func() { defer wg.Done(); g.every(ctx, g.Cfg.DiskInterval, g.checkDisk) }()
	go func() { defer wg.Done(); g.every(ctx, time.Minute, g.checkDnf) }()
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (g *Engine) every(ctx context.Context, d time.Duration, f func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f(ctx)
		}
	}
}

// initialScan reports what is already wrong at start.
func (g *Engine) initialScan(ctx context.Context) {
	res := g.Env.R.Read(ctx, "systemctl", "list-units", "--failed", "--plain", "--no-legend", "--no-pager")
	for _, l := range strings.Split(res.Out, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			g.enqueue(Event{Kind: KindUnit, Subject: f[0], Time: time.Now()})
		}
	}
	g.checkDisk(ctx)
	g.checkDnf(ctx)
}

// --- journal ---------------------------------------------------------------------

func (g *Engine) cursorPath() string { return filepath.Join(g.Cfg.StateDir, "journal.cursor") }

func (g *Engine) followJournal(ctx context.Context) {
	for ctx.Err() == nil {
		args := []string{"--no-pager", "-o", "json", "-f"}
		if c := g.loadCursor(); c != "" {
			args = append(args, "--after-cursor", c)
		} else {
			args = append(args, "-n", "0")
		}
		cmd := exec.CommandContext(ctx, "journalctl", args...)
		cmd.Env = append(os.Environ(), "LANG=C", "SYSTEMD_COLORS=0")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			g.logf("journal follower: %v; retrying in 10s", err)
			sleep(ctx, 10*time.Second)
			continue
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var cursor string
		n := 0
		for sc.Scan() {
			e, ok := journal.ParseLine(sc.Bytes())
			if !ok {
				continue
			}
			cursor = e.Cursor
			g.onEntry(e)
			if n++; n%50 == 0 {
				g.saveCursor(cursor)
			}
		}
		g.saveCursor(cursor)
		_ = cmd.Wait()
		if ctx.Err() == nil {
			g.logf("journal follower exited; restarting in 5s")
			sleep(ctx, 5*time.Second)
		}
	}
}

// loadCursor returns the saved journal position, but only from this boot.
// The state directory is part of the root and rolls back with it: after a
// rollback the cursor (and the proposals) are older than the journal, and
// resuming there would report events that were already handled.
func (g *Engine) loadCursor() string {
	b, err := os.ReadFile(g.cursorPath())
	if err != nil {
		return ""
	}
	c := strings.TrimSpace(string(b))
	id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || !strings.Contains(c, "b="+strings.ReplaceAll(strings.TrimSpace(string(id)), "-", "")+";") {
		return ""
	}
	return c
}

func (g *Engine) saveCursor(c string) {
	if c == "" {
		return
	}
	tmp := g.cursorPath() + ".tmp"
	if os.WriteFile(tmp, []byte(c+"\n"), 0o600) == nil {
		_ = os.Rename(tmp, g.cursorPath())
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// onEntry classifies one journal record.
func (g *Engine) onEntry(e journal.Entry) {
	if e.Identifier == "basalt-assistant" || strings.HasPrefix(e.SystemUnit, "basalt-assistant") {
		return
	}
	switch {
	case e.IsAVC():
		if a, ok := selinux.ParseAVC(e.Message); ok {
			a.Time = e.Time
			g.enqueue(Event{Kind: KindAVC, Subject: a.Key(), Time: e.Time, AVC: &a})
		}
	case e.AuditType == journal.AuditSoftwareUpdate:
		if sw, ok := e.SoftwareUpdateFailed(); ok {
			g.logf("rpm reported a failed operation on %s", sw)
			at := e.Time
			go func() {
				// Let the transaction finish (post snapshot, log lines).
				time.Sleep(g.Cfg.DnfSettle)
				g.handleDnf(context.Background(), 0, at)
			}()
		}
	default:
		if u, ok := e.FailedUnit(); ok {
			g.enqueue(Event{Kind: KindUnit, Subject: u, Time: e.Time})
		} else if e.IsNoSpace() {
			g.enqueue(Event{Kind: KindNoSpace, Subject: "/", Time: e.Time})
		}
	}
}

// enqueue batches unit failures and denials for a short settle time, so a
// service that fails because of a denial gets one proposal (the unit's),
// not two.
func (g *Engine) enqueue(ev Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats["events_"+ev.Kind]++
	g.batch = append(g.batch, ev)
	if g.timer == nil {
		settle := max(g.Cfg.AVCSettle, g.Cfg.UnitSettle)
		g.timer = time.AfterFunc(settle, g.flush)
	}
}

func (g *Engine) flush() {
	g.mu.Lock()
	evs := g.batch
	g.batch, g.timer = nil, nil
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Units first, then denials not explained by a unit, then the rest.
	sort.SliceStable(evs, func(i, j int) bool { return order(evs[i].Kind) < order(evs[j].Kind) })
	seenUnit := map[string]bool{}
	var avcs []selinux.AVC
	for _, ev := range evs {
		switch ev.Kind {
		case KindUnit:
			if !seenUnit[ev.Subject] {
				seenUnit[ev.Subject] = true
				g.handleUnit(ctx, ev)
			}
		case KindAVC:
			avcs = append(avcs, *ev.AVC)
		case KindNoSpace:
			g.checkDisk(ctx)
		}
	}
	if len(avcs) > 0 {
		g.handleAVCs(ctx, avcs)
	}
}

func order(k string) int {
	switch k {
	case KindUnit:
		return 0
	case KindAVC:
		return 1
	}
	return 2
}

// --- handlers --------------------------------------------------------------------

// dedup folds a repeat into the open proposal for key (bumping its
// counter). A key whose proposal was ignored stays quiet for the dedup
// window; one that was applied is reported again (the fix did not hold).
func (g *Engine) dedup(key string) bool {
	if open := g.Store.FindOpen(key); open != nil {
		open.Seen++
		open.LastSeen = time.Now().UTC()
		_ = g.Store.Save(open)
		g.mu.Lock()
		g.stats["deduplicated"]++
		g.mu.Unlock()
		g.logf("%s: repeat of %s (seen %d times)", key, open.ID, open.Seen)
		return true
	}
	ps, _ := g.Store.List(proposal.Ignored)
	for _, p := range ps {
		if p.Key == key && time.Since(p.Updated) < g.Cfg.DedupWindow {
			g.mu.Lock()
			g.stats["deduplicated"]++
			g.mu.Unlock()
			return true
		}
	}
	return false
}

// allow applies the hourly rate limit.
func (g *Engine) allow(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	kept := g.created[:0]
	for _, t := range g.created {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	g.created = kept
	if len(g.created) >= g.Cfg.MaxPerHour {
		g.stats["rate_limited"]++
		if !g.limited[key] {
			g.limited[key] = true
			if g.Audit != nil {
				_, _ = g.Audit.Append("suppress", "rate limit reached; not reporting "+key,
					map[string]any{"key": key, "max_per_hour": g.Cfg.MaxPerHour})
			}
		}
		return false
	}
	g.created = append(g.created, time.Now())
	return true
}

// publish stores a proposal, records its decisions and logs a summary.
func (g *Engine) publish(ctx context.Context, p *proposal.Proposal, sevFeatures map[string]bool) {
	if !g.allow(p.Key) {
		return
	}
	// The daemon proposes no change that rests on a snapshot it cannot
	// check (the diagnosers already make those hints; this is the backstop).
	p.HoldForRoot("found by the confined daemon, which cannot check the snapshot")
	// Severity and notification are decisions too.
	sev := g.Decide.Ask(ctx, decide.Severity(p.Key, sevFeatures))
	ft := decide.SeverityFeatures(sev.Answer)
	notify := g.Decide.Ask(ctx, decide.Notify(p.Key, ft))
	route := g.Decide.Ask(ctx, decide.RouteBigger(p.Key, nil))
	p.Severity = sev.Answer.Expected()

	act := "review"
	if len(p.Actions) > 0 && !p.NeedsReview {
		act = "propose"
	}
	for i := range p.Decisions {
		p.Decisions[i] = g.Decide.Record(p.Decisions[i], act+" "+p.ID)
	}
	nAct := "list only"
	if notify.Answer.Top == "true" && notify.Confident {
		nAct = "notify"
	}
	sev = g.Decide.Record(sev, fmt.Sprintf("severity %.1f for %s", p.Severity, p.ID))
	notify = g.Decide.Record(notify, nAct+" "+p.ID)
	route = g.Decide.Record(route, "stay local (no bigger model configured)")
	p.Decisions = append(p.Decisions, sev, notify)
	_ = route

	if err := g.Store.Save(p); err != nil {
		g.logf("cannot save proposal %s: %v", p.ID, err)
		return
	}
	g.mu.Lock()
	g.stats["proposals"]++
	g.mu.Unlock()
	if g.Audit != nil {
		// basalt-notify reads these records from the journal: "notify" is the
		// decision layer's choice for desktop sessions and the webhook.
		_, _ = g.Audit.Append("finding", p.ID+": "+p.Title, map[string]any{"proposal": p.ID, "kind": p.Kind,
			"title": p.Title, "subject": p.Subject, "actions": p.Actions, "hints": p.Hints, "needs_review": p.NeedsReview,
			"severity": p.Severity, "notify": nAct == "notify"})
	}
	prefix := ""
	if nAct == "notify" {
		prefix = "<4>" // journal priority warning for the service's stdout
	}
	g.logf("%sproposal %s: %s (see: basalt pending, basalt show %s)", prefix, p.ID, p.Title, p.ID)
	g.logf("%s", report.Render(p))
}

func (g *Engine) handleUnit(ctx context.Context, ev Event) {
	rep, err := diag.WhyUnit(ctx, g.Env, ev.Subject)
	if err != nil {
		g.logf("diagnosing %s: %v", ev.Subject, err)
		return
	}
	if rep.Healthy {
		g.logf("%s failed but is running again; nothing to report", ev.Subject)
		return
	}
	if rep.Domain != "" && rep.Features["avc_for_domain"] {
		g.mu.Lock()
		g.covered[rep.Domain] = time.Now()
		g.mu.Unlock()
	}
	p := report.FromUnit("daemon", rep)
	if g.dedup(p.Key) {
		return
	}
	g.publish(ctx, p, map[string]bool{"unit_failed": true})
}

func (g *Engine) handleAVCs(ctx context.Context, avcs []selinux.AVC) {
	for _, grp := range selinux.GroupAVCs(avcs) {
		key := "avc:" + grp.AVC.Key()
		g.mu.Lock()
		t, cov := g.covered[grp.AVC.SType()]
		g.mu.Unlock()
		if cov && time.Since(t) < 10*time.Minute {
			g.logf("denial %s is part of a unit proposal", key)
			continue
		}
		if g.dedup(key) {
			continue
		}
		f := g.Env.AnalyzeAVC(ctx, grp)
		q := decide.AVCClass(grp.AVC.Key(), f.Features)
		g.Env.MarkView(&q)
		d := g.Decide.Ask(ctx, q)
		p := report.FromSELinux("daemon", diag.SELinuxItem{Fix: f, Decision: d})
		p.Seen = grp.Count
		g.publish(ctx, p, map[string]bool{"avc": true, "enforcing": !grp.AVC.Permissive, "suspicious": d.Answer.Top == selinux.ClassSuspicious})
	}
}

func (g *Engine) checkDisk(ctx context.Context) {
	st, err := g.Env.Statfs("/")
	if err != nil {
		return
	}
	g.Env.RecordSample(st)
	pct := st.UsedPct()
	fc := diag.Predict(g.Env.LoadSamples(), st)
	soon := fc.DaysToFull >= 0 && fc.DaysToFull < 2
	if pct < g.Cfg.Disk.WarnPct && !soon {
		// Back to normal: a pending disk proposal is stale.
		if p := g.Store.FindOpen("disk:/"); p != nil {
			p.Status = proposal.Resolved
			if g.Store.Save(p) == nil && g.Audit != nil {
				_, _ = g.Audit.Append("resolve", fmt.Sprintf("%s: / is %.0f %% full again; proposal closed", p.ID, pct), map[string]any{"proposal": p.ID})
			}
		}
		return
	}
	if g.dedup("disk:/") {
		return
	}
	rep, err := g.Env.Disk(ctx, g.Cfg.Disk, 30)
	if err != nil {
		return
	}
	p := report.FromDisk("daemon", rep)
	g.publish(ctx, p, map[string]bool{"disk_warn": rep.Features["disk_warn"], "disk_crit": rep.Features["disk_crit"]})
}

// checkDnf reports unfinished transactions (a dnf pre snapshot without its
// post, older than DnfMinAge: power loss, a killed dnf) and failures that
// dnf5 only writes to its log (a %post scriptlet failure leaves the package
// installed and rpm reports success).
func (g *Engine) checkDnf(ctx context.Context) {
	g.resolveUnits(ctx)
	for _, at := range g.newDnfFailures() {
		g.handleDnf(ctx, 0, at)
	}
	for _, s := range diag.OrphanPre(g.Env.Snapshots(ctx), time.Now(), g.Cfg.DnfMinAge) {
		if s.Userdata["basalt"] != "dnf" {
			continue // only dnf transactions (not basalt apply)
		}
		g.handleDnf(ctx, s.Number, s.Time)
	}
}

// resolveUnits closes pending unit proposals whose unit runs again (fixed
// by hand, or it recovered): their diagnosis is stale.
func (g *Engine) resolveUnits(ctx context.Context) {
	ps, _ := g.Store.List(proposal.Pending)
	for _, p := range ps {
		if p.Kind != "unit" || time.Since(p.LastSeen) < 30*time.Second {
			continue
		}
		if strings.TrimSpace(g.Env.R.Read(ctx, "systemctl", "is-active", p.Subject).Out) != "active" {
			continue
		}
		p.Status = proposal.Resolved
		if err := g.Store.Save(p); err != nil {
			continue
		}
		if g.Audit != nil {
			_, _ = g.Audit.Append("resolve", p.ID+": "+p.Subject+" is active again; proposal closed", map[string]any{"proposal": p.ID})
		}
		g.logf("%s: %s is active again; proposal closed", p.ID, p.Subject)
	}
}

// newDnfFailures reads dnf5.log from where the last call stopped and
// returns the times of failure lines.
func (g *Engine) newDnfFailures() []time.Time {
	f, err := os.Open(diag.DnfLog)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	g.mu.Lock()
	off, first := g.dnfOffset, !g.dnfSeen
	g.dnfSeen = true
	g.mu.Unlock()
	if first || st.Size() < off {
		// First look (or the log was rotated): start at the end.
		g.mu.Lock()
		g.dnfOffset = st.Size()
		g.mu.Unlock()
		if first {
			return nil
		}
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	var out []time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	read := off
	for sc.Scan() {
		read += int64(len(sc.Bytes())) + 1
		if t, ok := diag.DnfLogFailure(sc.Text()); ok {
			out = append(out, t)
		}
	}
	g.mu.Lock()
	g.dnfOffset = read
	g.mu.Unlock()
	return out
}

// handleDnf diagnoses one failed transaction (pre snapshot number, or 0 to
// find it from the failure time).
func (g *Engine) handleDnf(ctx context.Context, pre int, at time.Time) {
	g.dnfMu.Lock()
	defer g.dnfMu.Unlock()
	rep := g.Env.DiagnoseDnf(ctx, pre, at)
	key := "dnf:"
	if rep.Pre != nil {
		key += strconv.Itoa(rep.Pre.Number)
	} else {
		key += at.Format(time.RFC3339)
	}
	if g.dedup(key) || g.closed(key) {
		return
	}
	p := report.FromDnf("daemon", rep)
	p.Key = key
	g.publish(ctx, p, map[string]bool{"dnf_failed": true})
}

// closed reports a key whose proposal was applied or ignored already (a
// failed transaction stays orphaned forever; report it once).
func (g *Engine) closed(key string) bool {
	ps, _ := g.Store.List("")
	for _, p := range ps {
		if p.Key == key && p.Status != proposal.Pending {
			return true
		}
	}
	return false
}

// Stats is a snapshot of the counters.
func (g *Engine) Stats() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int{}
	for k, v := range g.stats {
		out[k] = v
	}
	return out
}
