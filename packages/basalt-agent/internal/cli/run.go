package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/audit"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/credential"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/egress"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/native"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/podman"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/proxy"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/secrets"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/selinux"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/session"
)

// run is one session in progress.
type run struct {
	dirs    session.Dirs
	pr      *profile.Profile
	info    session.Info
	rtDir   string
	log     *audit.Log
	subject audit.Subject

	proxyIn  io.WriteCloser
	proxyCmd *exec.Cmd
	proxyMu  sync.Mutex

	mu        sync.Mutex
	seen      map[string]bool
	allowed   int
	denied    int
	auditErrs int
	credUses  map[string]int // requests sent with each credential (by name)
	stripped  int            // requests that lost credential headers

	// kernel is set when basalt-resolver filters the session in the
	// kernel (default-deny by cgroup); ledger forwards the records.
	kernel bool
	ledger *ledger.Client
}

func (r *run) record(event, outcome string, data map[string]any) {
	rec, err := r.log.Append(audit.Record{UID: os.Getuid(), Session: r.info.ID, Event: event, Outcome: outcome,
		Subject: r.subject, Data: data})
	if err != nil {
		r.mu.Lock()
		r.auditErrs++
		r.mu.Unlock()
		return
	}
	r.ledger.Send(rec)
}

func cmdRun(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	project := fs.String("project", "", "project directory (required)")
	mode := fs.String("mode", "container", "container or native")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 2, errors.New("usage: basalt-agent run PROFILE --project DIR [--mode container|native] [-- ARGS...]")
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	extra := fs.Args()
	if *project == "" {
		return 2, errors.New("--project DIR is required")
	}
	if *mode != "container" && *mode != "native" {
		return 2, fmt.Errorf("unknown mode %q", *mode)
	}
	if !selinux.Enabled() {
		return 1, errors.New("SELinux is disabled; basalt-agent confines agents with SELinux and will not run them without it")
	}
	if !selinux.Enforcing() {
		fmt.Fprintln(os.Stderr, "basalt-agent: warning: SELinux is permissive, the session is NOT confined (denials are only logged)")
	}
	d, err := session.UserDirs()
	if err != nil {
		return 1, err
	}
	pr, err := sharePaths(d).Load(name)
	if err != nil {
		return 1, err
	}
	proj, err := session.CheckProject(*project, d)
	if err != nil {
		return 1, err
	}
	unlock, err := session.Lock(filepath.Join(d.Data, "home", pr.Name+".lock"))
	if err != nil {
		return 1, fmt.Errorf("profile %s is in use by another session (its home is relabeled per session): %w", pr.Name, err)
	}
	defer unlock()

	var used []string
	for _, s := range d.Running() {
		used = append(used, s.Level)
	}
	level, err := session.PickLevel(used)
	if err != nil {
		return 1, err
	}
	// Native mode is default-deny in the kernel: without basalt-resolver
	// there is no kernel filter, so the session does not start (fail closed).
	r := &run{dirs: d, pr: pr, log: audit.Open(d.AuditLog()), seen: map[string]bool{}, credUses: map[string]int{},
		kernel: egress.Available()}
	if *mode == "native" && !r.kernel {
		return 1, errors.New("native mode needs basalt-resolver for its default-deny network (systemctl enable --now basalt-resolver)")
	}
	if !r.kernel {
		fmt.Fprintln(os.Stderr, "basalt-agent: basalt-resolver is not running; the container has no network and the proxy filters by name, but the proxy itself is not filtered in the kernel")
	}
	r.ledger = ledger.New()
	defer r.ledger.Flush(3 * time.Second)
	r.info = session.Info{ID: session.NewID(), Profile: pr.Name, Mode: *mode, Level: level, Project: proj,
		PID: os.Getpid(), Started: time.Now()}
	r.subject = audit.Subject{Profile: pr.Name, Mode: *mode, Level: level, Project: proj}
	r.rtDir = filepath.Join(d.Runtime, r.info.ID)
	if err := os.MkdirAll(d.Runtime, 0o700); err != nil {
		return 1, err
	}
	if err := os.Mkdir(r.rtDir, 0o700); err != nil {
		return 1, err
	}
	defer os.RemoveAll(r.rtDir)
	if _, err := selinux.Relabel(r.rtDir, native.SessionType, level, nil); err != nil {
		return 1, err
	}
	home := d.AgentHome(pr.Name)
	if err := os.MkdirAll(filepath.Join(home, ".tmp"), 0o700); err != nil {
		return 1, err
	}

	// The secret store is labeled so no agent domain can read it, even if
	// a later policy gave an agent the rest of ~/.config.
	if fi, err := os.Stat(d.SecretDir()); err == nil && fi.IsDir() {
		if _, err := selinux.Relabel(d.SecretDir(), native.SecretType, "s0", nil); err != nil {
			fmt.Fprintf(os.Stderr, "basalt-agent: warning: labeling %s: %v\n", d.SecretDir(), err)
		}
	}
	vals, missing, err := secrets.Load(d.SecretFile(pr.Name), pr.Name, pr.SecretEnv)
	if err != nil {
		return 1, err
	}
	if len(vals) == 0 && len(pr.SecretEnv) > 0 {
		fmt.Fprintf(os.Stderr, "basalt-agent: no API key for %s (%s in %s); the agent may ask you to log in\n",
			pr.Name, strings.Join(missing, " or "), d.SecretFile(pr.Name))
	}
	// Keys stay in the session proxy. The agent gets a placeholder and a
	// base URL; a key without a usable route is not used at all.
	creds, withheld := credentialRoutes(pr, vals)
	for _, w := range withheld {
		fmt.Fprintf(os.Stderr, "basalt-agent: %s is not used in this session: %s\n", w["name"], w["reason"])
	}
	agentEnv := map[string]string{"BASALT_AGENT_SESSION": r.info.ID}
	for _, c := range creds {
		agentEnv[c.Name] = credential.Placeholder
		if c.BaseEnv != "" {
			agentEnv[c.BaseEnv] = c.BaseURL()
		}
	}

	// The session proxy: Unix socket for a container, loopback port with a
	// password for native mode.
	token := ""
	listen := "unix:" + filepath.Join(r.rtDir, "proxy.sock")
	if *mode == "native" {
		token = session.RandomToken()
		listen = "tcp:127.0.0.1:" + native.ProxyPorts
	}
	addr, err := r.startProxy(listen, token, creds)
	if err != nil {
		return 1, err
	}
	defer r.stopProxy()
	r.info.Proxy = addr
	filter := "proxy"
	if r.kernel {
		dns, err := r.registerEgress()
		if err != nil {
			r.record(audit.SessionStart, "error", map[string]any{"error": err.Error()})
			return 1, fmt.Errorf("kernel egress filter: %w", err)
		}
		defer r.endEgress()
		filter = "kernel+proxy (resolver " + dns + ")"
	}
	b, _ := json.Marshal(r.info)
	if err := os.WriteFile(filepath.Join(r.rtDir, "session.json"), b, 0o600); err != nil {
		return 1, err
	}
	stopCtl, err := r.serveControl()
	if err != nil {
		return 1, err
	}
	defer stopCtl()

	names := make([]string, 0, len(creds))
	credInfo := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		names = append(names, c.Name)
		credInfo = append(credInfo, c.Describe())
	}
	hosts := make([]string, 0, len(pr.Egress))
	for _, e := range pr.Egress {
		hosts = append(hosts, e.String())
	}
	start := time.Now()
	command := append(append([]string{pr.Command}, pr.Args...), extra...)

	var cmd *exec.Cmd
	startData := map[string]any{"command": command, "egress": hosts, "secrets": names, "credentials": credInfo,
		"proxy": addr, "egress_filter": filter, "loopback": pr.Loopback}
	if len(withheld) > 0 {
		startData["secrets_withheld"] = withheld
	}
	if *mode == "container" {
		cmd, err = r.containerCmd(proj, home, agentEnv, command, startData)
	} else {
		cmd, err = r.nativeCmd(proj, home, agentEnv, command, token, addr, startData)
	}
	if err != nil {
		r.record(audit.SessionStart, "error", map[string]any{"error": err.Error()})
		return 1, err
	}
	r.record(audit.SessionStart, "ok", startData)
	fmt.Fprintf(os.Stderr, "basalt-agent: session %s, %s mode, level %s, project %s\n", r.info.ID, *mode, level, proj)

	// The agent owns the terminal: ^C goes to it, the launcher stays.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sig)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	code := 0
	if err := cmd.Start(); err != nil {
		r.record(audit.SessionEnd, "error", map[string]any{"error": err.Error()})
		return 1, err
	}
	go func() {
		for s := range sig {
			if s == syscall.SIGTERM || s == syscall.SIGHUP {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
		if code < 0 {
			code = 1
		}
	case err != nil:
		code = 1
	}
	r.mu.Lock()
	allowed, denied, stripped := r.allowed, r.denied, r.stripped
	uses := map[string]int{}
	for k, v := range r.credUses {
		uses[k] = v
	}
	r.mu.Unlock()
	endData := map[string]any{"exit_code": code, "duration_s": int(time.Since(start).Seconds()),
		"egress_allowed": allowed, "egress_denied": denied}
	if len(uses) > 0 {
		endData["credential_uses"] = uses
	}
	if stripped > 0 {
		endData["credential_stripped"] = stripped
	}
	r.record(audit.SessionEnd, "ok", endData)
	if denied > 0 {
		fmt.Fprintf(os.Stderr, "basalt-agent: %d connection(s) refused by the session allowlist: basalt-agent audit --session %s\n", denied, r.info.ID)
	}
	if r.auditErrs > 0 {
		fmt.Fprintf(os.Stderr, "basalt-agent: warning: %d audit records could not be written\n", r.auditErrs)
	}
	return code, nil
}

// registerEgress hands the session's cgroup (the slice the proxy runs
// in, and the native agent later) and allowlist to basalt-resolver, which
// makes the session default-deny in the kernel.
func (r *run) registerEgress() (string, error) {
	cg, err := egress.CgroupOf(r.proxyCmd.Process.Pid)
	if err != nil {
		return "", err
	}
	slice, err := egress.SessionSlice(cg, r.info.ID)
	if err != nil {
		return "", err
	}
	allow := make([]string, 0, len(r.pr.Egress))
	for _, e := range r.pr.Egress {
		allow = append(allow, e.String())
	}
	lb := r.pr.Loopback
	rep, err := egress.Do(egress.Request{Op: "register", Session: r.info.ID, Profile: r.pr.Name, Mode: r.info.Mode,
		Level: r.info.Level, Project: r.info.Project, Cgroup: slice, Allow: allow, Loopback: &lb})
	if err != nil {
		return "", err
	}
	return rep.DNS, nil
}

func (r *run) endEgress() {
	if _, err := egress.Do(egress.Request{Op: "end", Session: r.info.ID}); err != nil {
		fmt.Fprintf(os.Stderr, "basalt-agent: ending the kernel egress filter: %v\n", err)
	}
}

// startProxy starts the proxy process in basalt_agent_proxy_t at the
// session level and returns its address. With kernel filtering it runs in
// the session's slice, so its own connections are default-deny as well:
// a proxy bug cannot reach an address no allowed name resolved to.
func (r *run) startProxy(listen, token string, creds []credential.Route) (string, error) {
	argv := native.Runcon(native.ProxyDomain, r.info.Level, native.ProxyHelper)
	if r.kernel {
		argv = append(egress.ScopeArgs(r.info.ID, "proxy"), argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// A small environment, not the user's: whatever the user's shell
	// exports (an API key, say) stays out of the session. The pure Go
	// resolver sends DNS to the stub address, which the kernel redirects
	// to the session resolver (the C library could use other paths, such
	// as systemd-resolved's varlink socket).
	cmd.Env = proxyEnv(r.kernel)
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = os.Stderr
	// Own process group: a ^C in the terminal does not stop the proxy.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("session proxy: %w", err)
	}
	r.proxyIn, r.proxyCmd = in, cmd
	allow := make([]string, 0, len(r.pr.Egress))
	for _, e := range r.pr.Egress {
		allow = append(allow, e.String())
	}
	cfg, _ := json.Marshal(proxy.Config{Listen: listen, Allow: allow, Token: token, Credentials: creds})
	_, err = in.Write(append(cfg, '\n'))
	for i := range cfg {
		cfg[i] = 0
	}
	if err != nil {
		return "", err
	}
	br := bufio.NewReader(out)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = cmd.Wait()
		return "", fmt.Errorf("session proxy did not start (is the basalt-agent SELinux module installed?)")
	}
	var m proxy.Message
	if err := json.Unmarshal(line, &m); err != nil || !m.Ready {
		return "", fmt.Errorf("session proxy: %s", strings.TrimSpace(m.Error+" "+string(line)))
	}
	go r.readProxy(br)
	return m.Addr, nil
}

func (r *run) readProxy(br *bufio.Reader) {
	sc := bufio.NewScanner(br)
	for sc.Scan() {
		var m proxy.Message
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Decision == nil {
			continue
		}
		d := *m.Decision
		data := map[string]any{"host": d.Host, "port": d.Port}
		if d.Credential != "" {
			data["credential"] = d.Credential
		}
		if len(d.Stripped) > 0 {
			r.mu.Lock()
			r.stripped++
			n := r.stripped
			r.mu.Unlock()
			if n <= 1000 {
				r.record(audit.CredentialStrip, "denied", map[string]any{"host": d.Host, "port": d.Port,
					"headers": d.Stripped, "reason": "credential headers are sent only to the host their key belongs to"})
			}
		}
		if d.Allowed && d.Credential != "" {
			key := d.Credential + "@" + d.Host
			r.mu.Lock()
			r.credUses[d.Credential]++
			first := !r.seen[key]
			r.seen[key] = true
			r.mu.Unlock()
			if first {
				r.record(audit.CredentialUse, "ok", map[string]any{"credential": d.Credential, "host": d.Host, "port": d.Port})
			}
		}
		if d.Allowed {
			key := fmt.Sprintf("%s:%d", d.Host, d.Port)
			r.mu.Lock()
			r.allowed++
			first := !r.seen[key]
			r.seen[key] = true
			r.mu.Unlock()
			if first {
				r.record(audit.EgressAllow, "allowed", data)
			}
			continue
		}
		r.mu.Lock()
		r.denied++
		n := r.denied
		r.mu.Unlock()
		// Every refusal is recorded, up to a cap per session.
		if n <= 1000 {
			data["reason"] = d.Reason
			r.record(audit.EgressDeny, "denied", data)
		}
	}
}

func (r *run) stopProxy() {
	if r.proxyIn != nil {
		r.proxyIn.Close()
	}
	if r.proxyCmd != nil {
		done := make(chan struct{})
		go func() { _ = r.proxyCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = r.proxyCmd.Process.Kill()
		}
	}
}

func (r *run) proxyAllow(entry string) error {
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()
	b, _ := json.Marshal(proxy.Command{Op: "allow", Entry: entry})
	_, err := r.proxyIn.Write(append(b, '\n'))
	return err
}

func (r *run) containerCmd(proj, home string, agentEnv map[string]string, command []string, data map[string]any) (*exec.Cmd, error) {
	img := podman.Image(r.pr.Name)
	if exec.Command("podman", "image", "exists", img).Run() != nil {
		return nil, fmt.Errorf("no image %s: basalt-agent image build %s", img, r.pr.Name)
	}
	spec := podman.Spec{Session: r.info.ID, Image: img, Level: r.info.Level, Project: proj, AgentHome: home,
		ProxySocket: filepath.Join(r.rtDir, "proxy.sock"), Env: podman.SessionEnv(r.pr, agentEnv), Command: command,
		TTY: isTTY(0) && isTTY(1)}
	for _, p := range readOnlyPaths {
		if _, err := os.Lstat(filepath.Join(proj, p)); err == nil {
			spec.ReadOnly = append(spec.ReadOnly, p)
		}
	}
	data["image"] = img
	data["read_only"] = spec.ReadOnly
	args := podman.RunArgs(spec)
	return exec.Command("podman", args...), nil
}

func (r *run) nativeCmd(proj, home string, agentEnv map[string]string, command []string, token, addr string, data map[string]any) (*exec.Cmd, error) {
	tools := r.dirs.Tools(r.pr.Name)
	if r.pr.Method != "none" {
		if _, err := os.Stat(tools); err != nil {
			return nil, fmt.Errorf("%s is not installed for native mode: basalt-agent install %s", r.pr.Name, r.pr.Name)
		}
	}
	lvl := r.info.Level
	isRO := func(rel string) bool {
		for _, p := range readOnlyPaths {
			if rel == p {
				return true
			}
		}
		return false
	}
	n, err := selinux.Relabel(proj, native.ProjectType, lvl, isRO)
	if err != nil {
		return nil, err
	}
	for _, p := range readOnlyPaths {
		if _, err := os.Lstat(filepath.Join(proj, p)); err == nil {
			m, err := selinux.Relabel(filepath.Join(proj, p), native.ReadOnlyTyp, lvl, nil)
			if err != nil {
				return nil, err
			}
			n += m
		}
	}
	h, err := selinux.Relabel(home, native.HomeType, lvl, nil)
	if err != nil {
		return nil, err
	}
	t := 0
	if _, err := os.Stat(tools); err == nil {
		if t, err = selinux.Relabel(tools, native.ToolType, "s0", nil); err != nil {
			return nil, err
		}
	}
	r.record(audit.Relabel, "ok", map[string]any{"path": proj, "type": native.ProjectType, "level": lvl, "count": n,
		"home_count": h, "tools_count": t})
	data["relabeled"] = n
	proxyURL := fmt.Sprintf("http://basalt:%s@%s", token, addr)
	env := native.Env(home, tools, podman.ProxyEnv(proxyURL), agentEnv, r.pr.Env)
	argv := native.Runcon(native.Domain, lvl, native.ExecHelper, append([]string{proj}, command...)...)
	if r.kernel {
		// The agent joins the session slice, where the kernel filter applies.
		// systemd-run needs the user's runtime directory to reach the user
		// service manager; the exec helper removes it before the agent runs.
		argv = append(egress.ScopeArgs(r.info.ID, "agent"), argv...)
		for _, k := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = proj
	return cmd, nil
}

// Grant requests arrive on RUNTIME/ID/control.sock from the root helper
// (after polkit admin authentication); any other peer is refused.
type grantRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	ByUID int    `json:"by_uid"`
}

type grantReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (r *run) serveControl() (func(), error) {
	path := filepath.Join(r.rtDir, "control.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go r.handleControl(c.(*net.UnixConn))
		}
	}()
	return func() { l.Close() }, nil
}

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *syscall.Ucred
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

