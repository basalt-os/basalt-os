// Package server is the basalt-ledger daemon's core: it accepts records
// from producers over a Unix socket, applies who-may-write-what rules,
// chains them into the store, and answers queries, summaries, chain
// verification and signed exports (the JSON API in docs/ledger.md).
//
// Nobody can change history through the socket: the protocol has no
// operation that edits or removes a record, the ledger assigns sequence
// numbers and hashes itself, and any other request is refused and the
// refusal itself recorded. Who may connect at all is SELinux's decision
// (agent domains may not); who may write as which producer, and read
// which records, is decided here from the kernel's view of the peer
// (SO_PEERCRED, SO_PEERSEC).
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/sign"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/store"
)

// Config of the ledger rules.
type Config struct {
	// UserProducers are the producer names unprivileged users may write
	// as (always for their own uid only).
	UserProducers []string
	// Host names the machine in exports.
	Host string
	// RateLimit per non-root uid: records per second and burst.
	Rate  float64
	Burst float64
}

// DefaultConfig is the packaged rule set.
func DefaultConfig() Config {
	h, _ := os.Hostname()
	return Config{UserProducers: []string{"basalt-agent", "basalt-shell"}, Host: h, Rate: 200, Burst: 2000}
}

// Reserved producer names: only the ledger's own collectors write them.
var Reserved = map[string]bool{record.Producer: true, "selinux": true, "journal": true, "polkit": true, "login": true,
	"snapper": true, "basalt-assistant": true, "sudo": true, "pkexec": true, "basalt-agent-grant": true}

// trustedProducers must come from root in a specific SELinux domain.
var trustedProducers = map[string]string{"basalt-resolver": "basalt_resolver_t"}

// Ledger is the daemon state.
type Ledger struct {
	cfg   Config
	st    *store.Store
	key   *sign.Key
	logf  func(string, ...any)
	limit map[int]*bucket

	mu       sync.Mutex
	levels   map[string]sessionInfo // SELinux level -> running agent session
	refusals map[string]time.Time
}

type sessionInfo struct {
	Session string
	UID     int
	Subject record.Subject
}

type bucket struct {
	tokens float64
	last   time.Time
	denied int
	note   time.Time
}

// New returns a ledger over st, signing exports with key.
func New(cfg Config, st *store.Store, key *sign.Key, logf func(string, ...any)) *Ledger {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Ledger{cfg: cfg, st: st, key: key, logf: logf, limit: map[int]*bucket{}, levels: map[string]sessionInfo{},
		refusals: map[string]time.Time{}}
}

// Store returns the underlying store.
func (l *Ledger) Store() *store.Store { return l.st }

// Peer is a socket client as the kernel sees it.
type Peer = record.Peer

