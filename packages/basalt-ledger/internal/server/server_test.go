package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/record"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/sign"
	"github.com/basalt-os/basalt-os/packages/basalt-ledger/internal/store"
)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.jsonl"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	key, err := sign.LoadOrCreate(filepath.Join(dir, "keys", "k"))
	if err != nil {
		t.Fatal(err)
	}
	return New(DefaultConfig(), st, key, t.Logf)
}

func intp(i int) *int { return &i }

var dev = Peer{UID: 1000, PID: 4242, Context: "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023"}
var other = Peer{UID: 1001, PID: 4343, Context: "unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023"}

func agentRec(event string) record.Incoming {
	return record.Incoming{V: 1, Producer: "basalt-agent", Session: "s-0123456789ab", Event: event, Outcome: "ok",
		Subject: record.Subject{Profile: "labshell", Mode: "native", Level: "s0:c1,c2", Project: "/home/dev/src/app"},
		Data:    map[string]any{"host": "example.com", "port": 443}}
}

func TestProducerRules(t *testing.T) {
	l := newLedger(t)
	if _, err := l.Accept(dev, agentRec("session.start")); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*record.Incoming){
		"another user's uid": func(r *record.Incoming) { r.UID = intp(0) },
		"reserved producer":  func(r *record.Incoming) { r.Producer = "selinux" },
		"ledger itself":      func(r *record.Incoming) { r.Producer = record.Producer },
		"trusted producer":   func(r *record.Incoming) { r.Producer = "basalt-resolver" },
		"unknown producer":   func(r *record.Incoming) { r.Producer = "my-tool" },
		"bad outcome":        func(r *record.Incoming) { r.Outcome = "fine" },
		"bad version":        func(r *record.Incoming) { r.V = 2 },
		"bad event":          func(r *record.Incoming) { r.Event = "Rewrite History!" },
	}
	for name, f := range bad {
		in := agentRec("egress.deny")
		f(&in)
		if _, err := l.Accept(dev, in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A confined agent domain is refused even with a valid record.
	agent := Peer{UID: 1000, PID: 1, Context: "unconfined_u:unconfined_r:basalt_agent_t:s0:c1,c2"}
	if _, err := l.Accept(agent, agentRec("egress.deny")); err == nil {
		t.Error("agent domain accepted")
	}
	// A Basalt desktop app domain reads, never writes.
	app := Peer{UID: 1000, PID: 2, Context: "unconfined_u:unconfined_r:basalt_app_secact_t:s0-s0:c0.c1023"}
	if _, err := l.Accept(app, agentRec("egress.deny")); err == nil {
		t.Error("desktop app domain accepted")
	}
	if recs, err := l.Query(app, Filter{}); err != nil || len(recs) == 0 {
		t.Errorf("desktop app domain cannot read its user's records: %v", err)
	}
	// The resolver: root in its own domain only.
	res := record.Incoming{V: 1, Producer: "basalt-resolver", UID: intp(1000), Event: "dns.deny", Outcome: "denied"}
	if _, err := l.Accept(Peer{UID: 0, Context: "system_u:system_r:basalt_resolver_t:s0"}, res); err != nil {
		t.Errorf("resolver refused: %v", err)
	}
	if _, err := l.Accept(Peer{UID: 0, Context: "unconfined_u:unconfined_r:unconfined_t:s0"}, res); err == nil {
		t.Error("resolver records accepted from another domain")
	}
	// The approval gate: root in basalt_gate_t only.
	gate := record.Incoming{V: 1, Producer: "basalt-gate", UID: intp(1000), Event: "gate.decision", Outcome: "allowed"}
	if _, err := l.Accept(Peer{UID: 0, Context: "system_u:system_r:basalt_gate_t:s0"}, gate); err != nil {
		t.Errorf("gate refused: %v", err)
	}
	for _, p := range []Peer{{UID: 0, Context: "unconfined_u:unconfined_r:unconfined_t:s0"}, dev,
		{UID: 1000, Context: "unconfined_u:unconfined_r:basalt_gate_tty_t:s0"}} {
		if _, err := l.Accept(p, gate); err == nil {
			t.Errorf("gate records accepted from %+v", p)
		}
	}
	// Refusals are recorded, visible to the user who was refused.
	recs, _ := l.Query(dev, Filter{Event: record.EventRefused})
	if len(recs) == 0 {
		t.Fatal("refusals not recorded")
	}
}

func TestNoRewrite(t *testing.T) {
	l := newLedger(t)
	for i := 0; i < 3; i++ {
		if rep := l.Handle(dev, Request{Op: "append", Record: ptr(agentRec("egress.allow"))}); !rep.OK {
			t.Fatal(rep.Error)
		}
	}
	before, _ := store.Verify(l.st.Path)
	// Try to overwrite record 2 by sending seq/prev/hash: it becomes a new
	// record, the producer's values are only kept as its source position.
	in := agentRec("egress.allow")
	in.Seq, in.Prev, in.Hash = 2, strings.Repeat("0", 64), strings.Repeat("f", 64)
	rep := l.Handle(dev, Request{Op: "append", Record: &in})
	if !rep.OK || rep.Seq != before.LastSeq+1 {
		t.Fatalf("%+v", rep)
	}
	for _, op := range []string{"delete", "update", "truncate", "rewrite", "set", ""} {
		if rep := l.Handle(dev, Request{Op: op}); rep.OK || !strings.Contains(rep.Error, "append-only") {
			t.Errorf("op %q: %+v", op, rep)
		}
	}
	if rep := l.Handle(dev, Request{Op: "rotate"}); rep.OK {
		t.Error("user rotated the ledger")
	}
	after, err := store.Verify(l.st.Path)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := l.Query(Peer{UID: 0}, Filter{})
	if recs[1].Seq != 2 || recs[1].Src != nil {
		t.Fatalf("record 2 changed: %+v", recs[1])
	}
	last, _ := l.Query(dev, Filter{Event: "egress.allow", Limit: 1})
	if last[0].Src == nil || last[0].Src.Seq != 2 || after.LastSeq <= before.LastSeq {
		t.Fatalf("%+v", last[0])
	}
	refused, _ := l.Query(dev, Filter{Event: record.EventRefused})
	if len(refused) < 5 {
		t.Fatalf("refused ops recorded: %d", len(refused))
	}
}

func ptr(r record.Incoming) *record.Incoming { return &r }

func TestReadIsolation(t *testing.T) {
	l := newLedger(t)
	_, _ = l.Accept(dev, agentRec("session.start"))
	o := agentRec("session.start")
	o.Session = "s-aaaaaaaaaaaa"
	_, _ = l.Accept(other, o)
	mine, _ := l.Query(dev, Filter{})
	for _, r := range mine {
		if r.UID != 1000 {
			t.Fatalf("dev sees uid %d", r.UID)
		}
	}
	// Asking for another uid explicitly does not help.
	theirs, _ := l.Query(dev, Filter{UID: intp(1001)})
	for _, r := range theirs {
		if r.UID != 1000 {
			t.Fatal("uid filter overrides isolation")
		}
	}
	all, _ := l.Query(Peer{UID: 0}, Filter{Event: "session.start"})
	if len(all) != 2 {
		t.Fatalf("root sees %d", len(all))
	}
}

func TestFiltersAndAVCAttribution(t *testing.T) {
	l := newLedger(t)
	_, _ = l.Accept(dev, agentRec("session.start"))
	d := agentRec("egress.deny")
	d.Outcome = "denied"
	_, _ = l.Accept(dev, d)
	avc := record.Record{Producer: "selinux", UID: -1, Event: "selinux.avc", Outcome: "denied",
		Subject: record.Subject{Level: "s0:c1,c2", App: "basalt_agent_t"}}
	r, err := l.Internal("journal", avc)
	if err != nil {
		t.Fatal(err)
	}
	if r.Session != "s-0123456789ab" || r.UID != 1000 || r.Subject.Profile != "labshell" {
		t.Fatalf("AVC not attributed: %+v", r)
	}
	if got, _ := l.Query(dev, Filter{Agent: "labshell", Severity: "warning"}); len(got) != 2 {
		t.Fatalf("warning filter: %d", len(got))
	}
	if got, _ := l.Query(dev, Filter{Project: "/home/dev/src"}); len(got) != 3 {
		t.Fatalf("project filter: %d", len(got))
	}
	if got, _ := l.Query(dev, Filter{Project: "/home/dev/sr"}); len(got) != 0 {
		t.Fatalf("project prefix must be a directory: %d", len(got))
	}
	if got, _ := l.Query(dev, Filter{Event: "selinux."}); len(got) != 1 {
		t.Fatalf("event prefix: %d", len(got))
	}
	if got, _ := l.Query(dev, Filter{App: "basalt_agent_t"}); len(got) != 1 {
		t.Fatalf("app filter: %d", len(got))
	}
	s := l.Handle(dev, Request{Op: "summary"})
	if !s.OK || len(s.Summary.Sessions) != 1 || s.Summary.Sessions[0].Denials != 1 {
		t.Fatalf("%+v", s.Summary)
	}
	_, _ = l.Accept(dev, agentRec("session.end"))
	// After the session ended, the level is no longer attributed.
	r, _ = l.Internal("journal", avc)
	if r.Session != "" {
		t.Fatal("attributed to an ended session")
	}
}

func TestExport(t *testing.T) {
	l := newLedger(t)
	_, _ = l.Accept(dev, agentRec("session.start"))
	_, _ = l.Accept(dev, agentRec("egress.allow"))
	rep := l.Handle(dev, Request{Op: "export"})
	if !rep.OK {
		t.Fatal(rep.Error)
	}
	e := *rep.Export
	if _, err := sign.Verify(e, l.key.Public); err != nil {
		t.Fatal(err)
	}
	if !e.Chain.Verified || len(e.Records) != 2 {
		t.Fatalf("%+v", e.Chain)
	}
	e.Records[0].Outcome = "denied"
	if _, err := sign.Verify(e, nil); err == nil {
		t.Fatal("tampered export verified")
	}
}
