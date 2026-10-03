package diag

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/journal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// UnitReport is the diagnosis of one unit.
type UnitReport struct {
	Unit        string            `json:"unit"`
	State       map[string]string `json:"state"`
	Healthy     bool              `json:"healthy"`
	Journal     []string          `json:"journal,omitempty"` // relevant lines
	Domain      string            `json:"domain,omitempty"`
	AVCs        []selinux.Fix     `json:"avcs,omitempty"`
	Deps        []DepState        `json:"deps,omitempty"`
	Ports       []PortInfo        `json:"ports,omitempty"`
	Disks       []FSStat          `json:"disks,omitempty"`
	ConfigCheck *CheckResult      `json:"config_check,omitempty"`
	ConfigFile  string            `json:"config_file,omitempty"`
	ConfigLine  int               `json:"config_line,omitempty"`
	Restore     *RestoreCandidate `json:"restore,omitempty"`
	Features    map[string]bool   `json:"features"`
	Decision    decide.Decision   `json:"decision"`
	AVCDecision []decide.Decision `json:"avc_decisions,omitempty"`
	Cause       string            `json:"cause"`
	Explanation string            `json:"explanation"`
	Evidence    []string          `json:"evidence"`
	Actions     []action.Action   `json:"actions,omitempty"`
	Skipped     []string          `json:"skipped,omitempty"` // probes left out (confined)
}

// DepState is a dependency's state.
type DepState struct {
	Unit   string `json:"unit"`
	Active string `json:"active"`
	Result string `json:"result"`
	Load   string `json:"load"`
}

// PortInfo is a port named in the failure and who holds it.
type PortInfo struct {
	Port  int    `json:"port"`
	Owner string `json:"owner,omitempty"`
}

// CheckResult is a config checker's outcome.
type CheckResult struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Output  string `json:"output"`
}

// RestoreCandidate is a snapshot copy of a broken config file.
type RestoreCandidate struct {
	Path     string `json:"path"`
	Snapshot int    `json:"snapshot"`
	Date     string `json:"date"`
	Diff     string `json:"diff,omitempty"`
	How      string `json:"how"` // content or size/mtime
}

// Config checkers for common services, keyed by unit name without suffix.
var checkers = map[string][]string{
	"nginx":     {"nginx", "-t"},
	"httpd":     {"httpd", "-t"},
	"sshd":      {"sshd", "-t"},
	"named":     {"named-checkconf"},
	"haproxy":   {"haproxy", "-c", "-f", "/etc/haproxy/haproxy.cfg"},
	"postfix":   {"postfix", "check"},
	"squid":     {"squid", "-k", "parse"},
	"unbound":   {"unbound-checkconf"},
	"smb":       {"testparm", "-s"},
	"dhcpd":     {"dhcpd", "-t", "-cf", "/etc/dhcp/dhcpd.conf"},
	"chronyd":   nil,
	"caddy":     {"caddy", "validate", "--config", "/etc/caddy/Caddyfile"},
	"dovecot":   {"doveconf", "-n"},
	"mosquitto": nil,
}

