package server

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/dnsmsg"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/ledger"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/nflog"
	"github.com/basalt-os/basalt-os/packages/basalt-resolver/internal/nft"
)

type fakeFW struct {
	mu       sync.Mutex
	sessions map[string]nft.Session
	elems    map[string][]nft.Element
	failAdd  bool
}

func newFW() *fakeFW {
	return &fakeFW{sessions: map[string]nft.Session{}, elems: map[string][]nft.Element{}}
}
func (f *fakeFW) Init() error { return nil }
func (f *fakeFW) Add(s nft.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAdd {
		return fmt.Errorf("nft failed")
	}
	f.sessions[s.Name] = s
	return nil
}
func (f *fakeFW) Remove(n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, n)
	delete(f.elems, n)
	return nil
}
func (f *fakeFW) AddElements(n string, els []nft.Element) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.elems[n] = append(f.elems[n], els...)
	return nil
}
func (f *fakeFW) State() (nft.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := nft.State{Exists: true, Chains: map[string]bool{}, Sets: map[string]bool{}, Jumps: map[string][]nft.Jump{}}
	for n := range f.sessions {
		st.Chains[n] = true
	}
	return st, nil
}

type sink struct {
	mu   sync.Mutex
	recs []ledger.Record
}

func (s *sink) Send(r ledger.Record) {
	s.mu.Lock()
	s.recs = append(s.recs, r)
	s.mu.Unlock()
}
func (s *sink) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.recs {
		out = append(out, r.Event+":"+r.Outcome)
	}
	return out
}
func (s *sink) has(ev string) bool {
	for _, e := range s.events() {
		if e == ev {
			return true
		}
	}
	return false
}

// zone is the fake upstream: name -> A addresses.
var zone = map[string][]string{
	"api.example.com":    {"93.184.216.34"},
	"rebind.example.com": {"10.86.0.1"},
	"mixed.example.com":  {"93.184.216.35", "127.0.0.1"},
	"model.lab.example":  {"10.86.0.1"},
}

func fakeExchange(query []byte) ([]byte, error) {
	q, err := dnsmsg.ParseQuery(query)
	if err != nil {
		return nil, err
	}
	addrs, ok := zone[q.Question.Name]
	if !ok {
		return dnsmsg.Reply(q, dnsmsg.RcodeNXDomain)
	}
	var rrs []dnsmsg.RR
	for _, a := range addrs {
		if q.Question.Type == dnsmsg.TypeA {
			rrs = append(rrs, dnsmsg.RR{Name: q.Question.Name, Type: dnsmsg.TypeA, TTL: 20, IP: net.ParseIP(a)})
		}
	}
	return dnsmsg.Build(q, rrs, 0)
}

const testCgroup = "/user.slice/user-1000.slice/user@1000.service/basaltagent.slice/basaltagent-0123456789ab.slice"