var (
	producerRe = regexp.MustCompile(`^[a-z][a-z0-9.-]{1,39}$`)
	eventRe    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,63}$`)
	sessionRe  = regexp.MustCompile(`^[a-z0-9-]{0,40}$`)
)

// ContextType returns the type field of an SELinux context.
func ContextType(ctx string) string {
	f := strings.Split(ctx, ":")
	if len(f) >= 3 {
		return f[2]
	}
	return ""
}

// ErrRefused wraps every refusal of a producer's record.
var ErrRefused = errors.New("refused")

func refused(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// Accept validates a producer's record, applies the producer rules and
// appends it. Refusals are recorded (throttled) as ledger.refused.
func (l *Ledger) Accept(p Peer, in record.Incoming) (record.Record, error) {
	r, err := l.check(p, in)
	if err != nil {
		l.refuse(p, "append", err.Error())
		return r, err
	}
	out, err := l.st.Append(r)
	if err == nil {
		l.track(out)
	}
	return out, err
}

func (l *Ledger) check(p Peer, in record.Incoming) (record.Record, error) {
	var r record.Record
	if in.V != record.Version {
		return r, refused("schema version %d, want %d", in.V, record.Version)
	}
	if !producerRe.MatchString(in.Producer) {
		return r, refused("bad producer name")
	}
	if !eventRe.MatchString(in.Event) {
		return r, refused("bad event name")
	}
	if !record.Outcomes[in.Outcome] {
		return r, refused("bad outcome %q", in.Outcome)
	}
	if !sessionRe.MatchString(in.Session) {
		return r, refused("bad session id")
	}
	if t := ContextType(p.Context); strings.HasPrefix(t, "basalt_agent") || strings.HasPrefix(t, "container_") {
		return r, refused("confined agents cannot write to the ledger")
	}
	if Reserved[in.Producer] {
		return r, refused("producer %q is reserved for the ledger's own collectors", in.Producer)
	}
	uid := p.UID
	if in.UID != nil {
		uid = *in.UID
	}
	if dom, ok := trustedProducers[in.Producer]; ok {
		if p.UID != 0 || (p.Context != "" && ContextType(p.Context) != dom) {
			return r, refused("producer %q is accepted only from %s", in.Producer, dom)
		}
	} else if p.UID != 0 {
		allowed := false
		for _, n := range l.cfg.UserProducers {
			allowed = allowed || n == in.Producer
		}
		if !allowed {
			return r, refused("producer %q is not open to users", in.Producer)
		}
		if uid != p.UID {
			return r, refused("a producer may only record events of its own user (uid %d, record says %d)", p.UID, uid)
		}
		if !l.allowRate(p.UID) {
			return r, refused("too many records")
		}
	}
	var data json.RawMessage
	if len(in.Data) > 0 {
		b, err := json.Marshal(in.Data)
		if err != nil || len(b) > 16<<10 {
			return r, refused("data missing, unencodable or larger than 16 KiB")
		}
		data = b
	}
	for _, s := range []string{in.Subject.Profile, in.Subject.Mode, in.Subject.Level, in.Subject.Project, in.Subject.App} {
		if len(s) > 4096 {
			return r, refused("subject field too long")
		}
	}
	tm := in.Time
	if _, err := time.Parse(time.RFC3339Nano, tm); err != nil {
		tm = time.Now().UTC().Format(time.RFC3339Nano)
	}
	r = record.Record{Time: tm, Producer: in.Producer, UID: uid, Session: in.Session, Event: in.Event, Outcome: in.Outcome,
		Severity: record.Severity(in.Event, in.Outcome), Subject: in.Subject, Data: data, Peer: &record.Peer{UID: p.UID, PID: p.PID, Context: p.Context}}
	if in.Seq != 0 || in.Hash != "" || in.Prev != "" {
		r.Src = &record.Src{Seq: in.Seq, Hash: in.Hash, Prev: in.Prev}
	}
	return r, nil
}

func (l *Ledger) allowRate(uid int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.limit[uid]
	now := time.Now()
	if b == nil {
		b = &bucket{tokens: l.cfg.Burst, last: now}
		l.limit[uid] = b
	}
	b.tokens = min(l.cfg.Burst, b.tokens+now.Sub(b.last).Seconds()*l.cfg.Rate)
	b.last = now
	if b.tokens < 1 {
		b.denied++
		return false
	}
	b.tokens--
	return true
}

// refuse records a refused request, at most once per peer and reason
// every ten seconds.
func (l *Ledger) refuse(p Peer, op, reason string) {
	key := fmt.Sprintf("%d %s %s", p.UID, op, reason)
	l.mu.Lock()
	if t, ok := l.refusals[key]; ok && time.Since(t) < 10*time.Second {
		l.mu.Unlock()
		return
	}
	if len(l.refusals) > 4096 {
		l.refusals = map[string]time.Time{}
	}
	l.refusals[key] = time.Now()
	l.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"op": op, "reason": reason, "peer_uid": p.UID, "peer_pid": p.PID, "peer_context": p.Context})
	_, err := l.st.Append(record.Record{Time: time.Now().UTC().Format(time.RFC3339Nano), Producer: record.Producer, UID: p.UID,
		Event: record.EventRefused, Outcome: "denied", Severity: record.Severity(record.EventRefused, "denied"), Data: data,
		Peer: &record.Peer{UID: p.UID, PID: p.PID, Context: p.Context}})
	if err != nil {
		l.logf("recording a refusal: %v", err)
	}
}

// track keeps the SELinux level of running agent sessions, so SELinux
// denials can be attributed to the session that caused them.
func (l *Ledger) track(r record.Record) {
	if r.Producer != "basalt-agent" || r.Subject.Level == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch r.Event {
	case "session.start":
		l.levels[r.Subject.Level] = sessionInfo{Session: r.Session, UID: r.UID, Subject: r.Subject}
	case "session.end":
		delete(l.levels, r.Subject.Level)
	}
}

// Internal appends a record from one of the ledger's own collectors.
// Records with a level of a running agent session are attributed to it.
func (l *Ledger) Internal(collector string, r record.Record) (record.Record, error) {
	if r.Subject.Level != "" && r.Session == "" {
		l.mu.Lock()
		if si, ok := l.levels[r.Subject.Level]; ok {
			r.Session, r.UID = si.Session, si.UID
			lvl, app := r.Subject.Level, r.Subject.App
			r.Subject = si.Subject
			r.Subject.Level, r.Subject.App = lvl, app
		}
		l.mu.Unlock()
	}
	if r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if r.Outcome == "" {
		r.Outcome = "ok"
	}
	r.Severity = record.Severity(r.Event, r.Outcome)
	r.Peer = &record.Peer{UID: 0, PID: os.Getpid(), Collector: collector}
	return l.st.Append(r)
}

// ---------------------------------------------------------------------------
// Queries

// Filter selects records. Times are RFC 3339; they apply to the time the
// ledger received a record (producers cannot backdate into a window).
type Filter struct {
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
	Producer string `json:"producer,omitempty"`
	Event    string `json:"event,omitempty"` // exact, or a prefix ending in "."
	Session  string `json:"session,omitempty"`
	Agent    string `json:"agent,omitempty"`   // subject profile
	Project  string `json:"project,omitempty"` // subject project, or a directory above it
	App      string `json:"app,omitempty"`     // subject app or producer
	Severity string `json:"severity,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	UID      *int   `json:"uid,omitempty"`
	AfterSeq int64  `json:"after_seq,omitempty"`
	Limit    int    `json:"limit,omitempty"` // newest N (0: 1000)
}

