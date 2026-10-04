package diag

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/selinux"
)

// --- the service's SELinux domain --------------------------------------------

// Programs whose name says nothing about the service (a unit that runs a
// shell or an interpreter): their command name must not attribute denials
// of other processes to the unit.
var genericComm = map[string]bool{
	"sh": true, "bash": true, "dash": true, "zsh": true, "env": true, "python": true, "python3": true,
	"perl": true, "ruby": true, "node": true, "java": true, "php": true, "busybox": true, "sudo": true,
}

// unitDomain finds the domain the unit runs in: SELinuxContext= of the
// unit, else the loaded policy's transition from init_t for the label of
// the executable. Guessing from the label's name (shell_exec_t -> shell_t)
// blamed SELinux for file-mode errors of units that run a shell: those run
// in unconfined_service_t, which SELinux does not stop. The policy is only
// queried when lookup is set (the caller needs the domain).
func (e *Env) unitDomain(ctx context.Context, rep *UnitReport, lookup bool) {
	if c := strings.TrimPrefix(strings.TrimSpace(rep.State["SELinuxContext"]), "-"); c != "" {
		if t := selinux.Type(c); t != "" {
			rep.Domain, rep.DomainHow = t, "SELinuxContext= of the unit"
			return
		}
	}
	exe := execPath(rep.State)
	if !lookup || exe == "" || e.Label == nil {
		return
	}
	et := selinux.Type(e.Label(exe))
	if et == "" {
		return
	}
	dom, err := e.policy().ServiceDomain(ctx, et)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		rep.Features["policy_query_failed"] = true
		return
	}
	if dom == "" {
		rep.DomainHow = fmt.Sprintf("the policy has no transition from init_t for %s (%s)", exe, et)
		return
	}
	rep.Domain = dom
	rep.DomainHow = fmt.Sprintf("policy: init_t starts %s (%s) in %s", exe, et, dom)
	if selinux.Unconfined(dom) {
		rep.DomainHow += ", which SELinux does not confine"
	}
}

// --- file permissions (DAC) ----------------------------------------------------

// DACFinding is a path a service may not use because of its mode and owner.
type DACFinding struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	Owner       string `json:"owner"`
	User        string `json:"user"`
	Need        string `json:"need"`
	Explanation string `json:"explanation"`
}

type cred struct {
	name   string
	uid    int
	groups map[int]bool
}

type fileMode struct {
	mode     uint32
	uid, gid int
	owner    string
	dir      bool
}

// may reports whether the credentials grant need (bits of rwx = 4 2 1).
func (c cred) may(st fileMode, need uint32) bool {
	bits := st.mode & 0o777
	switch {
	case c.uid == st.uid:
		bits >>= 6
	case c.groups[st.gid]:
		bits >>= 3
	}
	return bits&need == need
}

func needName(need uint32, dir bool) string {
	switch {
	case dir && need == 1:
		return "search"
	case dir && need == 3:
		return "create files in"
	case need == 2:
		return "write"
	}
	return "read"
}

// Users and groups are read from /etc/passwd and /etc/group first: the
// tools (id, getent, stat %U) go through NSS, whose systemd module reads
// systemd-userdbd's runtime directory, which the confined domain may not
// (a denial of its own each time). `id` and `getent` are asked only for a
// user or group the files do not have (LDAP, a DynamicUser= unit is not
// checked at all).
type nssFiles struct {
	users  map[string][2]int // name -> uid, gid
	names  map[int]string    // uid -> name
	groups map[string]int    // name -> gid
	gnames map[int]string    // gid -> name
	member map[string][]int  // user -> supplementary gids
}

func (e *Env) nss() nssFiles {
	n := nssFiles{users: map[string][2]int{}, names: map[int]string{}, groups: map[string]int{},
		gnames: map[int]string{}, member: map[string][]int{}}
	if e.ReadFile == nil {
		return n
	}
	if b, err := e.ReadFile("/etc/passwd"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Split(l, ":")
			if len(f) < 4 {
				continue
			}
			uid, err1 := strconv.Atoi(f[2])
			gid, err2 := strconv.Atoi(f[3])
			if err1 == nil && err2 == nil && f[0] != "" {
				n.users[f[0]] = [2]int{uid, gid}
				if _, ok := n.names[uid]; !ok {
					n.names[uid] = f[0]
				}
			}
		}
	}
	if b, err := e.ReadFile("/etc/group"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Split(l, ":")
			if len(f) < 4 {
				continue
			}
			gid, err := strconv.Atoi(f[2])
			if err != nil || f[0] == "" {
				continue
			}
			n.groups[f[0]] = gid
			if _, ok := n.gnames[gid]; !ok {
				n.gnames[gid] = f[0]
			}
			for _, m := range strings.Split(f[3], ",") {
				if m = strings.TrimSpace(m); m != "" {
					n.member[m] = append(n.member[m], gid)
				}
			}
		}
	}
	return n
}

