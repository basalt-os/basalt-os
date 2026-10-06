// Package server is basalt-gated's core: the only place where a request
// for a side effect becomes a decision. It accepts requests over a Unix
// socket (newline-delimited JSON, PROTOCOL.md), identifies every peer from
// the kernel (SO_PEERCRED, SO_PEERSEC, the session cgroup), validates and
// classifies requests with the action registry, decides with the policy
// engine, keeps the queue of what a person must decide, hands single-use
// claims to executors bound to the digest, and records everything in the
// ledger. It never runs an action itself (rule changes, its own state,
// are the exception).
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/user"

	"strconv"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/config"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/peer"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/policy"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/polkit"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/registry"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/store"
	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// Version is set by the daemon at build time.
var Version = "dev"

// Recorder receives ledger records (the asynchronous ledger client).
type Recorder interface {
	Send(ledger.Record)
}

// Options wire a server.
type Options struct {
	Config  config.Config
	Reg     *registry.Registry
	Limits  *policy.HardLimits
	Presets map[string][]policy.Rule
	Store   *store.Store
	Ledger  Recorder
	Polkit  polkit.Checker
	// Now and Env default to the clock and the machine's conditions.
	Now func() time.Time
	Env func(time.Time) policy.Env
	// Home returns a user's home directory (default: the user database).
	Home registry.HomeFunc
	// AgentName returns the profile of a basalt-agent session (default:
	// the session.start record in the ledger).
	AgentName func(session string) string
	// History reads recorded gate records for dry runs (default: the
	// ledger, as root).
	History func(uid int, since time.Time) ([]ledger.Stored, error)
	Logf    func(string, ...any)
}

// Server is the daemon state.
type Server struct {
	o   Options
	cfg config.Config
	eng *policy.Engine
	now func() time.Time

	mu       sync.Mutex
	entries  map[string]*entry
	order    []string
	state    store.State
	preset   string
	sealOK   bool
	subs     map[chan gate.Event]subscriber
	buckets  map[string]*bucket
	refusals map[string]time.Time
	names    map[string]string
}

