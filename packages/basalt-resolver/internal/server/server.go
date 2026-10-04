// Package server is basalt-resolver: a local DNS resolver owned by Basalt
// that gives each confined session (an agent session, a confined app) a
// default-deny network policy with name-based exceptions.
//
// A launcher registers a session over the control socket: the session's
// cgroup, its egress allowlist (basalt-agent's format) and who it is. The
// resolver then
//
//   - installs nftables chains that match the session's sockets by cgroup
//     and drop everything except loopback essentials and the addresses in
//     the session's sets (package nft);
//   - redirects the session's DNS to a resolver port of its own, where it
//     answers only names on the allowlist, refuses addresses that are not
//     public for entries not marked "private" (DNS rebinding), and adds
//     each address it hands out to the session's set, for the entry's
//     ports, until the record's TTL (clamped) runs out;
//   - turns every refusal and every dropped packet (nflog) into a record
//     for basalt-ledger.
//
// Allowlists only grow through the control socket from root (the polkit
// helper of basalt-agent grant); the session owner registers and ends a
// session, nothing more.
package server

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/allowlist"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/dnsmsg"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/nflog"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/nft"
)

// Config of the resolver.
type Config struct {
	ControlSocket string        // /run/basalt-resolver/control.sock
	StateDir      string        // /run/basalt-resolver/sessions
	CgroupRoot    string        // /sys/fs/cgroup
	Upstream      string        // 127.0.0.53:53 (systemd-resolved's stub)
	Listen        string        // 127.0.0.1
	PortFirst     int           // 47200
	PortLast      int           // 47263
	LogGroup      int           // nflog group of the session drop rules
	MinTTL        time.Duration // shortest set element lifetime
	MaxTTL        time.Duration // longest
	Grace         time.Duration // added to the TTL so a client that just got an answer can connect
	MaxSessions   int
}

// Defaults returns the packaged configuration.
func Defaults() Config {
	return Config{ControlSocket: "/run/basalt-resolver/control.sock", StateDir: "/run/basalt-resolver/sessions",
		CgroupRoot: "/sys/fs/cgroup", Upstream: "127.0.0.53:53", Listen: "127.0.0.1", PortFirst: 47200, PortLast: 47263,
		LogGroup: 47, MinTTL: time.Minute, MaxTTL: time.Hour, Grace: 5 * time.Minute, MaxSessions: 64}
}

// Firewall is what the server needs from package nft (a fake in tests).
type Firewall interface {
	Init() error
	Add(nft.Session) error
	Remove(name string) error
	AddElements(name string, els []nft.Element) error
	State() (nft.State, error)
}

// Sink receives audit records (the ledger client, or a recorder in tests).
type Sink interface{ Send(ledger.Record) }

// Server is the resolver.
type Server struct {
	cfg  Config
	fw   Firewall
	sink Sink
	logf func(string, ...any)

	// Upstream exchange; replaced in tests.
	Exchange func(query []byte) ([]byte, error)

	mu       sync.Mutex
	sessions map[string]*session // by id
	byNft    map[string]*session
	closed   bool
}

// session is one registered session.
type session struct {
	State
	list *allowlist.List

	udp *net.UDPConn
	tcp *net.TCPListener

	mu        sync.Mutex
	seenAllow map[string]bool
	lastDeny  map[string]time.Time
	counts    map[string]int
}

// State is what is saved per session (StateDir/ID.json), so a restarted
// resolver picks its sessions up again.
type State struct {
	ID       string    `json:"id"`
	Nft      string    `json:"nft"`
	UID      int       `json:"uid"`
	Profile  string    `json:"profile,omitempty"`
	Mode     string    `json:"mode,omitempty"`
	Level    string    `json:"level,omitempty"`
	Project  string    `json:"project,omitempty"`
	App      string    `json:"app,omitempty"`
	Cgroup   string    `json:"cgroup"`
	Port     int       `json:"port"`
	Loopback bool      `json:"loopback"`
	Allow    []string  `json:"allow"`
	Created  time.Time `json:"created"`
}

// New returns a server; Start brings it up.
func New(cfg Config, fw Firewall, sink Sink, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Server{cfg: cfg, fw: fw, sink: sink, logf: logf, sessions: map[string]*session{}, byNft: map[string]*session{}}
	s.Exchange = s.exchangeUpstream
	return s
}

var idRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// NftName maps a session id to its nftables name: s-b4896c08ff03 -> s_b4896c08ff03.
func NftName(id string) string {
	n := strings.ReplaceAll(id, "-", "_")
	if !strings.HasPrefix(n, "s_") {
		n = "s_" + n
	}
	return n
}

// subject is the ledger subject of a session.
func (s *session) subject() ledger.Subject {
	return ledger.Subject{Profile: s.Profile, Mode: s.Mode, Level: s.Level, Project: s.Project, App: s.App}
}

func (s *Server) record(ss *session, event, outcome string, data map[string]any) {
	if s.sink == nil {
		return
	}
	r := ledger.Record{Event: event, Outcome: outcome, Data: data}
	if ss != nil {
		r.UID, r.Session, r.Subject = ss.UID, ss.ID, ss.subject()
	}
	s.sink.Send(r)
}

// ---------------------------------------------------------------------------
// Start, restore, stop

// Start initializes the table, restores saved sessions whose cgroup still
// exists and removes the rest.
func (s *Server) Start() error {
	if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
		return err
	}
	if err := s.fw.Init(); err != nil {
		return err
	}
	st, err := s.fw.State()
	if err != nil {
		return err
	}
	saved := map[string]State{}
	files, _ := filepath.Glob(filepath.Join(s.cfg.StateDir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ss State
		if json.Unmarshal(b, &ss) != nil || !idRe.MatchString(ss.ID) {
			_ = os.Remove(f)
			continue
		}
		saved[ss.Nft] = ss
	}
	for _, name := range st.Sessions() {
		ss, ok := saved[name]
		if !ok || !s.cgroupExists(ss.Cgroup) {
			s.logf("removing stale session rules %s", name)
			if err := s.fw.Remove(name); err != nil {
				s.logf("remove %s: %v", name, err)
			}
			if ok {
				_ = os.Remove(filepath.Join(s.cfg.StateDir, ss.ID+".json"))
			}
			continue
		}
		if err := s.restore(ss); err != nil {
			s.logf("restore %s: %v; removing its rules (the session loses its network)", ss.ID, err)
			_ = s.fw.Remove(name)
			_ = os.Remove(filepath.Join(s.cfg.StateDir, ss.ID+".json"))
			continue
		}
		s.logf("restored session %s (uid %d, %s)", ss.ID, ss.UID, ss.Cgroup)
		delete(saved, name)
	}
	for _, ss := range saved {
		if _, ok := s.sessions[ss.ID]; !ok {
			_ = os.Remove(filepath.Join(s.cfg.StateDir, ss.ID+".json"))
		}
	}
	return nil
}

func (s *Server) restore(st State) error {
	ss, err := newSession(st)
	if err != nil {
		return err
	}
	if err := s.listen(ss); err != nil {
		return err
	}
	s.mu.Lock()
	s.sessions[ss.ID], s.byNft[ss.Nft] = ss, ss
	s.mu.Unlock()
	return nil
}

func newSession(st State) (*session, error) {
	l := allowlist.New(nil)
	for _, a := range st.Allow {
		e, err := allowlist.ParseEntry(a)
		if err != nil {
			return nil, err
		}
		l.Add(e)
	}
	return &session{State: st, list: l, seenAllow: map[string]bool{}, lastDeny: map[string]time.Time{}, counts: map[string]int{}}, nil
}

// Close stops the DNS listeners. Session rules stay in the kernel (fail
// closed); a restart restores them.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	for _, ss := range s.sessions {
		ss.closeListeners()
	}
	s.mu.Unlock()
}

func (ss *session) closeListeners() {
	if ss.udp != nil {
		ss.udp.Close()
	}
	if ss.tcp != nil {
		ss.tcp.Close()
	}
}

func (s *Server) cgroupExists(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return false
	}
	st, err := os.Stat(filepath.Join(s.cfg.CgroupRoot, path))
	return err == nil && st.IsDir()
}

// GC removes sessions whose cgroup is gone (the launcher died without
// ending the session). Run periodically.
func (s *Server) GC() {
	s.mu.Lock()
	var gone []*session
	for _, ss := range s.sessions {
		if !s.cgroupExists(ss.Cgroup) {
			gone = append(gone, ss)
		}
	}
	s.mu.Unlock()
	for _, ss := range gone {
		s.logf("session %s: cgroup %s is gone, ending it", ss.ID, ss.Cgroup)
		_ = s.end(ss, "cgroup gone")
	}
}