// userCred resolves the unit's User= (and Group=) to ids: the local files,
// else `id` and `getent` (read-only).
func (e *Env) userCred(ctx context.Context, user, group string) (cred, bool) {
	n := e.nss()
	c := cred{name: user, groups: map[int]bool{}}
	if u, ok := n.users[user]; ok {
		c.uid = u[0]
		c.groups[u[1]] = true
		for _, g := range n.member[user] {
			c.groups[g] = true
		}
	} else {
		r := e.R.Read(ctx, "id", "-u", user)
		uid, err := strconv.Atoi(strings.TrimSpace(r.Out))
		if r.Err != nil || r.Code != 0 || err != nil {
			return cred{}, false
		}
		c.uid = uid
		for _, g := range strings.Fields(e.R.Read(ctx, "id", "-G", user).Out) {
			if n, err := strconv.Atoi(g); err == nil {
				c.groups[n] = true
			}
		}
	}
	if group != "" {
		if g, err := strconv.Atoi(group); err == nil {
			c.groups[g] = true
		} else if g, ok := n.groups[group]; ok {
			c.groups[g] = true
		} else if f := strings.Split(strings.TrimSpace(e.R.Read(ctx, "getent", "group", group).Out), ":"); len(f) >= 3 {
			if g, err := strconv.Atoi(f[2]); err == nil {
				c.groups[g] = true
			}
		}
	}
	return c, true
}

// statMode reads a file's mode and owner with stat (%f is the raw mode in
// hex); owner names come from the local files (numbers otherwise), not
// from stat's %U %G (NSS, see nssFiles).
func (e *Env) statMode(ctx context.Context, p string) (fileMode, bool) {
	r := e.R.Read(ctx, "stat", "-c", "%f %u %g", p)
	f := strings.Fields(r.Out)
	if r.Err != nil || r.Code != 0 || len(f) != 3 {
		return fileMode{}, false
	}
	raw, err := strconv.ParseUint(f[0], 16, 32)
	if err != nil {
		return fileMode{}, false
	}
	uid, err1 := strconv.Atoi(f[1])
	gid, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return fileMode{}, false
	}
	n := e.nss()
	un, gn := n.names[uid], n.gnames[gid]
	if un == "" {
		un = f[1]
	}
	if gn == "" {
		gn = f[2]
	}
	return fileMode{mode: uint32(raw), uid: uid, gid: gid, owner: un + ":" + gn, dir: raw&0o170000 == 0o040000}, true
}

// dacCheck walks the components of p the way the kernel does for the unit's
// user: every directory on the way needs search, the leaf read (or write);
// a missing leaf needs write and search on its directory. The first
// component that refuses is the finding. Root (or a unit without User=) is
// not checked: CAP_DAC_OVERRIDE passes file modes.
func (e *Env) dacCheck(ctx context.Context, rep *UnitReport, p string, write bool) (DACFinding, bool) {
	user := strings.TrimSpace(rep.State["User"])
	if user == "" || user == "root" || user == "0" || rep.State["DynamicUser"] == "yes" || !selinux.ValidPath(p) {
		return DACFinding{}, false
	}
	c, ok := e.userCred(ctx, user, strings.TrimSpace(rep.State["Group"]))
	if !ok || c.uid == 0 {
		return DACFinding{}, false
	}
	var comps []string
	for x := path.Clean(p); x != "/"; x = path.Dir(x) {
		comps = append([]string{x}, comps...)
	}
	var parent fileMode
	for i, comp := range comps {
		st, ok := e.statMode(ctx, comp)
		if !ok {
			if i > 0 && write && !c.may(parent, 3) {
				return e.dacFinding(c, comps[i-1], parent, 3), true
			}
			return DACFinding{}, false
		}
		leaf := i == len(comps)-1
		var need uint32 = 1
		if leaf && !st.dir {
			need = 4
			if write {
				need = 2
			}
		}
		if !c.may(st, need) {
			return e.dacFinding(c, comp, st, need), true
		}
		parent = st
	}
	return DACFinding{}, false
}