var (
	reConfigAt = regexp.MustCompile(`(?:in |at |file )?(/etc/[A-Za-z0-9._/@+-]+):(\d+)`)
	// A configuration error names a syntax problem or a file:line location.
	// "[emerg] bind() failed" or "test failed" alone are not config errors:
	// nginx -t also fails when it cannot bind its port.
	reConfigErr  = regexp.MustCompile(`(?i)syntax error|bad configuration|unknown directive|invalid (?:option|directive|value|parameter|number)|parse error|unexpected|AH00526|directive is not allowed|is duplicate|not terminated| in /etc/[^ :]+:\d+`)
	reAddrInUse  = regexp.MustCompile(`(?i)address already in use|EADDRINUSE|\(98:`)
	rePortInMsg  = regexp.MustCompile(`(?:bind\(\) to |listen(?:ing)? on |port[ =]|:)\[?[0-9a-fA-F.:]*\]?:?(\d{1,5})\b`)
	reBindPort   = regexp.MustCompile(`(?:bind\(\) to |listen )\S*?:(\d{1,5}) failed`)
	rePermDenied = regexp.MustCompile(`(?i)permission denied|\(13:|EACCES`)
	reNoSuchFile = regexp.MustCompile(`(?i)no such file or directory|\(2:`)
	reQuotedPath = regexp.MustCompile(`"(/[^"]+)"|'(/[^']+)'|\s(/[A-Za-z0-9._/@+-]+)`)
	reUnitName   = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+$`)
)

// NormalizeUnit adds ".service" when no suffix is given.
func NormalizeUnit(u string) (string, error) {
	if !reUnitName.MatchString(u) {
		return "", fmt.Errorf("invalid unit name %q", u)
	}
	for _, s := range []string{".service", ".socket", ".timer", ".mount", ".path", ".target"} {
		if strings.HasSuffix(u, s) {
			return u, nil
		}
	}
	return u + ".service", nil
}

// ParseShow reads `systemctl show` output (KEY=value lines; several units
// are separated by blank lines).
func ParseShow(out string) []map[string]string {
	var all []map[string]string
	cur := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				all = append(all, cur)
				cur = map[string]string{}
			}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			cur[k] = v
		}
	}
	if len(cur) > 0 {
		all = append(all, cur)
	}
	return all
}

var showProps = "Id,LoadState,ActiveState,SubState,Result,ExecMainStatus,ExecMainCode,FragmentPath,ExecStart,ExecStartPre," +
	"MainPID,NRestarts,StateChangeTimestamp,InactiveExitTimestamp,ActiveEnterTimestamp,Requires,Requisite,BindsTo,Wants,After,Description"

// WhyUnit diagnoses a unit.
func WhyUnit(ctx context.Context, e *Env, unit string) (*UnitReport, error) {
	unit, err := NormalizeUnit(unit)
	if err != nil {
		return nil, err
	}
	rep := &UnitReport{Unit: unit, Features: map[string]bool{}}
	res := e.R.Read(ctx, "systemctl", "show", "--timestamp=unix", "-p", showProps, unit)
	if res.Err != nil {
		return nil, fmt.Errorf("systemctl show %s: %v", unit, res.Err)
	}
	shows := ParseShow(res.Out)
	if len(shows) == 0 {
		return nil, fmt.Errorf("systemctl show %s: no output", unit)
	}
	st := shows[0]
	rep.State = st
	if st["LoadState"] == "not-found" {
		rep.Cause, rep.Explanation = "unknown", unit+" does not exist (LoadState=not-found)"
		rep.Evidence = append(rep.Evidence, "systemctl: LoadState=not-found")
		return rep, nil
	}
	rep.Evidence = append(rep.Evidence, fmt.Sprintf("state: %s/%s, result %s, main process %s status %s, restarts %s",
		st["ActiveState"], st["SubState"], st["Result"], st["ExecMainCode"], st["ExecMainStatus"], orDash(st["NRestarts"])))
	rep.Healthy = st["ActiveState"] == "active" && (st["Result"] == "success" || st["Result"] == "")
	if st["ActiveState"] == "failed" {
		rep.Features["unit_failed"] = true
	}
	if st["Result"] == "signal" || st["Result"] == "core-dump" || st["Result"] == "watchdog" {
		rep.Features["signal_or_core"] = true
	}

	// The window opens at the last start attempt: older failures of the
	// same unit (already fixed) must not count as evidence now.
	window := unixTS(st["InactiveExitTimestamp"]).Add(-5 * time.Second)
	if unixTS(st["InactiveExitTimestamp"]).IsZero() {
		window = unixTS(st["StateChangeTimestamp"]).Add(-1 * time.Minute)
	}
	if window.Year() < 2000 {
		window = e.now().Add(-1 * time.Hour)
	}

	e.unitJournal(ctx, rep, window)
	e.unitAVCs(ctx, rep, window)
	e.unitPathLabels(ctx, rep)
	e.unitDeps(ctx, rep)
	e.unitPorts(ctx, rep)
	e.unitDisks(ctx, rep)
	e.unitConfig(ctx, rep)

	d := e.ask(ctx, decide.UnitCause(unit, rep.Features))
	rep.Decision = d
	rep.Cause = d.Answer.Top
	e.unitPlan(rep)
	return rep, nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func unixTS(s string) time.Time {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	if s == "" || s == "0" || s == "n/a" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

func (e *Env) unitJournal(ctx context.Context, rep *UnitReport, window time.Time) {
	res := e.R.Read(ctx, "journalctl", "--no-pager", "-o", "json", "-u", rep.Unit,
		"--since", "@"+strconv.FormatInt(window.Unix(), 10), "-n", "400")
	entries := journal.ParseAll(res.Out)
	seen := map[string]bool{}
	for _, en := range entries {
		m := strings.TrimSpace(en.Message)
		if m == "" {
			continue
		}
		interesting := en.Priority <= 4 || reConfigErr.MatchString(m) || reAddrInUse.MatchString(m) ||
			rePermDenied.MatchString(m) || reNoSuchFile.MatchString(m) || en.IsNoSpace() ||
			strings.Contains(m, "Failed with result") || strings.Contains(m, "Dependency failed")
		if !interesting {
			continue
		}
		if reConfigErr.MatchString(m) {
			rep.Features["journal_config_error"] = true
		}
		if reAddrInUse.MatchString(m) {
			rep.Features["journal_address_in_use"] = true
		}
		if rePermDenied.MatchString(m) {
			rep.Features["journal_permission_denied"] = true
		}
		if reNoSuchFile.MatchString(m) && !reConfigErr.MatchString(m) {
			rep.Features["journal_no_such_file"] = true
		}
		if en.IsNoSpace() {
			rep.Features["journal_no_space"] = true
		}
		if strings.Contains(m, "Dependency failed") {
			rep.Features["dependency_failed"] = true
		}
		if mm := reConfigAt.FindStringSubmatch(m); mm != nil && rep.ConfigFile == "" && reConfigErr.MatchString(m) {
			rep.Features["journal_config_location"] = true
			rep.ConfigFile = mm[1]
			rep.ConfigLine, _ = strconv.Atoi(mm[2])
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		ts := ""
		if !en.Time.IsZero() {
			ts = en.Time.Format("15:04:05") + " "
		}
		rep.Journal = append(rep.Journal, ts+m)
	}
	if len(rep.Journal) > 25 {
		rep.Journal = rep.Journal[len(rep.Journal)-25:]
	}
}

// execPath returns the first ExecStart (or ExecStartPre) binary.
func execPath(st map[string]string) string {
	for _, k := range []string{"ExecStart", "ExecStartPre"} {
		v := st[k]
		if i := strings.Index(v, "path="); i >= 0 {
			rest := v[i+5:]
			if j := strings.IndexAny(rest, " ;"); j >= 0 {
				return rest[:j]
			}
			return rest
		}
	}
	return ""
}

// DomainOf guesses a service's SELinux domain from its executable's label
// (httpd_exec_t -> httpd_t).
func (e *Env) DomainOf(exe string) string {
	if exe == "" || e.Label == nil {
		return ""
	}
	t := selinux.Type(e.Label(exe))
	if strings.HasSuffix(t, "_exec_t") {
		return strings.TrimSuffix(t, "_exec_t") + "_t"
	}
	return ""
}

func (e *Env) unitAVCs(ctx context.Context, rep *UnitReport, window time.Time) {
	exe := execPath(rep.State)
	rep.Domain = e.DomainOf(exe)
	comm := path.Base(exe)
	avcs := e.CollectAVCs(ctx, window)
	var mine []selinux.AVC
	for _, a := range avcs {
		if (rep.Domain != "" && a.SType() == rep.Domain) || (comm != "" && comm != "." && a.Comm == comm) {
			mine = append(mine, a)
		}
	}
	if len(mine) == 0 {
		return
	}
	rep.Features["avc_for_domain"] = true
	for _, g := range selinux.GroupAVCs(mine) {
		f := e.AnalyzeAVC(ctx, g)
		if f.Features["resolved"] {
			rep.Evidence = append(rep.Evidence, "already allowed now: "+f.Explanation)
			continue
		}
		rep.AVCs = append(rep.AVCs, f)
	}
	if len(rep.AVCs) == 0 {
		delete(rep.Features, "avc_for_domain")
	}
}

// unitPathLabels checks the paths of "Permission denied" messages when no
// denial was logged for them (dontaudit rules hide many).
func (e *Env) unitPathLabels(ctx context.Context, rep *UnitReport) {
	if rep.Domain == "" || !rep.Features["journal_permission_denied"] || e.Label == nil {
		return
	}
	logged := map[string]bool{}
	for _, f := range rep.AVCs {
		if f.Path != "" {
			logged[f.Path] = true
		}
	}
	z := selinux.Analyzer{R: e.R}
	seen := map[string]bool{}
	for _, l := range rep.Journal {
		if !rePermDenied.MatchString(l) {
			continue
		}
		for _, m := range reQuotedPath.FindAllStringSubmatch(l, -1) {
			p := strings.TrimRight(m[1]+m[2]+m[3], ".,:;)")
			if seen[p] || logged[p] || !strings.HasPrefix(p, "/") {
				continue
			}
			seen[p] = true
			write := strings.HasSuffix(p, ".log") || strings.Contains(p, "/log") || strings.Contains(l, "open()")
			if f, ok := z.AnalyzePath(ctx, rep.Domain, p, write, e.Label); ok {
				rep.AVCs = append(rep.AVCs, f)
				rep.Features["path_label_problem"] = true
				if f.Features["default_differs"] && f.Features["default_allowed"] {
					rep.Features["path_mislabeled"] = true
				}
			}
		}
	}
}

// CollectAVCs reads denials from the journal (audit transport); outside the
// confined daemon it falls back to ausearch when the journal has none.
func (e *Env) CollectAVCs(ctx context.Context, since time.Time) []selinux.AVC {
	res := e.R.Read(ctx, "journalctl", "--no-pager", "-o", "json", "_TRANSPORT=audit",
		"--since", "@"+strconv.FormatInt(since.Unix(), 10))
	var out []selinux.AVC
	for _, en := range journal.ParseAll(res.Out) {
		if !en.IsAVC() {
			continue
		}
		if a, ok := selinux.ParseAVC(en.Message); ok {
			a.Time = en.Time
			out = append(out, a)
		}
	}
	if len(out) == 0 && !e.Confined {
		r := e.R.Read(ctx, "ausearch", "--input-logs", "-m", "AVC,USER_AVC", "-ts",
			since.Local().Format("01/02/2006"), since.Local().Format("15:04:05"))
		for _, line := range strings.Split(r.Out, "\n") {
			if a, ok := selinux.ParseAVC(line); ok {
				a.Time = auditTime(line)
				out = append(out, a)
			}
		}
	}
	return out
}

var reAuditTS = regexp.MustCompile(`audit\((\d+)\.(\d+):`)

func auditTime(line string) time.Time {
	if m := reAuditTS.FindStringSubmatch(line); m != nil {
		s, _ := strconv.ParseInt(m[1], 10, 64)
		return time.Unix(s, 0).UTC()
	}
	return time.Time{}
}

// AnalyzeAVC classifies one group and asks the decision layer.
func (e *Env) AnalyzeAVC(ctx context.Context, g selinux.Group) selinux.Fix {
	z := selinux.Analyzer{R: e.R, Resolve: e.resolveAVCPath}
	f := z.Analyze(ctx, g)
	return f
}

// resolveAVCPath finds the file an AVC names: candidate paths from recent
// journal messages that end in the AVC's name, checked by inode; outside the
// daemon, a bounded search by inode under the usual service directories.
func (e *Env) resolveAVCPath(ctx context.Context, a selinux.AVC) (string, string) {
	if a.Name == "" || a.Ino == 0 {
		return "", ""
	}
	since := a.Time.Add(-10 * time.Minute)
	if a.Time.IsZero() {
		since = e.now().Add(-1 * time.Hour)
	}
	res := e.R.Read(ctx, "journalctl", "--no-pager", "-o", "cat", "--since", "@"+strconv.FormatInt(since.Unix(), 10),
		"-p", "0..5", "-n", "2000")
	cands := map[string]bool{}
	for _, m := range reQuotedPath.FindAllStringSubmatch(res.Out, -1) {
		p := m[1] + m[2] + m[3]
		p = strings.TrimRight(p, ".,:;)")
		if path.Base(p) == a.Name || strings.HasSuffix(p, "/"+a.Name) {
			cands[p] = true
		} else if strings.Contains(p, "/"+a.Name+"/") {
			cands[p[:strings.Index(p, "/"+a.Name+"/")+len(a.Name)+1]] = true
		}
	}
	keys := make([]string, 0, len(cands))
	for k := range cands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		if ino, ok := e.Inode(p); ok && ino == a.Ino {
			return p, "journal message, inode match"
		}
	}
	if e.Confined {
		return "", ""
	}
	r := e.R.Read(ctx, "find", "/srv", "/var/www", "/var/lib", "/var/log", "/opt", "/home", "/etc", "/var/spool", "/var/cache", "/run",
		"-xdev", "-inum", strconv.FormatUint(a.Ino, 10), "-name", a.Name, "-print", "-quit")
	if p := strings.TrimSpace(firstLine(r.Out)); strings.HasPrefix(p, "/") {
		return p, "inode search"
	}
	// btrfs subvolumes are separate devices for -xdev: search each mount.
	for _, d := range []string{"/srv", "/var/log", "/home", "/var/lib/containers", "/var/lib/pgsql", "/var/lib/mysql", "/var/spool", "/var/cache", "/var/tmp"} {
		r := e.R.Read(ctx, "find", d, "-xdev", "-inum", strconv.FormatUint(a.Ino, 10), "-name", a.Name, "-print", "-quit")
		if p := strings.TrimSpace(firstLine(r.Out)); strings.HasPrefix(p, "/") {
			return p, "inode search"
		}
	}
	return "", ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (e *Env) unitDeps(ctx context.Context, rep *UnitReport) {
	var deps []string
	for _, k := range []string{"Requires", "Requisite", "BindsTo", "Wants"} {
		deps = append(deps, strings.Fields(rep.State[k])...)
	}
	if len(deps) == 0 {
		return
	}
	sort.Strings(deps)
	deps = dedupe(deps)
	res := e.R.Read(ctx, append([]string{"systemctl", "show", "-p", "Id,ActiveState,Result,LoadState"}, deps...)...)
	for _, d := range ParseShow(res.Out) {
		ds := DepState{Unit: d["Id"], Active: d["ActiveState"], Result: d["Result"], Load: d["LoadState"]}
		if ds.Active == "failed" || (ds.Result != "" && ds.Result != "success") || ds.Load == "not-found" && isHard(rep.State, ds.Unit) {
			rep.Deps = append(rep.Deps, ds)
			if isHard(rep.State, ds.Unit) {
				rep.Features["dependency_failed"] = true
			}
		}
	}
}

func isHard(st map[string]string, unit string) bool {
	for _, k := range []string{"Requires", "Requisite", "BindsTo"} {
		for _, u := range strings.Fields(st[k]) {
			if u == unit {
				return true
			}
		}
	}
	return false
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

var reSSUsers = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

func (e *Env) unitPorts(ctx context.Context, rep *UnitReport) {
	ports := map[int]bool{}
	for _, l := range rep.Journal {
		if !reAddrInUse.MatchString(l) {
			continue
		}
		if m := reBindPort.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			ports[n] = true
			continue
		}
		for _, m := range rePortInMsg.FindAllStringSubmatch(l, -1) {
			if n, _ := strconv.Atoi(m[1]); n > 0 && n < 65536 {
				ports[n] = true
			}
		}
	}
	for _, f := range rep.AVCs {
		if f.Group.AVC.IsPort() {
			ports[f.Group.AVC.Port] = true
		}
	}
	var keys []int
	for p := range ports {
		keys = append(keys, p)
	}
	sort.Ints(keys)
	for _, p := range keys {
		pi := PortInfo{Port: p}
		args := []string{"ss", "-H", "-ltnu"}
		if !e.Confined {
			args = []string{"ss", "-H", "-ltnup"}
		}
		res := e.R.Read(ctx, append(args, "sport = :"+strconv.Itoa(p))...)
		if strings.TrimSpace(res.Out) != "" {
			pi.Owner = "in use"
			if m := reSSUsers.FindStringSubmatch(res.Out); m != nil {
				pi.Owner = fmt.Sprintf("%s (pid %s)", m[1], m[2])
			}
			rep.Features["port_owner_found"] = true
		}
		rep.Ports = append(rep.Ports, pi)
	}
	if e.Confined && len(keys) > 0 {
		rep.Skipped = append(rep.Skipped, "process owning the port (needs `basalt why` as root)")
	}
}

func (e *Env) unitDisks(ctx context.Context, rep *UnitReport) {
	for _, p := range []string{"/", "/var/log", "/var/lib", "/srv"} {
		s, err := e.Statfs(p)
		if err != nil {
			continue
		}
		if s.UsedPct() >= 98 || s.Free < 64<<20 {
			rep.Features["disk_full"] = true
			rep.Disks = append(rep.Disks, s)
		}
	}
}

func (e *Env) unitConfig(ctx context.Context, rep *UnitReport) {
	name := strings.TrimSuffix(rep.Unit, path.Ext(rep.Unit))
	name, _, _ = strings.Cut(name, "@")
	argv, ok := checkers[name]
	if ok && argv != nil {
		if e.Confined {
			rep.Skipped = append(rep.Skipped, "config syntax check `"+strings.Join(argv, " ")+"` (run `basalt why "+name+"` as root)")
		} else {
			res := e.R.Read(ctx, argv...)
			if res.Err == nil {
				cr := &CheckResult{Command: strings.Join(argv, " "), OK: res.Code == 0, Output: trimLines(res.Out, 12)}
				rep.ConfigCheck = cr
				if cr.OK {
					rep.Features["config_check_passed"] = true
				} else if reConfigErr.MatchString(res.Out) {
					rep.Features["config_check_failed"] = true
					if mm := reConfigAt.FindStringSubmatch(res.Out); mm != nil {
						rep.ConfigFile = mm[1]
						rep.ConfigLine, _ = strconv.Atoi(mm[2])
					}
				} else {
					// The checker failed for a runtime reason (a port it
					// could not bind, a file it could not open): not syntax.
					rep.Features["config_check_runtime"] = true
				}
			}
		}
	}
	if rep.ConfigFile != "" && (rep.Features["config_check_failed"] || rep.Features["journal_config_error"]) {
		rep.Restore = e.findRestore(ctx, rep.ConfigFile, unixTS(rep.State["ActiveEnterTimestamp"]))
	}
}

func trimLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// findRestore looks for the newest snapshot copy of a config file that is
// different from the current one and was already in place the last time
// the unit started successfully (its modification time is not newer than
// ActiveEnterTimestamp): that copy is known to have worked. Outside the
// daemon the contents are compared and a diff is shown; the confined
// daemon compares metadata only (it may not read every service's config).
func (e *Env) findRestore(ctx context.Context, file string, lastGood time.Time) *RestoreCandidate {
	snaps := e.Snapshots(ctx)
	cur, curErr := []byte(nil), error(nil)
	if !e.Confined {
		cur, curErr = e.ReadFile(file)
	}
	curMeta := strings.TrimSpace(e.R.Read(ctx, "stat", "-c", "%s %Y", file).Out)
	for i := len(snaps) - 1; i >= 0; i-- {
		s := snaps[i]
		if s.Number == 0 {
			continue
		}
		cand := fmt.Sprintf("%s/%d/snapshot%s", e.SnapshotDir, s.Number, file)
		meta := e.R.Read(ctx, "stat", "-c", "%s %Y", cand)
		if meta.Code != 0 || strings.TrimSpace(meta.Out) == curMeta {
			continue
		}
		f := strings.Fields(meta.Out)
		if len(f) == 2 && !lastGood.IsZero() {
			if mt, err := strconv.ParseInt(f[1], 10, 64); err == nil && time.Unix(mt, 0).After(lastGood) {
				continue // changed after the last good start: not known to work
			}
		}
		rc := &RestoreCandidate{Path: file, Snapshot: s.Number, Date: s.Date, How: "size/mtime differ"}
		if !lastGood.IsZero() {
			rc.How += ", in place at the last successful start"
		}
		if e.Confined || curErr != nil {
			return rc
		}
		old, err := e.ReadFile(cand)
		if err != nil || bytes.Equal(old, cur) {
			continue
		}
		rc.Diff = trimLines(e.R.Read(ctx, "diff", "-u", cand, file).Out, 30)
		rc.How = strings.Replace(rc.How, "size/mtime differ", "content differs", 1)
		return rc
	}
	return nil
}

// unitPlan turns the cause into an explanation and actions.
func (e *Env) unitPlan(rep *UnitReport) {
	u := rep.Unit
	switch rep.Cause {
	case "config_error":
		where := "its configuration"
		if rep.ConfigFile != "" {
			where = rep.ConfigFile
			if rep.ConfigLine > 0 {
				where += fmt.Sprintf(" line %d", rep.ConfigLine)
			}
		}
		rep.Explanation = u + " does not start because of an error in " + where + "."
		if rep.Restore != nil {
			rep.Explanation += fmt.Sprintf(" Snapshot %d (%s) has a different copy of %s (%s); restoring it and restarting is proposed.",
				rep.Restore.Snapshot, rep.Restore.Date, rep.Restore.Path, rep.Restore.How)
			rep.Actions = []action.Action{
				{Kind: action.FileRestore, Params: map[string]string{"path": rep.Restore.Path, "snapshot": strconv.Itoa(rep.Restore.Snapshot)}},
				{Kind: action.UnitRestart, Params: map[string]string{"unit": u}},
			}
		} else {
			rep.Explanation += " No snapshot holds another version of the file; fix it by hand, then restart the unit."
		}
	case "selinux_denial":
		rep.Explanation = u + " was stopped by SELinux denials for its domain " + orDash(rep.Domain) + "."
		var acts []action.Action
		for _, f := range rep.AVCs {
			d := e.ask(context.Background(), decide.AVCClass(f.Group.AVC.Key(), f.Features))
			rep.AVCDecision = append(rep.AVCDecision, d)
			if d.Confident && len(f.Actions) > 0 {
				acts = append(acts, f.Actions...)
				rep.Explanation += " " + f.Explanation + "."
			} else {
				rep.Explanation += " A denial without a confident fix needs review: " + f.Explanation + "."
			}
		}
		if len(acts) > 0 {
			rep.Actions = append(acts, action.Action{Kind: action.UnitRestart, Params: map[string]string{"unit": u}})
		}
	case "port_conflict":
		rep.Explanation = u + " cannot bind its port: another process holds it."
		for _, p := range rep.Ports {
			if p.Owner != "" {
				rep.Explanation += fmt.Sprintf(" Port %d: %s.", p.Port, p.Owner)
			}
		}
		rep.Explanation += " Stop the other service or change the port; nothing is changed automatically."
	case "dependency_failed":
		rep.Explanation = u + " did not start because a unit it requires failed:"
		for _, d := range rep.Deps {
			rep.Explanation += fmt.Sprintf(" %s (%s, %s); diagnose it with `basalt why %s`.", d.Unit, d.Active, d.Result, d.Unit)
		}
	case "disk_full":
		rep.Explanation = u + " failed while a file system is full; see `basalt disk` for what holds the space."
	case "missing_file":
		rep.Explanation = u + " refers to a file that does not exist; see the journal lines."
	case "crashed":
		rep.Explanation = u + " was killed (" + rep.State["Result"] + "); a restart is proposed, but the crash itself needs review."
		rep.Actions = []action.Action{{Kind: action.UnitRestart, Params: map[string]string{"unit": u}}}
	default:
		if rep.Healthy {
			rep.Explanation = u + " is running normally."
		} else {
			rep.Explanation = u + ": no known cause found; read the journal lines below."
		}
	}
	if !rep.Decision.Confident && !rep.Healthy {
		rep.Explanation += fmt.Sprintf(" (Confidence %.2f is below the threshold %.2f: review before applying.)",
			rep.Decision.Answer.Confidence, rep.Decision.Threshold)
	}
	if rep.Healthy && rep.Cause != "unknown" && !rep.Features["unit_failed"] {
		// A running unit gets no change proposed.
		rep.Actions = nil
	}
}