type subscriber struct {
	uid     int
	decider bool
	owner   string
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a server and loads (or installs) the rule set.
func New(o Options) (*Server, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Home == nil {
		o.Home = homeOf
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Env == nil {
		ps := o.Config.PowerSupply
		o.Env = func(t time.Time) policy.Env {
			return policy.Env{Now: t, Power: policy.PowerFrom(ps), Presence: policy.Unknown, Network: policy.Unknown}
		}
	}
	if o.Ledger == nil {
		o.Ledger = nopRecorder{}
	}
	s := &Server{o: o, cfg: o.Config, now: o.Now, entries: map[string]*entry{}, subs: map[chan gate.Event]subscriber{},
		buckets: map[string]*bucket{}, refusals: map[string]time.Time{}, names: map[string]string{}}
	s.eng = policy.NewEngine(o.Reg, o.Limits, o.Home)
	if err := s.loadRules(); err != nil {
		return nil, err
	}
	st, err := o.Store.LoadState()
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	if st.Paused == nil {
		st.Paused = map[string]string{}
	}
	s.state = st
	s.loadQueue()
	return s, nil
}

type nopRecorder struct{}

func (nopRecorder) Send(ledger.Record) {}

func homeOf(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// loadRules reads the sealed rule set; with none, it installs the default
// preset; with a broken seal, the careful preset only, and a critical
// record.
func (s *Server) loadRules() error {
	rules, preset, ok, err := s.o.Store.LoadRules()
	switch {
	case errors.Is(err, store.ErrSeal):
		bad, qerr := s.o.Store.QuarantineRules()
		s.record(ledger.Record{Event: "gate.seal_error", Outcome: "error", Data: map[string]any{
			"reason": "the rule file does not match its seal", "moved_to": bad, "move_error": errString(qerr)}})
		s.o.Logf("rules: %v; the file was moved to %s and only the careful preset is loaded", err, bad)
		return s.installPreset("careful", false)
	case err != nil:
		return err
	case !ok:
		return s.installPreset(s.cfg.DefaultPreset, true)
	}
	for i := range rules {
		if rules[i].Scope == "" {
			rules[i].Scope = policy.ScopeSystem
		}
	}
	s.eng.SetRules(rules)
	s.preset, s.sealOK = preset, true
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) installPreset(name string, sealOK bool) error {
	rules, ok := s.o.Presets[name]
	if !ok {
		return fmt.Errorf("preset %q is not installed", name)
	}
	now := s.now()
	out := make([]policy.Rule, 0, len(rules))
	for _, r := range rules {
		r.Created = &policy.Created{By: "preset", At: now.UTC().Format(time.RFC3339), Via: name}
		if err := policy.Validate(&r, s.o.Reg, policy.ScopeSystem, now); err != nil {
			return fmt.Errorf("preset %s, rule %s: %w", name, r.ID, err)
		}
		out = append(out, r)
	}
	if err := s.o.Store.SaveRules(out, name); err != nil {
		return err
	}
	s.eng.SetRules(out)
	s.preset, s.sealOK = name, sealOK
	return nil
}

// Engine returns the policy engine (tests, dry runs).
func (s *Server) Engine() *policy.Engine { return s.eng }

// ---------------------------------------------------------------------------
// Connections

// conn is one client connection.
type conn struct {
	p      peer.Peer
	roles  peer.Roles
	client string
	out    chan []byte // subscription events
}

func (c *conn) owner() string { return ownerKey(c.p, c.roles) }

// ownerKey identifies "the same requester" across connections: uid,
// SELinux context and, for agents, the session.
func ownerKey(p peer.Peer, r peer.Roles) string {
	return fmt.Sprintf("%d|%s|%s", p.UID, p.Context, r.Session)
}

// Serve accepts connections on ln until it is closed.
func (s *Server) Serve(ln *net.UnixListener) {
	go s.expireLoop()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.serveConn(c)
	}
}

func (s *Server) serveConn(c *net.UnixConn) {
	defer c.Close()
	p, err := peer.Of(c)
	if err != nil {
		return
	}
	cn := &conn{p: p, roles: s.cfg.Peers.Classify(p, "")}
	br := bufio.NewReaderSize(c, 64<<10)
	for {
		_ = c.SetDeadline(time.Now().Add(30 * time.Minute))
		line, err := readLine(br, 256<<10)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				writeReply(c, gate.Reply{Error: "request too large"})
			}
			return
		}
		var req gate.Request
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var rep gate.Reply
		if err := dec.Decode(&req); err != nil {
			rep = gate.Reply{Error: "bad request"}
		} else if req.Op == "subscribe" {
			s.subscribe(c, cn)
			return
		} else {
			rep = s.Handle(context.Background(), cn, req)
		}
		if !writeReply(c, rep) {
			return
		}
	}
}

func writeReply(c net.Conn, rep gate.Reply) bool {
	b, _ := json.Marshal(rep)
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.Write(append(b, '\n'))
	return err == nil
}

// NewConn returns a connection state for a peer (tests drive Handle
// with it).
func (s *Server) NewConn(p peer.Peer, client string) *conn {
	return &conn{p: p, roles: s.cfg.Peers.Classify(p, client), client: client}
}

// Handle answers one request.
func (s *Server) Handle(ctx context.Context, c *conn, req gate.Request) gate.Reply {
	switch req.Op {
	case "hello":
		return s.hello(c, req)
	case "propose":
		return s.propose(c, req, false)
	case "check":
		return s.propose(c, req, true)
	case "wait":
		return s.wait(c, req)
	case "status":
		if req.ID == "" {
			return s.gateStatus()
		}
		return s.status(c, req.ID)
	case "cancel":
		return s.cancel(c, req.ID)
	case "pending":
		return s.list(c, true, req.Limit)
	case "history":
		return s.list(c, false, req.Limit)
	case "decide":
		return s.decide(ctx, c, req)
	case "claim":
		return s.claim(c, req)
	case "result":
		return s.result(c, req)
	case "stop":
		return s.stop(c, req)
	case "resume":
		return s.resume(ctx, c)
	case "unlock":
		return s.unlock(c, req)
	case "rules.list":
		return s.rulesList(c)
	case "rules.draft":
		return s.rulesDraft(c, req)
	case "rules.simulate":
		return s.rulesSimulate(c, req)
	case "rules.apply":
		return s.rulesApply(ctx, c, req)
	case "rules.unpause":
		return s.rulesUnpause(ctx, c, req)
	}
	op := req.Op
	if len(op) > 40 {
		op = op[:40]
	}
	return gate.Reply{Error: fmt.Sprintf("unknown operation %q", op)}
}