func (e *Env) dacFinding(c cred, p string, st fileMode, need uint32) DACFinding {
	d := DACFinding{Path: p, Mode: fmt.Sprintf("%04o", st.mode&0o7777), Owner: st.owner,
		User: fmt.Sprintf("%s (uid %d)", c.name, c.uid), Need: needName(need, st.dir)}
	kind := "file"
	if st.dir {
		kind = "directory"
	}
	d.Explanation = fmt.Sprintf("the %s %s is mode %s, owner %s; the service runs as %s, which may not %s it",
		kind, p, d.Mode, d.Owner, d.User, d.Need)
	return d
}

// unitPermissions explains "Permission denied" messages: file modes first
// (DAC), then, for a confined domain, the labels of every component of the
// path when no denial was logged for it (dontaudit rules hide many).
func (e *Env) unitPermissions(ctx context.Context, rep *UnitReport) {
	if !rep.Features["journal_permission_denied"] {
		return
	}
	logged := map[string]bool{}
	for _, f := range rep.AVCs {
		if f.Path != "" {
			logged[f.Path] = true
		}
	}
	z := e.analyzer()
	labels := rep.Domain != "" && !selinux.Unconfined(rep.Domain) && e.Label != nil
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
			if d, ok := e.dacCheck(ctx, rep, p, write); ok {
				rep.DAC = append(rep.DAC, d)
				rep.Features["dac_denied"] = true
				rep.Evidence = append(rep.Evidence, "file permissions: "+d.Explanation+" (DAC, not SELinux)")
				continue
			}
			if !labels {
				continue
			}
			if f, ok := z.AnalyzePath(ctx, rep.Domain, p, write, e.Label); ok {
				if len(f.Errors) > 0 {
					rep.Errors = append(rep.Errors, f.Errors...)
					rep.Features["policy_query_failed"] = true
				}
				rep.AVCs = append(rep.AVCs, f)
				rep.Features["path_label_problem"] = true
				if f.Features["default_differs"] && f.Features["default_allowed"] {
					rep.Features["path_mislabeled"] = true
				}
			}
		}
	}
	if rep.Domain != "" && selinux.Unconfined(rep.Domain) && !rep.Features["dac_denied"] {
		rep.Evidence = append(rep.Evidence, "the unit runs in "+rep.Domain+", which SELinux does not confine: its \"Permission denied\" does not come from a label")
	}
}

// --- out of memory -------------------------------------------------------------

// OOMInfo is what shows an out-of-memory kill of the unit.
type OOMInfo struct {
	Kernel     []string          `json:"kernel,omitempty"`     // the kernel's oom-kill lines for the unit
	Constraint string            `json:"constraint,omitempty"` // CONSTRAINT_MEMCG (a cgroup limit) or CONSTRAINT_NONE (the whole system)
	Limits     map[string]string `json:"limits"`
}

var (
	reOOM        = regexp.MustCompile(`(?i)killed by the OOM killer|result 'oom-kill'|oom-kill(?:ed)?\b|out of memory: killed process|memory cgroup out of memory`)
	reOOMKill    = regexp.MustCompile(`oom-kill:constraint=(\w+)\b.*\btask_memcg=([^,\s]+)`)
	reKilledProc = regexp.MustCompile(`(?i)(?:out of memory|memory cgroup out of memory): Killed process (\d+) \(([^)]*)\)`)
)

var memoryProps = []string{"MemoryMax", "MemoryHigh", "MemorySwapMax", "MemoryPeak", "MemoryCurrent", "OOMPolicy"}

