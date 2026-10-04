package selinux

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// Fix classes, the options of the decision "avc.class".
const (
	ClassMislabeled      = "mislabeled"       // the file has a label other than its policy default; restorecon
	ClassMissingFcontext = "missing_fcontext" // no rule labels this place for the service; semanage fcontext
	ClassPort            = "port"             // the port is not labeled for the service; semanage port
	ClassBoolean         = "boolean"          // a boolean that allows it is off; setsebool
	ClassUnknown         = "unknown"          // no known fix: review, never generate policy blindly
	ClassSuspicious      = "suspicious"       // looks like something the policy is right to stop
)

// Classes in a stable order.
var Classes = []string{ClassMislabeled, ClassMissingFcontext, ClassPort, ClassBoolean, ClassUnknown, ClassSuspicious}

// Fix is the analysis of one group of denials.
type Fix struct {
	Group       Group             `json:"group"`
	Class       string            `json:"class"` // the rule-based classification (decision input)
	Features    map[string]bool   `json:"features"`
	Path        string            `json:"path,omitempty"`
	PathFrom    string            `json:"path_from,omitempty"`
	DefaultType string            `json:"default_type,omitempty"`
	Explanation string            `json:"explanation"`
	Evidence    []string          `json:"evidence"`
	Actions     []action.Action   `json:"actions,omitempty"`
	Facts       map[string]string `json:"facts,omitempty"`
	// Errors are policy queries that failed (after retries): the analysis
	// has no conclusion and proposes nothing.
	Errors []string `json:"errors,omitempty"`
}

// PathResolver finds the file an AVC refers to (AVC records carry only the
// last path component and an inode number).
type PathResolver func(ctx context.Context, a AVC) (path, how string)

// Analyzer maps denials to fixes using the loaded policy (sesearch, seinfo,
// matchpathcon, getsebool from setools-console and libselinux-utils).
// Policy queries go through Policy (retries when another process holds the
// policy file, short cache); one is made from R when it is nil.
type Analyzer struct {
	R       runner.Reader
	Resolve PathResolver
	Policy  *Policy
	errs    *[]string
}

// begin prepares one analysis: its own error list, a policy reader.
func (z Analyzer) begin() Analyzer {
	if z.Policy == nil {
		z.Policy = NewPolicy(z.R)
	}
	z.errs = &[]string{}
	return z
}

// prefetch answers the queries an analysis is about to make in one walk of
// the policy (see Policy.Prefetch).
func (z Analyzer) prefetch(ctx context.Context, queries ...[]string) {
	if z.Policy != nil && len(queries) > 0 {
		z.Policy.Prefetch(ctx, queries...)
	}
}

// PrefetchGroups answers, in one walk of the policy, the queries the
// analysis of these groups will need that are known before it starts: the
// port label and the rules of each port denial; the rules for the current
// label, the policy default (when the audit record names the path) and the
// candidate types of each file denial.
func (z Analyzer) PrefetchGroups(ctx context.Context, groups []Group) {
	z = z.begin()
	var qs [][]string
	for _, g := range groups {
		a := g.AVC
		switch {
		case a.SType() == "" || len(a.Perms) == 0:
		case a.IsPort():
			qs = append(qs, portQueries(a)...)
		case a.IsFile():
			def := ""
			if a.Path != "" && ValidPath(a.Path) {
				def = z.defaultType(ctx, a.Path)
			}
			qs = append(qs, fileQueries(a, a.Path, def)...)
		}
	}
	z.prefetch(ctx, qs...)
}

// portQueries are the queries analyzePort makes when the port still has the
// label the denial recorded.
func portQueries(a AVC) [][]string {
	dom, perm := a.SType(), "name_bind"
	if hasAny(a.Perms, "name_connect") {
		perm = "name_connect"
	}
	return [][]string{PortconQuery(a.Port), AllowQuery(dom, a.TType(), a.Class, perm), AllowQuery(dom, "", a.Class, perm)}
}

// filePerm is the permission a file denial is analyzed for: the first
// write permission, else the first one.
func filePerm(a AVC) (perm string, write bool) {
	perm = a.Perms[0]
	for _, x := range a.Perms {
		if writePerms[x] {
			write, perm = true, x
		}
	}
	return perm, write
}