// ---------------------------------------------------------------------------
// Control socket

// Request is a control socket request (one JSON object per line).
type Request struct {
	Op       string   `json:"op"` // register, allow, end, list, status
	Session  string   `json:"session,omitempty"`
	Profile  string   `json:"profile,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Level    string   `json:"level,omitempty"`
	Project  string   `json:"project,omitempty"`
	App      string   `json:"app,omitempty"`
	Cgroup   string   `json:"cgroup,omitempty"`
	Allow    []string `json:"allow,omitempty"`
	Entry    string   `json:"entry,omitempty"`
	Loopback *bool    `json:"loopback,omitempty"`
	ByUID    int      `json:"by_uid,omitempty"`
}

// Reply to a control request.
type Reply struct {
	OK       bool          `json:"ok"`
	Error    string        `json:"error,omitempty"`
	DNS      string        `json:"dns,omitempty"`
	Sessions []SessionView `json:"sessions,omitempty"`
}

// SessionView is a session as listed.
type SessionView struct {
	State
	Counts map[string]int `json:"counts"`
}

// Peer is the identity of a control socket client.
type Peer struct {
	UID int
	PID int
}

// ServeControl accepts control connections on l.
func (s *Server) ServeControl(l *net.UnixListener) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		go s.handleControl(c)
	}
}

func peerCred(c *net.UnixConn) (Peer, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Peer{}, err
	}
	if cerr != nil {
		return Peer{}, cerr
	}
	return Peer{UID: int(cred.Uid), PID: int(cred.Pid)}, nil
}

func (s *Server) handleControl(c *net.UnixConn) {
	defer c.Close()
	peer, err := peerCred(c)
	if err != nil {
		return
	}
	br := bufio.NewReader(io.LimitReader(c, 1<<20))
	for {
		_ = c.SetDeadline(time.Now().Add(5 * time.Minute))
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var req Request
		var rep Reply
		if err := json.Unmarshal(line, &req); err != nil {
			rep.Error = "bad request"
		} else {
			rep = s.Handle(peer, req)
		}
		b, _ := json.Marshal(rep)
		if _, err := c.Write(append(b, '\n')); err != nil {
			return
		}
	}
}

// Handle runs one control request for peer.
func (s *Server) Handle(peer Peer, req Request) Reply {
	fail := func(format string, a ...any) Reply { return Reply{Error: fmt.Sprintf(format, a...)} }
	switch req.Op {
	case "register":
		addr, err := s.register(peer, req)
		if err != nil {
			s.logf("register %s by uid %d refused: %v", req.Session, peer.UID, err)
			s.record(nil, "egress.session.refused", "denied", map[string]any{"session": req.Session,
				"cgroup": req.Cgroup, "peer_uid": peer.UID, "reason": err.Error()})
			return fail("%v", err)
		}
		return Reply{OK: true, DNS: addr}
	case "allow":
		if err := s.allow(peer, req); err != nil {
			return fail("%v", err)
		}
		return Reply{OK: true}
	case "end":
		ss := s.get(req.Session)
		if ss == nil {
			return fail("no session %s", req.Session)
		}
		if peer.UID != 0 && peer.UID != ss.UID {
			return fail("session %s belongs to another user", req.Session)
		}
		if err := s.end(ss, "ended by its launcher"); err != nil {
			return fail("%v", err)
		}
		return Reply{OK: true}
	case "list":
		return Reply{OK: true, Sessions: s.list(peer)}
	case "status":
		return Reply{OK: true}
	}
	return fail("unknown operation %q", req.Op)
}

func (s *Server) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) list(peer Peer) []SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SessionView
	for _, ss := range s.sessions {
		if peer.UID != 0 && peer.UID != ss.UID {
			continue
		}
		ss.mu.Lock()
		counts := map[string]int{}
		for k, v := range ss.counts {
			counts[k] = v
		}
		st := ss.State
		st.Allow = entryStrings(ss.list)
		ss.mu.Unlock()
		out = append(out, SessionView{State: st, Counts: counts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func entryStrings(l *allowlist.List) []string {
	es := l.Entries()
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.String()
	}
	return out
}

// checkCgroup verifies that path is a cgroup the peer may hand over: for
// a regular user, strictly below their own user manager
// (/user.slice/user-UID.slice/user@UID.service/...), owned by them and
// not one of the manager's own slices; root may register any cgroup
// except the root and its first level.
func (s *Server) checkCgroup(peer Peer, path string) error {
	clean := filepath.Clean(path)
	if clean != path || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\"\n\t ") {
		return fmt.Errorf("bad cgroup path %q", path)
	}
	if peer.UID != 0 {
		base := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/", peer.UID, peer.UID)
		rest, ok := strings.CutPrefix(path, base)
		if !ok || rest == "" {
			return fmt.Errorf("cgroup %s is not inside your user manager (%s)", path, base)
		}
		switch rest {
		case "app.slice", "session.slice", "background.slice", "user.slice", "init.scope":
			return fmt.Errorf("cgroup %s is shared by other programs; a session needs its own", path)
		}
	} else if nft.Level(path) < 2 {
		return fmt.Errorf("cgroup %s is too broad", path)
	}
	st, err := os.Stat(filepath.Join(s.cfg.CgroupRoot, path))
	if err != nil || !st.IsDir() {
		return fmt.Errorf("no cgroup %s", path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && peer.UID != 0 && int(sys.Uid) != peer.UID {
		return fmt.Errorf("cgroup %s is not yours", path)
	}
	return nil
}

func (s *Server) register(peer Peer, req Request) (string, error) {
	if !idRe.MatchString(req.Session) {
		return "", fmt.Errorf("bad session id %q", req.Session)
	}
	if err := s.checkCgroup(peer, req.Cgroup); err != nil {
		return "", err
	}
	st := State{ID: req.Session, Nft: NftName(req.Session), UID: peer.UID, Profile: req.Profile, Mode: req.Mode,
		Level: req.Level, Project: req.Project, App: req.App, Cgroup: req.Cgroup, Loopback: true, Created: time.Now().UTC()}
	if req.Loopback != nil {
		st.Loopback = *req.Loopback
	}
	if !nft.ValidName(st.Nft) {
		return "", fmt.Errorf("bad session id %q", req.Session)
	}
	for _, a := range req.Allow {
		e, err := allowlist.ParseEntry(a)
		if err != nil {
			return "", err
		}
		st.Allow = append(st.Allow, e.String())
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", errors.New("resolver is stopping")
	}
	if _, ok := s.sessions[st.ID]; ok {
		s.mu.Unlock()
		return "", fmt.Errorf("session %s is already registered", st.ID)
	}
	for _, o := range s.sessions {
		if o.Cgroup == st.Cgroup {
			s.mu.Unlock()
			return "", fmt.Errorf("cgroup %s already belongs to session %s", st.Cgroup, o.ID)
		}
	}
	if len(s.sessions) >= s.cfg.MaxSessions {
		s.mu.Unlock()
		return "", errors.New("too many sessions")
	}
	used := map[int]bool{}
	for _, o := range s.sessions {
		used[o.Port] = true
	}
	for p := s.cfg.PortFirst; p <= s.cfg.PortLast; p++ {
		if !used[p] {
			st.Port = p
			break
		}
	}
	if st.Port == 0 {
		s.mu.Unlock()
		return "", errors.New("no free resolver port")
	}
	ss, err := newSession(st)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	// Reserve the id and port while the listeners and rules come up.
	s.sessions[ss.ID], s.byNft[ss.Nft] = ss, ss
	s.mu.Unlock()
	undo := func() {
		ss.closeListeners()
		s.mu.Lock()
		delete(s.sessions, ss.ID)
		delete(s.byNft, ss.Nft)
		s.mu.Unlock()
	}
	if err := s.listen(ss); err != nil {
		undo()
		return "", err
	}
	_ = s.fw.Remove(ss.Nft) // leftovers of an earlier session with the same id
	if err := s.fw.Add(nft.Session{Name: ss.Nft, Cgroup: ss.Cgroup, DNSPort: ss.Port, Loopback: ss.Loopback, LogGroup: s.cfg.LogGroup}); err != nil {
		undo()
		return "", err
	}
	if err := s.save(ss); err != nil {
		_ = s.fw.Remove(ss.Nft)
		undo()
		return "", err
	}
	addr := net.JoinHostPort(s.cfg.Listen, strconv.Itoa(ss.Port))
	s.logf("session %s registered: uid %d, cgroup %s, dns %s, %d entries", ss.ID, ss.UID, ss.Cgroup, addr, len(ss.Allow))
	s.record(ss, "egress.session.start", "ok", map[string]any{"cgroup": ss.Cgroup, "dns": addr,
		"allow": ss.Allow, "loopback": ss.Loopback, "policy": "default-deny"})
	return addr, nil
}

func (s *Server) save(ss *session) error {
	ss.mu.Lock()
	st := ss.State
	st.Allow = entryStrings(ss.list)
	ss.mu.Unlock()
	b, _ := json.Marshal(st)
	tmp := filepath.Join(s.cfg.StateDir, "."+ss.ID+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.cfg.StateDir, ss.ID+".json"))
}

// allow widens a session: root only (basalt-agent's polkit grant helper).
func (s *Server) allow(peer Peer, req Request) error {
	ss := s.get(req.Session)
	if ss == nil {
		return fmt.Errorf("no session %s", req.Session)
	}
	if peer.UID != 0 {
		s.record(ss, "egress.grant", "denied", map[string]any{"entry": req.Entry, "peer_uid": peer.UID,
			"reason": "allowlist changes need administrator authentication"})
		return errors.New("refused: allowlist changes come only from the administrator-authorized helper")
	}
	e, err := allowlist.ParseEntry(req.Entry)
	if err != nil {
		return err
	}
	ss.list.Add(e)
	if err := s.save(ss); err != nil {
		return err
	}
	s.record(ss, "egress.grant", "ok", map[string]any{"entry": e.String(), "by_uid": req.ByUID})
	return nil
}

func (s *Server) end(ss *session, why string) error {
	s.mu.Lock()
	if s.sessions[ss.ID] != ss {
		s.mu.Unlock()
		return nil
	}
	delete(s.sessions, ss.ID)
	delete(s.byNft, ss.Nft)
	s.mu.Unlock()
	ss.closeListeners()
	err := s.fw.Remove(ss.Nft)
	_ = os.Remove(filepath.Join(s.cfg.StateDir, ss.ID+".json"))
	ss.mu.Lock()
	data := map[string]any{"reason": why}
	for k, v := range ss.counts {
		data[k] = v
	}
	ss.mu.Unlock()
	s.record(ss, "egress.session.end", "ok", data)
	return err
}

// ---------------------------------------------------------------------------
// DNS

func (s *Server) listen(ss *session) error {
	addr := net.JoinHostPort(s.cfg.Listen, strconv.Itoa(ss.Port))
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	u, err := net.ListenUDP("udp", ua)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	ta, _ := net.ResolveTCPAddr("tcp", addr)
	t, err := net.ListenTCP("tcp", ta)
	if err != nil {
		u.Close()
		return fmt.Errorf("listen %s/tcp: %w", addr, err)
	}
	ss.udp, ss.tcp = u, t
	go s.serveUDP(ss)
	go s.serveTCP(ss)
	return nil
}

func (s *Server) serveUDP(ss *session) {
	buf := make([]byte, 4096)
	for {
		n, from, err := ss.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg := append([]byte(nil), buf[:n]...)
		go func() {
			if out := s.Answer(ss, msg, false); out != nil {
				_, _ = ss.udp.WriteToUDP(out, from)
			}
		}()
	}
}

func (s *Server) serveTCP(ss *session) {
	for {
		c, err := ss.tcp.AcceptTCP()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				msg := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, msg); err != nil {
					return
				}
				out := s.Answer(ss, msg, true)
				if out == nil {
					return
				}
				resp := binary.BigEndian.AppendUint16(nil, uint16(len(out)))
				if _, err := c.Write(append(resp, out...)); err != nil {
					return
				}
			}
		}()
	}
}

// match returns the ports of every entry that covers name (any port) and
// whether one of them allows private addresses.
func match(l *allowlist.List, name string) (ports []int, private, ok bool) {
	seen := map[int]bool{}
	for _, e := range l.Entries() {
		for _, p := range e.Ports {
			if e.Matches(name, p) {
				ok = true
				private = private || e.Private
				if !seen[p] {
					seen[p] = true
					ports = append(ports, p)
				}
			}
		}
	}
	sort.Ints(ports)
	return ports, private, ok
}

// Global reports whether ip is a public unicast address (the same rule as
// basalt-agent's proxy).
func Global(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1]&0xc0 == 64 || v4[0] == 0 || v4[0] >= 240 {
			return false
		}
	}
	return true
}

func (ss *session) count(k string) {
	ss.mu.Lock()
	ss.counts[k]++
	ss.mu.Unlock()
}

// throttle reports whether a record for key may be written now (one per
// key per minute per session; the counters keep the totals).
func (ss *session) throttle(key string) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := time.Now()
	if t, ok := ss.lastDeny[key]; ok && now.Sub(t) < time.Minute {
		return false
	}
	if len(ss.lastDeny) > 4096 {
		ss.lastDeny = map[string]time.Time{}
	}
	ss.lastDeny[key] = now
	return true
}

// Answer handles one DNS message from a session and returns the reply
// (nil: no reply).
func (s *Server) Answer(ss *session, msg []byte, tcp bool) []byte {
	q, err := dnsmsg.ParseQuery(msg)
	if err != nil {
		if len(msg) >= 12 && msg[2]&0x80 == 0 {
			out, _ := dnsmsg.Reply(dnsmsg.Query{Header: dnsmsg.Header{ID: binary.BigEndian.Uint16(msg)}}, dnsmsg.RcodeFormErr)
			return out
		}
		return nil
	}
	name, qtype := q.Question.Name, q.Question.Type
	refuse := func(reason, event string) []byte {
		ss.count("dns_refused")
		if ss.throttle(event + " " + name) {
			s.record(ss, event, "denied", map[string]any{"name": name, "qtype": dnsmsg.TypeName(qtype), "reason": reason})
		}
		out, _ := dnsmsg.Reply(q, dnsmsg.RcodeRefused)
		return out
	}
	if q.Question.Class != dnsmsg.ClassINET || qtype == dnsmsg.TypeANY {
		return refuse("query type or class not served", "dns.deny")
	}
	ports, private, ok := match(ss.list, name)
	if !ok {
		return refuse("not in the session allowlist", "dns.deny")
	}
	switch qtype {
	case dnsmsg.TypeHTTPS, dnsmsg.TypeSVCB:
		// Service bindings carry address hints and alternative names the
		// session sets do not cover: answer "no data", clients fall back to
		// A and AAAA.
		out, _ := dnsmsg.Reply(q, dnsmsg.RcodeSuccess)
		return out
	case dnsmsg.TypeA, dnsmsg.TypeAAAA, dnsmsg.TypeCNAME:
		return s.answerAddresses(ss, q, ports, private, tcp)
	}
	// Other record types of an allowed name (TXT, MX, SRV...) carry no
	// address the session can use: passed through unchanged.
	up, err := s.forward(msg, q)
	if err != nil {
		ss.count("dns_errors")
		out, _ := dnsmsg.Reply(q, dnsmsg.RcodeServFail)
		return out
	}
	return up
}

// forward sends the client's query upstream with a fresh id and returns
// the answer with the client's id restored.
func (s *Server) forward(msg []byte, q dnsmsg.Query) ([]byte, error) {
	id := randomID()
	fwd := append([]byte(nil), msg...)
	dnsmsg.SetID(fwd, id)
	up, err := s.Exchange(fwd)
	if err != nil {
		return nil, err
	}
	if len(up) < 12 || binary.BigEndian.Uint16(up) != id {
		return nil, errors.New("upstream answer does not match")
	}
	dnsmsg.SetID(up, q.Header.ID)
	return up, nil
}

func (s *Server) answerAddresses(ss *session, q dnsmsg.Query, ports []int, private, tcp bool) []byte {
	name := q.Question.Name
	fail := func() []byte {
		ss.count("dns_errors")
		out, _ := dnsmsg.Reply(q, dnsmsg.RcodeServFail)
		return out
	}
	query, err := dnsmsg.NewQuery(randomID(), name, q.Question.Type, true)
	if err != nil {
		return fail()
	}
	id := binary.BigEndian.Uint16(query)
	up, err := s.Exchange(query)
	if err != nil {
		s.logf("session %s: upstream %s %s: %v", ss.ID, name, dnsmsg.TypeName(q.Question.Type), err)
		return fail()
	}
	a, err := dnsmsg.ParseAnswer(up, q.Question, id)
	if err != nil {
		return fail()
	}
	if a.Header.Rcode() != dnsmsg.RcodeSuccess {
		out, _ := dnsmsg.Reply(q, a.Header.Rcode())
		return out
	}
	var keep []dnsmsg.RR
	var accepted, refused []string
	var els []nft.Element
	for _, rr := range a.Answers {
		if rr.Type == dnsmsg.TypeCNAME {
			keep = append(keep, rr)
			continue
		}
		if !private && !Global(rr.IP) {
			refused = append(refused, rr.IP.String())
			continue
		}
		ttl := time.Duration(rr.TTL) * time.Second
		ttl = min(max(ttl, s.cfg.MinTTL), s.cfg.MaxTTL)
		for _, p := range ports {
			els = append(els, nft.Element{IP: rr.IP, Port: p, Timeout: ttl + s.cfg.Grace})
		}
		// The client must not cache the address longer than the set holds it.
		rr.TTL = uint32(min(time.Duration(rr.TTL)*time.Second, ttl) / time.Second)
		keep = append(keep, rr)
		accepted = append(accepted, rr.IP.String())
	}
	if len(refused) > 0 {
		ss.count("dns_rebinding")
		if ss.throttle("rebinding " + name) {
			s.record(ss, "dns.rebinding", "denied", map[string]any{"name": name, "qtype": dnsmsg.TypeName(q.Question.Type),
				"addresses": refused, "reason": "an allowed name pointed at a local or private address; the entry is not marked private"})
		}
		if len(accepted) == 0 {
			out, _ := dnsmsg.Reply(q, dnsmsg.RcodeRefused)
			return out
		}
	}
	if len(els) > 0 {
		if err := s.fw.AddElements(ss.Nft, els); err != nil {
			s.logf("session %s: nft elements for %s: %v", ss.ID, name, err)
			return fail()
		}
	}
	ss.count("dns_answered")
	key := name + " " + dnsmsg.TypeName(q.Question.Type)
	ss.mu.Lock()
	first := !ss.seenAllow[key]
	ss.seenAllow[key] = true
	ss.mu.Unlock()
	if first && len(accepted) > 0 {
		s.record(ss, "dns.allow", "allowed", map[string]any{"name": name, "qtype": dnsmsg.TypeName(q.Question.Type),
			"addresses": accepted, "ports": ports})
	}
	limit := dnsmsg.MaxUDP(q)
	if tcp {
		limit = 65535
	}
	out, err := dnsmsg.Build(q, keep, limit)
	if err != nil {
		return fail()
	}
	return out
}

func randomID() uint16 {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

// exchangeUpstream sends query to the upstream resolver over UDP (two
// tries) and over TCP when the answer is truncated.
func (s *Server) exchangeUpstream(query []byte) ([]byte, error) {
	var last error
	for try := 0; try < 2; try++ {
		c, err := net.DialTimeout("udp", s.cfg.Upstream, 2*time.Second)
		if err != nil {
			return nil, err
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write(query); err != nil {
			c.Close()
			last = err
			continue
		}
		buf := make([]byte, 65535)
		n, err := c.Read(buf)
		c.Close()
		if err != nil {
			last = err
			continue
		}
		if n >= 12 && buf[2]&0x02 != 0 { // TC: retry over TCP
			return s.exchangeTCP(query)
		}
		return buf[:n], nil
	}
	return nil, last
}

func (s *Server) exchangeTCP(query []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", s.cfg.Upstream, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(query))), query...)); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	out := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err = io.ReadFull(c, out)
	return out, err
}

// ---------------------------------------------------------------------------
// Dropped packets (nflog)

// Dropped turns a logged packet into a ledger record.
func (s *Server) Dropped(p nflog.Packet) {
	name, kind, ok := nft.ParsePrefix(p.Prefix)
	if !ok {
		return
	}
	s.mu.Lock()
	ss := s.byNft[name]
	s.mu.Unlock()
	if ss == nil {
		return
	}
	dst := ""
	if p.Dst != nil {
		dst = p.Dst.String()
	}
	event, reason := "egress.drop", "no allowed name resolved to this address and port"
	if kind == "dns" {
		event, reason = "dns.direct", "DNS to a server other than the session resolver"
	}
	ss.count(strings.ReplaceAll(event, ".", "_"))
	if !ss.throttle(fmt.Sprintf("%s %s %s %d", event, p.Proto, dst, p.DPort)) {
		return
	}
	s.record(ss, event, "denied", map[string]any{"dst": dst, "port": p.DPort, "proto": p.Proto, "reason": reason})
}