func (r *run) handleControl(c *net.UnixConn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	reply := func(err error) {
		rep := grantReply{OK: err == nil}
		if err != nil {
			rep.Error = err.Error()
		}
		b, _ := json.Marshal(rep)
		_, _ = c.Write(append(b, '\n'))
	}
	var req grantRequest
	if err := json.NewDecoder(io.LimitReader(c, 4096)).Decode(&req); err != nil {
		reply(errors.New("bad request"))
		return
	}
	uid, err := peerUID(c)
	data := map[string]any{"kind": req.Kind, "value": req.Value, "by_uid": req.ByUID, "peer_uid": uid}
	r.record(audit.GrantRequest, "ok", data)
	if err != nil || uid != 0 {
		r.record(audit.GrantApply, "denied", map[string]any{"kind": req.Kind, "value": req.Value,
			"reason": "grants come only from the polkit-authorized helper"})
		reply(errors.New("refused: not from the authorized grant helper"))
		return
	}
	switch req.Kind {
	case "host":
		e, perr := allowlist.ParseEntry(req.Value)
		if perr == nil {
			perr = r.proxyAllow(e.String())
		}
		err = perr
	case "path":
		err = r.grantPath(req.Value)
	default:
		err = fmt.Errorf("unknown grant %q", req.Kind)
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	r.record(audit.GrantApply, outcome, map[string]any{"kind": req.Kind, "value": req.Value})
	reply(err)
}

// grantPath gives a native session one more directory (relabeled to the
// project type at the session level). Containers cannot gain mounts.
func (r *run) grantPath(dir string) error {
	if r.info.Mode != "native" {
		return errors.New("path grants work in native mode only; restart the container session with that project")
	}
	p, err := session.CheckProject(dir, r.dirs)
	if err != nil {
		return err
	}
	_, err = selinux.Relabel(p, native.ProjectType, r.info.Level, nil)
	return err
}

// credentialRoutes pairs each loaded key with its route. A key is withheld
// (not used, not given to the agent) when it has no route, when its route's
// host is not on the session allowlist at the route's port, or when an
// earlier key already has that host.
func credentialRoutes(pr *profile.Profile, vals map[string]string) ([]credential.Route, []map[string]string) {
	l := allowlist.New(pr.Egress)
	var out []credential.Route
	var withheld []map[string]string
	hosts := map[string]string{}
	for _, name := range pr.SecretEnv {
		v, ok := vals[name]
		if !ok {
			continue
		}
		rt, ok := pr.Route(name)
		reason := ""
		switch {
		case !ok:
			reason = "no credential route (add \"route = " + name + " HOST header=HEADER\" to [secrets])"
		case !l.Allows(rt.Host, rt.Port):
			reason = fmt.Sprintf("%s:%d is not on the session allowlist", rt.Host, rt.Port)
		case hosts[rt.Host] != "":
			reason = fmt.Sprintf("%s already uses %s", rt.Host, hosts[rt.Host])
		}
		if reason != "" {
			withheld = append(withheld, map[string]string{"name": name, "reason": reason})
			continue
		}
		hosts[rt.Host] = name
		rt.Value = v
		out = append(out, rt)
	}
	return out, withheld
}

// proxyEnv is the proxy process's whole environment. systemd-run needs the
// user's runtime directory and bus to start the session scope; the proxy
// domain cannot use them.
func proxyEnv(kernel bool) []string {
	env := []string{"PATH=/usr/bin:/bin", "GODEBUG=netdns=go", "LANG=C.UTF-8"}
	for _, k := range []string{"HOME", "USER", "LOGNAME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if kernel {
		for _, k := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	return env
}