func setup(t *testing.T) (*Server, *fakeFW, *sink) {
	t.Helper()
	dir := t.TempDir()
	cfg := Defaults()
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.CgroupRoot = filepath.Join(dir, "cg")
	cfg.Listen = "127.0.0.1"
	cfg.PortFirst, cfg.PortLast = 0, 0 // replaced per test below
	if err := os.MkdirAll(filepath.Join(cfg.CgroupRoot, testCgroup), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pick a free port range for the listeners.
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := l.LocalAddr().(*net.UDPAddr).Port
	l.Close()
	cfg.PortFirst, cfg.PortLast = p, p+3
	fw, sk := newFW(), &sink{}
	s := New(cfg, fw, sk, t.Logf)
	s.Exchange = fakeExchange
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, fw, sk
}

func ownUID() int { return os.Getuid() }

func register(t *testing.T, s *Server, allow ...string) *session {
	t.Helper()
	// The test cgroup is owned by the test user; register as that user
	// with a matching path when not root.
	uid := ownUID()
	cg := testCgroup
	if uid != 1000 && uid != 0 {
		cg = strings.ReplaceAll(testCgroup, "1000", fmt.Sprint(uid))
		_ = os.MkdirAll(filepath.Join(s.cfg.CgroupRoot, cg), 0o755)
	}
	rep := s.Handle(Peer{UID: uid}, Request{Op: "register", Session: "s-0123456789ab", Profile: "labshell", Cgroup: cg, Allow: allow})
	if !rep.OK {
		t.Fatalf("register: %s", rep.Error)
	}
	return s.get("s-0123456789ab")
}

func ask(t *testing.T, s *Server, ss *session, name string, qtype uint16) dnsmsg.Header {
	t.Helper()
	b, _ := dnsmsg.NewQuery(42, name, qtype, false)
	out := s.Answer(ss, b, false)
	if len(out) < 12 {
		t.Fatalf("%s: no answer", name)
	}
	return dnsmsg.Header{ID: binary.BigEndian.Uint16(out), Flags: binary.BigEndian.Uint16(out[2:]), ANCount: binary.BigEndian.Uint16(out[6:])}
}

func TestAllowedNameFillsSet(t *testing.T) {
	s, fw, sk := setup(t)
	ss := register(t, s, "api.example.com:443,80")
	h := ask(t, s, ss, "API.example.com", dnsmsg.TypeA)
	if h.Rcode() != dnsmsg.RcodeSuccess || h.ANCount != 1 || h.ID != 42 {
		t.Fatalf("header %+v", h)
	}
	els := fw.elems[ss.Nft]
	if len(els) != 2 || els[0].IP.String() != "93.184.216.34" || els[0].Port != 80 || els[1].Port != 443 {
		t.Fatalf("elements %+v", els)
	}
	if els[0].Timeout != s.cfg.MinTTL+s.cfg.Grace {
		t.Fatalf("timeout %v: TTL 20 s must be raised to the minimum", els[0].Timeout)
	}
	if !sk.has("dns.allow:allowed") || !sk.has("egress.session.start:ok") {
		t.Fatalf("records %v", sk.events())
	}
}

func TestUnlistedRefused(t *testing.T) {
	s, fw, sk := setup(t)
	ss := register(t, s, "api.example.com")
	for _, n := range []string{"example.org", "evil.api.example.com", "data-exfil.attacker.example"} {
		if h := ask(t, s, ss, n, dnsmsg.TypeA); h.Rcode() != dnsmsg.RcodeRefused {
			t.Errorf("%s: rcode %d", n, h.Rcode())
		}
	}
	if len(fw.elems[ss.Nft]) != 0 {
		t.Fatal("a refused name filled the set")
	}
	if !sk.has("dns.deny:denied") {
		t.Fatalf("records %v", sk.events())
	}
	if h := ask(t, s, ss, "api.example.com", dnsmsg.TypeANY); h.Rcode() != dnsmsg.RcodeRefused {
		t.Error("ANY served")
	}
}

func TestRebindingRefused(t *testing.T) {
	s, fw, sk := setup(t)
	ss := register(t, s, "rebind.example.com", "mixed.example.com", "model.lab.example:11434 private")
	if h := ask(t, s, ss, "rebind.example.com", dnsmsg.TypeA); h.Rcode() != dnsmsg.RcodeRefused {
		t.Fatalf("rebinding answered: %+v", h)
	}
	if !sk.has("dns.rebinding:denied") {
		t.Fatalf("records %v", sk.events())
	}
	// Mixed answer: only the public address is kept.
	if h := ask(t, s, ss, "mixed.example.com", dnsmsg.TypeA); h.Rcode() != 0 || h.ANCount != 1 {
		t.Fatalf("mixed: %+v", h)
	}
	// An entry marked private may resolve to a private address.
	if h := ask(t, s, ss, "model.lab.example", dnsmsg.TypeA); h.Rcode() != 0 || h.ANCount != 1 {
		t.Fatalf("private entry: %+v", h)
	}
	var ips []string
	for _, e := range fw.elems[ss.Nft] {
		ips = append(ips, fmt.Sprintf("%s:%d", e.IP, e.Port))
	}
	if got := strings.Join(ips, " "); got != "93.184.216.35:443 10.86.0.1:11434" {
		t.Fatalf("elements %s", got)
	}
}

func TestServiceBindingNoData(t *testing.T) {
	s, fw, _ := setup(t)
	ss := register(t, s, "api.example.com")
	if h := ask(t, s, ss, "api.example.com", dnsmsg.TypeHTTPS); h.Rcode() != 0 || h.ANCount != 0 {
		t.Fatalf("%+v", h)
	}
	if len(fw.elems[ss.Nft]) != 0 {
		t.Fatal("HTTPS query filled the set")
	}
}

func TestRegisterChecks(t *testing.T) {
	s, fw, _ := setup(t)
	uid := ownUID()
	if uid == 0 {
		uid = 1000
	}
	other := uid + 1
	cases := map[string]Request{
		"other user's cgroup": {Op: "register", Session: "s-aaaaaaaaaaaa", Cgroup: strings.ReplaceAll(testCgroup, "1000", fmt.Sprint(other))},
		"shared app.slice":    {Op: "register", Session: "s-aaaaaaaaaaaa", Cgroup: fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/app.slice", uid, uid)},
		"missing cgroup":      {Op: "register", Session: "s-aaaaaaaaaaaa", Cgroup: fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/nope.slice", uid, uid)},
		"dot dot":             {Op: "register", Session: "s-aaaaaaaaaaaa", Cgroup: fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service/../../x", uid, uid)},
		"bad id":              {Op: "register", Session: "S;drop", Cgroup: testCgroup},
		"IP entry":            {Op: "register", Session: "s-aaaaaaaaaaaa", Cgroup: testCgroup, Allow: []string{"1.1.1.1"}},
	}
	for name, req := range cases {
		if rep := s.Handle(Peer{UID: uid}, req); rep.OK {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(fw.sessions) != 0 {
		t.Fatalf("rules installed: %v", fw.sessions)
	}
}

func TestAllowOnlyFromRoot(t *testing.T) {
	s, _, sk := setup(t)
	ss := register(t, s, "api.example.com")
	if rep := s.Handle(Peer{UID: ss.UID + 1}, Request{Op: "allow", Session: ss.ID, Entry: "evil.example.com"}); rep.OK {
		t.Fatal("non-root widened a session")
	}
	if ss.UID != 0 {
		if rep := s.Handle(Peer{UID: ss.UID}, Request{Op: "allow", Session: ss.ID, Entry: "evil.example.com"}); rep.OK {
			t.Fatal("the session owner widened the session without admin auth")
		}
	}
	if !sk.has("egress.grant:denied") {
		t.Fatalf("records %v", sk.events())
	}
	if rep := s.Handle(Peer{UID: 0}, Request{Op: "allow", Session: ss.ID, Entry: "other.example.com", ByUID: 1000}); !rep.OK {
		t.Fatalf("root grant: %s", rep.Error)
	}
	if _, _, ok := match(ss.list, "other.example.com"); !ok {
		t.Fatal("grant not applied")
	}
}

func TestDroppedAndEnd(t *testing.T) {
	s, fw, sk := setup(t)
	ss := register(t, s, "api.example.com")
	s.Dropped(nflog.Packet{Prefix: nft.LogPrefix(ss.Nft, "drop"), Proto: "tcp", Dst: net.ParseIP("1.1.1.1"), DPort: 443})
	s.Dropped(nflog.Packet{Prefix: nft.LogPrefix(ss.Nft, "drop"), Proto: "tcp", Dst: net.ParseIP("1.1.1.1"), DPort: 443}) // throttled
	s.Dropped(nflog.Packet{Prefix: nft.LogPrefix(ss.Nft, "dns"), Proto: "udp", Dst: net.ParseIP("8.8.8.8"), DPort: 53})
	n := 0
	for _, e := range sk.events() {
		if e == "egress.drop:denied" {
			n++
		}
	}
	if n != 1 || !sk.has("dns.direct:denied") {
		t.Fatalf("records %v", sk.events())
	}
	if rep := s.Handle(Peer{UID: ss.UID + 7}, Request{Op: "end", Session: ss.ID}); rep.OK && ss.UID+7 != 0 {
		t.Fatal("another user ended the session")
	}
	if rep := s.Handle(Peer{UID: ss.UID}, Request{Op: "end", Session: ss.ID}); !rep.OK {
		t.Fatal(rep.Error)
	}
	if _, ok := fw.sessions[ss.Nft]; ok || !sk.has("egress.session.end:ok") {
		t.Fatal("session rules not removed")
	}
}

func TestRestoreAfterRestart(t *testing.T) {
	s, fw, _ := setup(t)
	ss := register(t, s, "api.example.com")
	s.Close()
	s2 := New(s.cfg, fw, &sink{}, t.Logf)
	s2.Exchange = fakeExchange
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.get(ss.ID)
	if got == nil || got.Port != ss.Port {
		t.Fatal("session not restored")
	}
	// A session whose cgroup is gone is dropped on the next start.
	s2.Close()
	if err := os.RemoveAll(filepath.Join(s.cfg.CgroupRoot, ss.Cgroup)); err != nil {
		t.Fatal(err)
	}
	s3 := New(s.cfg, fw, &sink{}, t.Logf)
	if err := s3.Start(); err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if s3.get(ss.ID) != nil || len(fw.sessions) != 0 {
		t.Fatal("stale session kept")
	}
}

func TestUDPListener(t *testing.T) {
	s, _, _ := setup(t)
	ss := register(t, s, "api.example.com")
	c, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", ss.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q, _ := dnsmsg.NewQuery(99, "api.example.com", dnsmsg.TypeA, true)
	if _, err := c.Write(q); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	a, err := dnsmsg.ParseAnswer(buf[:n], dnsmsg.Question{Name: "api.example.com", Type: dnsmsg.TypeA, Class: 1}, 99)
	if err != nil || len(a.Answers) != 1 {
		t.Fatalf("%v %+v", err, a)
	}
}