// fileQueries are the queries analyzeFile may make for the object at p
// (empty: not known yet) whose policy default is def.
func fileQueries(a AVC, p, def string) [][]string {
	dom, tgt := a.SType(), a.TType()
	perm, write := filePerm(a)
	qs := [][]string{AllowQuery(dom, tgt, a.Class, perm)}
	if def != "" && def != tgt {
		qs = append(qs, AllowQuery(dom, def, a.Class, perm))
	}
	if p == "" {
		p = a.Name
	}
	if t := preferredType(dom, p, write); t != "" {
		qs = append(qs, AllowQuery(dom, t, a.Class, perm))
	}
	return append(qs, AllowQuery(dom, "", a.Class, perm))
}

// preferredType is the type candidateType tries first for a service with
// known types.
func preferredType(dom, p string, write bool) string {
	ts, ok := serviceTypes[dom]
	if !ok {
		return ""
	}
	isLog := strings.Contains(p, "/log") || strings.HasSuffix(p, ".log")
	switch {
	case isLog && write:
		return ts.log
	case write:
		return ts.rw
	}
	return ts.read
}

func (z Analyzer) fail(err error) {
	if z.errs != nil {
		*z.errs = append(*z.errs, err.Error())
	}
}

// finishErrors turns failed policy queries into "no conclusion": a query
// that could not be answered is never read as "no rule allows it".
func (z Analyzer) finishErrors(f *Fix) {
	if z.errs == nil || len(*z.errs) == 0 {
		return
	}
	f.Errors = append(f.Errors, *z.errs...)
	f.Features["policy_query_failed"] = true
	f.Actions = nil
	f.Class = ClassUnknown
	f.Explanation = "the SELinux policy could not be queried (" + (*z.errs)[0] + "); no conclusion was drawn and nothing is proposed: run the diagnosis again"
	f.Evidence = append(f.Evidence, "policy query failed: "+strings.Join(*z.errs, "; "))
}

// ValidPath reports a path the action validators accept: absolute, clean,
// from the allowed character set. Anything else (an error message a tool
// printed where a path was expected) is never used as a denied object.
func ValidPath(p string) bool {
	return action.Action{Kind: action.SELinuxRestorecon, Params: map[string]string{"path": p}}.Validate() == nil
}

// Generic types: a service file found here was never given a label for
// that service.
var genericTypes = map[string]bool{
	"default_t": true, "var_t": true, "usr_t": true, "etc_t": true, "tmp_t": true,
	"user_home_t": true, "user_home_dir_t": true, "admin_home_t": true, "unlabeled_t": true,
	"file_t": true, "var_lib_t": true, "var_log_t": true, "home_root_t": true, "user_tmp_t": true,
	"var_run_t": true, "public_content_t": false,
}

// Generic port types: a port with one of these has no service label yet.
var genericPorts = map[string]bool{
	"unreserved_port_t": true, "reserved_port_t": true, "hi_reserved_port_t": true,
	"ephemeral_port_t": true, "port_t": true,
}

// Preferred types for common services when a place needs a new label.
type typeSet struct{ read, rw, log string }

var serviceTypes = map[string]typeSet{
	"httpd_t":      {"httpd_sys_content_t", "httpd_sys_rw_content_t", "httpd_log_t"},
	"mysqld_t":     {"mysqld_db_t", "mysqld_db_t", "mysqld_log_t"},
	"postgresql_t": {"postgresql_db_t", "postgresql_db_t", "postgresql_log_t"},
	"smbd_t":       {"samba_share_t", "samba_share_t", "samba_log_t"},
	"container_t":  {"container_file_t", "container_file_t", "container_file_t"},
	"ftpd_t":       {"public_content_t", "public_content_rw_t", "xferlog_t"},
	"rsync_t":      {"public_content_t", "public_content_rw_t", "rsync_log_t"},
	"named_t":      {"named_conf_t", "named_cache_t", "named_log_t"},
	"squid_t":      {"squid_conf_t", "squid_cache_t", "squid_log_t"},
	"dovecot_t":    {"dovecot_etc_t", "mail_spool_t", "dovecot_var_log_t"},
}