type compiled struct {
	f            Filter
	since, until time.Time
	minSev       int
}

func compile(f Filter) (compiled, error) {
	c := compiled{f: f, minSev: -1}
	var err error
	if f.Since != "" {
		if c.since, err = time.Parse(time.RFC3339Nano, f.Since); err != nil {
			return c, fmt.Errorf("since: %w", err)
		}
	}
	if f.Until != "" {
		if c.until, err = time.Parse(time.RFC3339Nano, f.Until); err != nil {
			return c, fmt.Errorf("until: %w", err)
		}
	}
	if f.Severity != "" {
		if c.minSev = record.SeverityRank(f.Severity); c.minSev < 0 {
			return c, fmt.Errorf("unknown severity %q (info, notice, warning, critical)", f.Severity)
		}
	}
	return c, nil
}

func (c compiled) match(r record.Record) bool {
	f := c.f
	if f.AfterSeq > 0 && r.Seq <= f.AfterSeq {
		return false
	}
	if !c.since.IsZero() || !c.until.IsZero() {
		t, err := time.Parse(time.RFC3339Nano, r.Received)
		if err != nil || (!c.since.IsZero() && t.Before(c.since)) || (!c.until.IsZero() && t.After(c.until)) {
			return false
		}
	}
	if f.UID != nil && r.UID != *f.UID {
		return false
	}
	if f.Producer != "" && r.Producer != f.Producer {
		return false
	}
	if f.Event != "" && r.Event != f.Event && !(strings.HasSuffix(f.Event, ".") && strings.HasPrefix(r.Event, f.Event)) {
		return false
	}
	if f.Session != "" && r.Session != f.Session {
		return false
	}
	if f.Agent != "" && r.Subject.Profile != f.Agent {
		return false
	}
	if f.Project != "" {
		p := strings.TrimSuffix(f.Project, "/")
		if r.Subject.Project != p && !strings.HasPrefix(r.Subject.Project, p+"/") {
			return false
		}
	}
	if f.App != "" && r.Subject.App != f.App && r.Producer != f.App {
		return false
	}
	if f.Outcome != "" && r.Outcome != f.Outcome {
		return false
	}
	if c.minSev >= 0 && record.SeverityRank(r.Severity) < c.minSev {
		return false
	}
	return true
}