// unitOOM looks for an out-of-memory kill of the unit: systemd's result
// (oom-kill) and messages, and the kernel's report, which names the cgroup
// of the killed task and whether a cgroup limit (CONSTRAINT_MEMCG) or the
// whole system ran out.
func (e *Env) unitOOM(ctx context.Context, rep *UnitReport, window time.Time) {
	oom := rep.Features["oom_killed"]
	info := &OOMInfo{Limits: map[string]string{}}
	res := e.R.Read(ctx, "journalctl", "--no-pager", "-o", "cat", "-k", "--since", "@"+strconv.FormatInt(window.Unix(), 10), "-n", "1000")
	pid := strings.TrimSpace(rep.State["ExecMainPID"])
	for _, l := range strings.Split(res.Out, "\n") {
		l = strings.TrimSpace(l)
		if m := reOOMKill.FindStringSubmatch(l); m != nil && strings.HasSuffix(m[2], "/"+rep.Unit) {
			oom = true
			info.Constraint = m[1]
			info.Kernel = append(info.Kernel, l)
			continue
		}
		if m := reKilledProc.FindStringSubmatch(l); m != nil && pid != "" && pid != "0" && m[1] == pid {
			oom = true
			info.Kernel = append(info.Kernel, l)
		}
	}
	if !oom {
		return
	}
	rep.Features["oom_killed"] = true
	for _, k := range memoryProps {
		if v := strings.TrimSpace(rep.State[k]); v != "" && v != "[not set]" {
			info.Limits[k] = v
		}
	}
	if len(info.Kernel) > 3 {
		info.Kernel = info.Kernel[len(info.Kernel)-3:]
	}
	rep.OOM = info
	var lim []string
	for _, k := range memoryProps {
		if v, ok := info.Limits[k]; ok {
			lim = append(lim, k+"="+memValue(v))
		}
	}
	rep.Evidence = append(rep.Evidence, "killed by the out-of-memory killer; memory settings: "+orDash(strings.Join(lim, ", ")))
	for _, l := range info.Kernel {
		rep.Evidence = append(rep.Evidence, "kernel: "+l)
	}
}

// memValue renders a byte count from systemctl show in binary units.
// MemValue renders a systemd memory property (bytes) in binary units;
// other values (infinity) are returned as they are.
func MemValue(v string) string { return memValue(v) }

func memValue(v string) string {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return v // infinity, a policy name
	}
	return HumanBytes(n)
}

// oomLimit describes the limit that was hit, for the explanation.
func oomLimit(o *OOMInfo) string {
	if o == nil {
		return ""
	}
	max, ok := o.Limits["MemoryMax"]
	switch {
	case o.Constraint == "CONSTRAINT_MEMCG" && ok && max != "infinity":
		return " at its memory limit (MemoryMax=" + memValue(max) + ")"
	case o.Constraint == "CONSTRAINT_MEMCG":
		return " at a memory limit of its cgroup or slice"
	case o.Constraint == "CONSTRAINT_NONE":
		return " when the whole system ran out of memory"
	case ok && max != "infinity":
		return " (its limit is MemoryMax=" + memValue(max) + ")"
	}
	return ""
}

// --- web document roots ------------------------------------------------------------

// Usual document roots: a denied page is often below one of them.
var webRoots = []string{"/var/www", "/srv/www", "/srv/http", "/usr/share/nginx/html", "/usr/share/httpd", "/var/lib/nginx"}

var (
	reNginxRoot = regexp.MustCompile(`(?m)(?:^|[\s{;])(?:root|alias)\s+"?(/[^;"\s]+)"?\s*;`)
	reHttpdRoot = regexp.MustCompile(`(?mi)^\s*(?:DocumentRoot\s+|<Directory\s+)"?(/[^">\s]+)"?`)
)

// docRoots lists the document roots the web servers' configurations name
// (nginx root and alias, httpd DocumentRoot and Directory), then the usual
// ones. Reading service configurations needs the root view.
func (e *Env) docRoots() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = path.Clean(p)
		if p == "/" || seen[p] || !selinux.ValidPath(p) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	read := func(files []string, re *regexp.Regexp) {
		sort.Strings(files)
		for _, f := range files {
			b, err := e.ReadFile(f)
			if err != nil {
				continue
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				add(m[1])
			}
		}
	}
	if e.Glob != nil {
		nginx := []string{"/etc/nginx/nginx.conf"}
		for _, g := range []string{"/etc/nginx/conf.d/*.conf", "/etc/nginx/default.d/*.conf", "/etc/nginx/sites-enabled/*"} {
			nginx = append(nginx, e.Glob(g)...)
		}
		read(nginx, reNginxRoot)
		httpd := []string{"/etc/httpd/conf/httpd.conf"}
		httpd = append(httpd, e.Glob("/etc/httpd/conf.d/*.conf")...)
		read(httpd, reHttpdRoot)
	}
	for _, p := range webRoots {
		add(p)
	}
	return out
}