func (s *Server) hello(c *conn, req gate.Request) gate.Reply {
	if req.Client != "" {
		if len(req.Client) > 80 {
			return gate.Reply{Error: "client name too long"}
		}
		c.client = req.Client
		c.roles = s.cfg.Peers.Classify(c.p, req.Client)
	}
	roles := []string{"requester"}
	if c.roles.Decider {
		roles = append(roles, "decider")
	}
	if !c.roles.Agent {
		roles = append(roles, "executor")
	}
	return gate.Reply{OK: true, Protocol: gate.Protocol, Roles: roles, Kind: c.roles.Kind}
}

func (s *Server) gateStatus() gate.Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := 0
	for _, e := range s.entries {
		if e.Status == gate.Asked {
			pending++
		}
	}
	st := &gate.Status{Protocol: gate.Protocol, Version: Version, Stopped: s.state.Stop.On, StoppedBy: s.state.Stop.By,
		StoppedAt: s.state.Stop.At, Preset: s.preset, Rules: len(s.eng.Rules()), Pending: pending, SealOK: s.sealOK,
		AllowCodeConfirm: s.cfg.AllowCodeConfirm, Actions: len(s.o.Reg.Actions)}
	return gate.Reply{OK: true, Protocol: gate.Protocol, Status: st}
}

// ---------------------------------------------------------------------------
// Ledger

func (s *Server) record(r ledger.Record) {
	if r.Outcome == "" {
		r.Outcome = "ok"
	}
	s.o.Ledger.Send(r)
}

// refuseOnce records a refused operation at most once per peer, operation
// and reason every ten seconds (a flood is one record).
func (s *Server) refuseOnce(key string, r ledger.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseOnceLocked(key, r)
}

func (s *Server) refuseOnceLocked(key string, r ledger.Record) {
	t, seen := s.refusals[key]
	if seen && s.now().Sub(t) < 10*time.Second {
		return
	}
	if len(s.refusals) > 4096 {
		s.refusals = map[string]time.Time{}
	}
	s.refusals[key] = s.now()
	s.record(r)
}

// peerWho names a peer in plain English for stop and resume records.
func (s *Server) peerWho(c *conn) string {
	switch {
	case c.roles.Agent:
		w := "An agent"
		if c.roles.Session != "" {
			w += " (session " + c.roles.Session + ")"
		}
		return w
	case c.roles.Decider:
		return fmt.Sprintf("uid %d in %s", c.p.UID, orNone(c.p.Type()))
	}
	return fmt.Sprintf("uid %d (%s)", c.p.UID, orNone(c.p.Type()))
}

func orNone(s string) string {
	if s == "" {
		return "no SELinux label"
	}
	return s
}

// ---------------------------------------------------------------------------
// Subscriptions

func (s *Server) subscribe(nc net.Conn, c *conn) {
	if !writeReply(nc, gate.Reply{OK: true, Protocol: gate.Protocol}) {
		return
	}
	ch := make(chan gate.Event, 64)
	s.mu.Lock()
	s.subs[ch] = subscriber{uid: c.p.UID, decider: c.roles.Decider, owner: c.owner()}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	// The client may only listen; reading tells us when it goes away.
	gone := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			if _, err := nc.Read(buf); err != nil {
				close(gone)
				return
			}
		}
	}()
	for {
		select {
		case ev := <-ch:
			_ = nc.SetDeadline(time.Time{})
			if !writeReply(nc, gate.Reply{OK: true, Event: &ev}) {
				return
			}
		case <-gone:
			return
		}
	}
}

// publish sends an event to subscribers who may see it (locked).
func (s *Server) publishLocked(ev gate.Event, e *entry) {
	ev.Time = s.now().UTC().Format(time.RFC3339)
	for ch, sub := range s.subs {
		if e != nil && !(sub.owner == e.Owner || (sub.decider && (sub.uid == 0 || sub.uid == e.P.Requester.UID || e.P.Requester.UID == 0))) {
			continue
		}
		select {
		case ch <- ev:
		default: // a slow subscriber misses events, never blocks the gate
		}
	}
}

// ---------------------------------------------------------------------------

var errLineTooLong = errors.New("line too long")

func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > max {
			return nil, errLineTooLong
		}
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// Status returns the gate's state.
func (s *Server) Status() gate.Status { return *s.gateStatus().Status }