// Query returns the newest records matching f that p may read (oldest
// first). Users see the records of their own uid; root sees everything.
func (l *Ledger) Query(p Peer, f Filter) ([]record.Record, error) {
	if p.UID != 0 {
		uid := p.UID
		f.UID = &uid
	}
	c, err := compile(f)
	if err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 1000
	}
	if limit > 100000 {
		limit = 100000
	}
	var ring []record.Record
	err = store.Scan(l.st.Path, c.since, func(r record.Record) bool {
		if c.match(r) {
			ring = append(ring, r)
			if len(ring) > limit {
				ring = ring[len(ring)-limit:]
			}
		}
		return true
	})
	return ring, err
}

// Export runs a query and signs the result with the chain position.
func (l *Ledger) Export(p Peer, f Filter) (sign.Export, error) {
	if f.Limit == 0 {
		f.Limit = 100000
	}
	recs, err := l.Query(p, f)
	if err != nil {
		return sign.Export{}, err
	}
	fb, _ := json.Marshal(f)
	e := sign.Export{Generated: time.Now().UTC().Format(time.RFC3339), Host: l.cfg.Host, Filter: fb, Records: recs}
	sum, verr := store.Verify(l.st.Path)
	e.Chain = sign.ChainInfo{Verified: verr == nil, HeadSeq: sum.LastSeq, HeadHash: sum.LastHash}
	if verr != nil {
		e.Chain.Error = verr.Error()
	}
	if len(recs) > 0 {
		e.Chain.FirstSeq, e.Chain.LastSeq = recs[0].Seq, recs[len(recs)-1].Seq
	}
	if e.Records == nil {
		e.Records = []record.Record{}
	}
	if l.key == nil {
		return e, errors.New("no export key")
	}
	return e, sign.Sign(&e, l.key)
}

// ---------------------------------------------------------------------------
// The socket protocol

// Request is one line on the socket.
type Request struct {
	Op     string           `json:"op"`
	Record *record.Incoming `json:"record,omitempty"`
	Filter Filter           `json:"filter,omitempty"`
}

// Reply is the answer line.
type Reply struct {
	OK      bool                `json:"ok"`
	Error   string              `json:"error,omitempty"`
	Seq     int64               `json:"seq,omitempty"`
	Hash    string              `json:"hash,omitempty"`
	Records []View              `json:"records,omitempty"`
	Summary *record.Summary     `json:"summary,omitempty"`
	Verify  *store.Summary      `json:"verify,omitempty"`
	Export  *sign.Export        `json:"export,omitempty"`
	Rotate  *store.RotateResult `json:"rotate,omitempty"`
	Status  *Status             `json:"status,omitempty"`
}

// View is a record with its plain-English sentence.
type View struct {
	record.Record
	Text string `json:"text"`
}