// Preferred port types for common services.
var servicePorts = map[string]string{
	"httpd_t": "http_port_t", "sshd_t": "ssh_port_t", "named_t": "dns_port_t",
	"mysqld_t": "mysqld_port_t", "postgresql_t": "postgresql_port_t", "smbd_t": "smbd_port_t",
	"squid_t": "squid_port_t", "postfix_master_t": "smtp_port_t", "redis_t": "redis_port_t",
}

var writePerms = map[string]bool{
	"write": true, "append": true, "create": true, "add_name": true, "remove_name": true,
	"unlink": true, "rename": true, "setattr": true, "link": true, "rmdir": true, "reparent": true,
}

// Object classes and permissions that touch the security of the system
// itself: a denial here is more likely the policy doing its job.
var sensitiveClasses = map[string]bool{
	"process": true, "capability": true, "capability2": true, "security": true, "system": true,
	"kernel_service": true, "bpf": true, "key": true, "module_request": false, "cap_userns": true,
}
var sensitiveTypes = map[string]bool{
	"shadow_t": true, "security_t": true, "selinux_config_t": true, "policy_config_t": true,
	"sshd_key_t": true, "passwd_file_t": false, "kernel_t": true, "memory_device_t": true,
	"auditd_log_t": true, "boot_t": true, "systemd_passwd_var_run_t": true, "cert_t": false,
}
var topDirs = map[string]bool{
	"/srv": true, "/var": true, "/opt": true, "/home": true, "/etc": true, "/usr": true, "/var/lib": true,
	"/var/log": true, "/var/www": true, "/tmp": true, "/var/tmp": true, "/root": true, "/mnt": true, "/media": true,
}

// Analyze classifies one denial group and builds the fix, if there is one.
func (z Analyzer) Analyze(ctx context.Context, g Group) Fix {
	z = z.begin()
	a := g.AVC
	f := Fix{Group: g, Features: map[string]bool{}, Facts: map[string]string{}}
	dom, tgt := a.SType(), a.TType()
	f.Evidence = append(f.Evidence, fmt.Sprintf("%d x denied { %s } for %s (%s) on %s %s",
		g.Count, strings.Join(a.Perms, " "), dom, a.Comm, a.Class, objectName(a)))
	f.Features["permissive"] = a.Permissive

	if dom == "basalt_assistant_t" {
		// The assistant never proposes widening its own confinement.
		f.Features["sensitive_target"] = true
	}
	if sensitiveClasses[a.Class] || sensitiveTypes[tgt] || hasAny(a.Perms, "ptrace", "sys_module", "setenforce", "load_policy", "sys_rawio", "mac_admin") {
		f.Features["sensitive_target"] = true
	}

	switch {
	case a.IsPort():
		z.analyzePort(ctx, &f)
	case a.IsFile():
		z.analyzeFile(ctx, &f)
	default:
		f.Features["unsupported_class"] = true
	}
	f.Class = classify(f.Features)
	if f.Class == ClassUnknown || f.Class == ClassSuspicious {
		f.Actions = nil
	}
	z.finishErrors(&f)
	if dom == "basalt_assistant_t" {
		f.Explanation = "a denial of the assistant itself (basalt_assistant_t): its confinement held; this is a bug in the assistant or its policy, never something to allow from here"
	}
	if f.Explanation == "" {
		f.Explanation = "no known fix for this denial; review it (audit2why, sesearch) before changing policy, and do not generate a module from it blindly"
	}
	return f
}

func objectName(a AVC) string {
	switch {
	case a.Path != "":
		return a.Path
	case a.IsPort():
		return a.Proto() + " port " + strconv.Itoa(a.Port)
	case a.Name != "":
		s := a.Name
		if a.Ino != 0 {
			s += fmt.Sprintf(" (inode %d on %s)", a.Ino, a.Dev)
		}
		return s
	}
	return a.TContext
}

func hasAny(list []string, want ...string) bool {
	for _, x := range list {
		for _, w := range want {
			if x == w {
				return true
			}
		}
	}
	return false
}

// classify is the rule-based class used as the decision layer's input.
func classify(ft map[string]bool) string {
	switch {
	case ft["policy_query_failed"]:
		return ClassUnknown
	case ft["sensitive_target"]:
		return ClassSuspicious
	case ft["boolean_off"]:
		return ClassBoolean
	case ft["port_case"] && ft["port_type_found"]:
		return ClassPort
	case ft["default_differs"] && ft["default_allowed"]:
		return ClassMislabeled
	case ft["generic_target"] && ft["candidate_type"]:
		return ClassMissingFcontext
	}
	return ClassUnknown
}