// Status describes the ledger.
type Status struct {
	HeadSeq   int64  `json:"head_seq"`
	HeadHash  string `json:"head_hash"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	KeyID     string `json:"key_id"`
	KeyKind   string `json:"key_kind"`
	PeerUID   int    `json:"peer_uid"`
	SeesAll   bool   `json:"sees_all"`
	PublicKey string `json:"public_key_file,omitempty"`
}

// PublicKeyFile is reported by status (set by the daemon).
var PublicKeyFile string

// Handle answers one request from p.
func (l *Ledger) Handle(p Peer, req Request) Reply {
	fail := func(err error) Reply { return Reply{Error: err.Error()} }
	switch req.Op {
	case "append":
		if req.Record == nil {
			return fail(errors.New("append needs a record"))
		}
		r, err := l.Accept(p, *req.Record)
		if err != nil {
			return fail(err)
		}
		return Reply{OK: true, Seq: r.Seq, Hash: r.Hash}
	case "query":
		recs, err := l.Query(p, req.Filter)
		if err != nil {
			return fail(err)
		}
		out := make([]View, len(recs))
		for i, r := range recs {
			out[i] = View{Record: r, Text: record.Describe(r)}
		}
		return Reply{OK: true, Records: out}
	case "summary":
		recs, err := l.Query(p, req.Filter)
		if err != nil {
			return fail(err)
		}
		s := record.Summarize(recs)
		return Reply{OK: true, Summary: &s}
	case "verify":
		s, err := store.Verify(l.st.Path)
		if err != nil {
			return Reply{Error: "chain broken: " + err.Error(), Verify: &s}
		}
		if s.Truncated {
			// The ledger never removes its own files: missing oldest files
			// were removed by someone else.
			return Reply{Error: fmt.Sprintf("chain starts at record %d: the oldest ledger files were removed", s.FirstSeq), Verify: &s}
		}
		return Reply{OK: true, Verify: &s}
	case "export":
		e, err := l.Export(p, req.Filter)
		if err != nil {
			return fail(err)
		}
		return Reply{OK: true, Export: &e}
	case "status":
		h := l.st.Head()
		s := &Status{HeadSeq: h.Seq, HeadHash: h.Hash, Path: l.st.Path, Size: l.st.Size(), PeerUID: p.UID, SeesAll: p.UID == 0,
			PublicKey: PublicKeyFile}
		if l.key != nil {
			s.KeyID, s.KeyKind = sign.ID(l.key.Public), l.key.Kind
		}
		return Reply{OK: true, Status: s}
	case "rotate":
		if p.UID != 0 {
			l.refuse(p, "rotate", "rotation is for root only")
			return fail(errors.New("refused: rotation is for root only"))
		}
		res, err := l.st.Rotate()
		if err != nil {
			return fail(err)
		}
		return Reply{OK: true, Rotate: &res}
	}
	// Anything else (delete, update, truncate, rewrite...) does not exist:
	// the ledger is append-only. The attempt is itself recorded.
	op := req.Op
	if len(op) > 40 {
		op = op[:40]
	}
	l.refuse(p, op, "the ledger is append-only; no operation changes or removes records")
	return fail(fmt.Errorf("refused: unknown operation %q (the ledger is append-only)", op))
}

// Serve accepts connections on ln.
func (l *Ledger) Serve(ln *net.UnixListener) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go l.serveConn(c)
	}
}

func (l *Ledger) serveConn(c *net.UnixConn) {
	defer c.Close()
	p, err := PeerOf(c)
	if err != nil {
		return
	}
	br := bufio.NewReaderSize(c, 64<<10)
	for {
		_ = c.SetDeadline(time.Now().Add(10 * time.Minute))
		line, err := readLine(br, 256<<10)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				b, _ := json.Marshal(Reply{Error: "request too large"})
				_, _ = c.Write(append(b, '\n'))
			}
			return
		}
		var req Request
		var rep Reply
		if err := json.Unmarshal(line, &req); err != nil {
			rep = Reply{Error: "bad request"}
		} else {
			rep = l.Handle(p, req)
		}
		b, _ := json.Marshal(rep)
		if _, err := c.Write(append(b, '\n')); err != nil {
			return
		}
	}
}

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

// PeerOf returns the kernel's view of the client: uid and pid
// (SO_PEERCRED) and SELinux context (SO_PEERSEC, empty without SELinux).
func PeerOf(c *net.UnixConn) (Peer, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var p Peer
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, e := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		p.UID, p.PID = int(cred.Uid), int(cred.Pid)
		buf := make([]byte, 256)
		n := uint32(len(buf))
		_, _, e2 := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.SOL_SOCKET, syscall.SO_PEERSEC,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0)
		if e2 == 0 && n > 0 && int(n) <= len(buf) {
			p.Context = strings.TrimRight(string(buf[:n]), "\x00")
		}
	})
	if err != nil {
		return p, err
	}
	return p, cerr
}