// --- ports -----------------------------------------------------------------------

func (z Analyzer) analyzePort(ctx context.Context, f *Fix) {
	a := f.Group.AVC
	dom := a.SType()
	f.Features["port_case"] = true
	perm := "name_bind"
	if hasAny(a.Perms, "name_connect") {
		perm = "name_connect"
	}
	z.prefetch(ctx, portQueries(a)...)
	cur := z.portType(ctx, a.Proto(), a.Port)
	if cur == "" {
		cur = a.TType()
	}
	f.Facts["port_type_now"] = cur
	f.Evidence = append(f.Evidence, fmt.Sprintf("%s port %d is labeled %s", a.Proto(), a.Port, cur))

	// Booleans: only rules that cover this port's current type (sesearch
	// expands attributes for -t), so an attribute of other ports does not
	// suggest an unrelated boolean.
	for _, r := range z.rules(ctx, AllowQuery(dom, cur, a.Class, perm)) {
		// Only booleans written for this domain and this port type: a
		// boolean on an attribute (nis_enabled for every nsswitch domain
		// and every unreserved port) opens far more than this one port.
		if r.Bool != "" && r.Source == dom && r.Target == cur && !z.booleanOn(ctx, r.Bool) {
			f.Features["boolean_off"] = true
			f.Actions = []action.Action{{Kind: action.SELinuxBoolean, Params: map[string]string{"name": r.Bool, "value": "on"}}}
			f.Explanation = fmt.Sprintf("%s may %s %s port %d (%s) only while the boolean %s is on; it is off", dom, perm, a.Proto(), a.Port, cur, r.Bool)
			f.Evidence = append(f.Evidence, "policy rule: "+r.Line)
			return
		}
	}
	var types []string
	for _, r := range z.rules(ctx, AllowQuery(dom, "", a.Class, perm)) {
		if r.Bool != "" {
			continue
		}
		if strings.HasSuffix(r.Target, "_port_t") && !genericPorts[r.Target] {
			types = append(types, r.Target)
		}
	}
	if perm == "name_connect" {
		// Connecting out to a port is a policy decision (a boolean), not a
		// label: labeling someone else's port for this service is wrong.
		f.Explanation = fmt.Sprintf("%s is not allowed to connect to %s port %d and no boolean covers it", dom, a.Proto(), a.Port)
		return
	}
	want := pickPortType(dom, types)
	if want == "" {
		return
	}
	for _, t := range types {
		if t == cur {
			// The port has a type the domain may use now: fixed since.
			f.Features["resolved"] = true
			f.Explanation = fmt.Sprintf("%s port %d is %s now, which %s may %s", a.Proto(), a.Port, cur, dom, perm)
			return
		}
	}
	f.Features["port_type_found"] = true
	mode := "add"
	if !genericPorts[cur] {
		// The port already belongs to another service's type: changing it
		// takes it from that service. Lower confidence.
		mode = "modify"
		f.Features["port_owned_by_other"] = true
		f.Evidence = append(f.Evidence, fmt.Sprintf("port %d is already labeled %s; relabeling takes it from that type", a.Port, cur))
	}
	f.Actions = []action.Action{{Kind: action.SELinuxPort, Params: map[string]string{
		"proto": a.Proto(), "port": strconv.Itoa(a.Port), "type": want, "mode": mode}}}
	f.Explanation = fmt.Sprintf("%s may %s ports labeled %s; %s port %d is %s", dom, perm, want, a.Proto(), a.Port, cur)
}

func pickPortType(dom string, types []string) string {
	if t, ok := servicePorts[dom]; ok {
		for _, x := range types {
			if x == t {
				return t
			}
		}
	}
	if len(types) == 0 {
		return ""
	}
	stem := strings.TrimSuffix(dom, "_t")
	sort.SliceStable(types, func(i, j int) bool {
		return commonPrefix(types[i], stem) > commonPrefix(types[j], stem)
	})
	return types[0]
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

var rePortcon = regexp.MustCompile(`portcon\s+(\w+)\s+(\d+)(?:-(\d+))?\s+\S+:\S+:(\w+):`)

// portType is the label of a port in the loaded policy (most specific range).
func (z Analyzer) portType(ctx context.Context, proto string, port int) string {
	out, err := z.Policy.Query(ctx, PortconQuery(port)...)
	if err != nil {
		z.fail(err)
		return ""
	}
	best, width := "", 1<<30
	for _, m := range rePortcon.FindAllStringSubmatch(out, -1) {
		if m[1] != proto {
			continue
		}
		lo, _ := strconv.Atoi(m[2])
		hi := lo
		if m[3] != "" {
			hi, _ = strconv.Atoi(m[3])
		}
		if port >= lo && port <= hi && hi-lo < width {
			best, width = m[4], hi-lo
		}
	}
	return best
}

// --- files -----------------------------------------------------------------------

func (z Analyzer) analyzeFile(ctx context.Context, f *Fix) {
	a := f.Group.AVC
	dom, tgt := a.SType(), a.TType()
	p, how := a.Path, "audit record"
	if p == "" && z.Resolve != nil {
		p, how = z.Resolve(ctx, a)
	}
	if p != "" && !ValidPath(p) {
		f.Evidence = append(f.Evidence, fmt.Sprintf("the path lookup returned %q, which is not a valid path; ignored", p))
		p = ""
	}
	if p == "" {
		f.Explanation = fmt.Sprintf("the denied object %q (inode %d on %s) could not be located; find it with: find / -xdev -inum %d", a.Name, a.Ino, a.Dev, a.Ino)
		return
	}
	f.Path, f.PathFrom = p, how
	f.Features["has_path"] = true
	f.Evidence = append(f.Evidence, fmt.Sprintf("object: %s (found via %s), labeled %s", p, how, tgt))
	if PseudoPath(p) {
		f.Explanation = fmt.Sprintf("%s is on a kernel file system: its label (%s) comes from the kernel, not from a file context; no relabel applies", p, tgt)
		return
	}

	perm, write := filePerm(a)
	def := z.defaultType(ctx, p)
	f.DefaultType = def
	z.prefetch(ctx, fileQueries(a, p, def)...)
	if def != "" {
		f.Evidence = append(f.Evidence, fmt.Sprintf("policy default label for %s: %s", p, def))
	}

	// 1. Mislabeled: the default label differs and would be allowed.
	if def != "" && def != tgt {
		f.Features["default_differs"] = true
		if z.allowed(ctx, dom, def, a.Class, perm) {
			f.Features["default_allowed"] = true
			rec := "no"
			if a.Class == "dir" {
				rec = "yes"
			}
			f.Actions = []action.Action{{Kind: action.SELinuxRestorecon, Params: map[string]string{"path": p, "recursive": rec}}}
			f.Explanation = fmt.Sprintf("%s is labeled %s but the policy says %s, which %s may use; the file was probably moved or created with the wrong label", p, tgt, def, dom)
			return
		}
	}

	// 2. A boolean that allows this access is off.
	for _, r := range z.rules(ctx, AllowQuery(dom, tgt, a.Class, perm)) {
		// Only booleans written for this domain (not for an attribute of
		// many domains).
		if r.Bool != "" && r.Source == dom && !z.booleanOn(ctx, r.Bool) {
			f.Features["boolean_off"] = true
			f.Actions = []action.Action{{Kind: action.SELinuxBoolean, Params: map[string]string{"name": r.Bool, "value": "on"}}}
			f.Explanation = fmt.Sprintf("%s may %s %s objects only while the boolean %s is on; it is off", dom, perm, tgt, r.Bool)
			f.Evidence = append(f.Evidence, "policy rule: "+r.Line)
			return
		}
	}

	// 3. Missing file context: the place has a generic label.
	if genericTypes[tgt] || (def != "" && genericTypes[def]) {
		f.Features["generic_target"] = true
	}
	want := z.candidateType(ctx, dom, a.Class, perm, p, write)
	if want == "" {
		f.Explanation = fmt.Sprintf("%s has no type it may %s at %s", dom, perm, p)
		return
	}
	f.Features["candidate_type"] = true
	dir := labelDir(p, a.Class)
	f.Actions = []action.Action{{Kind: action.SELinuxFcontext, Params: map[string]string{"path": dir, "type": want}}}
	f.Explanation = fmt.Sprintf("%s (%s) is labeled %s, a type %s may not %s; give %s the type %s, which it may", dir, p, tgt, dom, perm, dir, want)
}

// labelDir picks the directory a new file context rule covers: the
// object's own directory, never a top-level system directory.
func labelDir(p, class string) string {
	dir := p
	if class != "dir" {
		dir = path.Dir(p)
	}
	if topDirs[dir] || dir == "/" {
		return p
	}
	return dir
}

// PseudoPath reports a path on a kernel pseudo file system (/proc, /sys):
// its labels come from the kernel and the policy (genfscon, the owning
// process), never from file contexts, so no relabel or file context rule
// applies there. Looking one up would also stat the object (matchpathcon
// lstat()s the path), which the confined domain may not do on other
// processes' /proc entries (getattr denials of its own).
func PseudoPath(p string) bool {
	for _, root := range []string{"/proc", "/sys"} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func (z Analyzer) defaultType(ctx context.Context, p string) string {
	if PseudoPath(p) {
		return ""
	}
	res := z.R.Read(ctx, "matchpathcon", "-n", p)
	if res.Code != 0 {
		return ""
	}
	return Type(strings.TrimSpace(res.Out))
}

func (z Analyzer) candidateType(ctx context.Context, dom, class, perm, p string, write bool) string {
	isLog := strings.Contains(p, "/log") || strings.HasSuffix(p, ".log")
	if t := preferredType(dom, p, write); t != "" && z.allowed(ctx, dom, t, class, perm) {
		return t
	}
	// Generic: a type named after the domain that it may use this way.
	stem := strings.TrimSuffix(dom, "_t") + "_"
	var cands []string
	for _, r := range z.rules(ctx, AllowQuery(dom, "", class, perm)) {
		if r.Bool == "" && strings.HasPrefix(r.Target, stem) && strings.HasSuffix(r.Target, "_t") && !genericTypes[r.Target] {
			cands = append(cands, r.Target)
		}
	}
	sort.Strings(cands)
	for _, pref := range []string{"_log_t", "_var_lib_t", "_rw_content_t", "_content_t", "_data_t"} {
		for _, c := range cands {
			if strings.HasSuffix(c, pref) && (isLog == (pref == "_log_t")) {
				return c
			}
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return ""
}

// Rule is one parsed sesearch line.
type Rule struct {
	Source, Target, Class string
	Perms                 []string
	Bool                  string // set for a conditional rule enabled by one boolean
	Line                  string
}

var reRule = regexp.MustCompile(`^allow\s+(\S+)\s+(\S+):(\S+)\s+(\{[^}]*\}|\S+);(?:\s*\[\s*([^\]]+)\s*\]:(True|False))?`)

// ParseSesearch reads `sesearch -A` output.
func ParseSesearch(out string) []Rule {
	var rules []Rule
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		m := reRule.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		r := Rule{Source: m[1], Target: m[2], Class: m[3], Perms: strings.Fields(strings.Trim(m[4], "{} ")), Line: line}
		if m[5] != "" {
			expr := strings.TrimSpace(m[5])
			if m[6] != "True" || strings.ContainsAny(expr, " &|!^") {
				continue // only single-boolean rules enabled by "true" are simple fixes
			}
			r.Bool = expr
		}
		rules = append(rules, r)
	}
	return rules
}

func (z Analyzer) rules(ctx context.Context, query []string) []Rule {
	out, err := z.Policy.Query(ctx, query...)
	if err != nil {
		z.fail(err)
		return nil
	}
	return ParseSesearch(out)
}

// allowed reports an unconditional rule (or one whose boolean is on).
func (z Analyzer) allowed(ctx context.Context, dom, tgt, class, perm string) bool {
	for _, r := range z.rules(ctx, AllowQuery(dom, tgt, class, perm)) {
		if r.Bool == "" || z.booleanOn(ctx, r.Bool) {
			return true
		}
	}
	return false
}

func (z Analyzer) booleanOn(ctx context.Context, name string) bool {
	res := z.R.Read(ctx, "getsebool", name)
	if res.Err != nil || res.Code != 0 {
		// An unknown state is not "off": suggesting to turn it on would
		// rest on nothing.
		z.fail(fmt.Errorf("getsebool %s: %s", name, detail(res)))
		return true
	}
	return strings.Contains(res.Out, "--> on")
}
