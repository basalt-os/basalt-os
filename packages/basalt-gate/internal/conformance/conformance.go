// Package conformance is the fake gate a client of the third-party subset
// (PROTOCOL.md) is tested against: it speaks only hello, propose (of
// tool.exec calls), wait and result, answers deterministically, and
// records every message that does not follow the protocol. Run it with
// `basalt-gate fake-server SOCKET`, point the client's $BASALT_GATE_SOCKET
// at it, and the client conforms when it reports no violations.
//
// Its answers:
//   - hello: protocol gate/1, roles requester and executor, kind tool;
//   - propose: refused when destructive is true or class_hint is C5
//     (as a locked disk wipe would be), allowed when class_hint is C0 or
//     C1, asked otherwise; ids are g-000000000001, g-000000000002, ...;
//   - wait: declined when the request's argv holds "--decline", else
//     allowed;
//   - result: ok for a known id.
//
// The replies are exactly the fixtures in testdata/protocol.
package conformance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
)

// FakeServer is the fake gate.
type FakeServer struct {
	mu         sync.Mutex
	next       int
	asked      map[string]fakeReq
	violations []string
	log        []string
}

type fakeReq struct {
	class   string
	decline bool
	result  bool
}

// NewFakeServer returns a fake gate.
func NewFakeServer() *FakeServer { return &FakeServer{asked: map[string]fakeReq{}} }

// Violations returns what clients did against the protocol.
func (f *FakeServer) Violations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.violations...)
}

// Log returns every request line received.
func (f *FakeServer) Log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *FakeServer) violate(format string, a ...any) {
	f.violations = append(f.violations, fmt.Sprintf(format, a...))
}

// Serve answers connections on ln until it is closed.
func (f *FakeServer) Serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go f.serve(c)
	}
}

func (f *FakeServer) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	st := &Conn{}
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if len(bytes.TrimSpace(line)) > 0 {
				f.mu.Lock()
				f.violate("a request without its newline: %q", line)
				f.mu.Unlock()
			}
			return
		}
		out := f.Answer(st, line)
		if _, err := c.Write(append(out, '\n')); err != nil {
			return
		}
	}
}

// Conn is the state of one client connection.
type Conn struct{ hello bool }

var (
	toolRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	idRe   = regexp.MustCompile(`^g-[0-9a-f]{12}$`)
)

// Answer returns the reply line for one request line.
func (f *FakeServer) Answer(st *Conn, line []byte) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, strings.TrimSpace(string(line)))
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	if err := dec.Decode(&m); err != nil {
		f.violate("not a JSON object: %v", err)
		return reply(map[string]any{"ok": false, "error": "bad request"})
	}
	var op string
	_ = json.Unmarshal(m["op"], &op)
	allowed := map[string][]string{
		"hello":   {"op", "role", "client", "protocol"},
		"propose": {"op", "calls", "group", "taint", "deferrable", "class_hint", "via"},
		"wait":    {"op", "id", "timeout"},
		"result":  {"op", "id", "ok", "exit", "detail"},
	}
	keys, known := allowed[op]
	if !known {
		f.violate("operation %q is not in the third-party subset", op)
		return reply(map[string]any{"ok": false, "error": fmt.Sprintf("unknown operation %q", op)})
	}
	for k := range m {
		ok := false
		for _, x := range keys {
			ok = ok || x == k
		}
		if !ok {
			f.violate("%s: unexpected member %q", op, k)
		}
	}
	if op != "hello" && !st.hello {
		f.violate("%s before hello", op)
	}
	switch op {
	case "hello":
		var h struct {
			Role, Client, Protocol string
		}
		_ = json.Unmarshal(line, &h)
		if h.Role != "requester" && h.Role != "" {
			f.violate("hello: a third-party tool is a requester, not %q", h.Role)
		}
		if h.Client == "" {
			f.violate("hello: no client name (tool/version)")
		}
		st.hello = true
		return reply(map[string]any{"ok": true, "protocol": "gate/1", "roles": []string{"requester", "executor"}, "kind": "tool"})
	case "propose":
		return f.propose(m)
	case "wait":
		var w struct {
			ID      string `json:"id"`
			Timeout *int   `json:"timeout"`
		}
		_ = json.Unmarshal(line, &w)
		r, ok := f.asked[w.ID]
		if !ok {
			f.violate("wait: unknown id %q", w.ID)
			return reply(map[string]any{"ok": false, "error": "no such request"})
		}
		if w.Timeout != nil && (*w.Timeout < 1 || *w.Timeout > 600) {
			f.violate("wait: timeout %d out of 1 to 600", *w.Timeout)
		}
		if r.decline {
			return reply(map[string]any{"ok": true, "id": w.ID, "decision": "declined", "class": r.class,
				"reason": "a person declined it", "by": "person:tty"})
		}
		return reply(map[string]any{"ok": true, "id": w.ID, "decision": "allowed", "class": r.class,
			"reason": "no rule covers it, so a person decides", "by": "person:tty"})
	case "result":
		var res struct {
			ID   string `json:"id"`
			OK   *bool  `json:"ok"`
			Exit *int   `json:"exit"`
		}
		if err := json.Unmarshal(line, &res); err != nil {
			f.violate("result: %v", err)
		}
		r, ok := f.asked[res.ID]
		if !ok {
			f.violate("result: unknown id %q", res.ID)
			return reply(map[string]any{"ok": false, "error": "no claimed request of yours with this id"})
		}
		if r.result {
			f.violate("result: sent twice for %s", res.ID)
		}
		if res.OK == nil && res.Exit == nil {
			f.violate("result: neither ok nor exit")
		}
		r.result = true
		f.asked[res.ID] = r
		return reply(map[string]any{"ok": true, "id": res.ID})
	}
	return nil
}

func (f *FakeServer) propose(m map[string]json.RawMessage) []byte {
	var calls []struct {
		Action string                     `json:"action"`
		Args   map[string]json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(m["calls"], &calls); err != nil || len(calls) == 0 {
		f.violate("propose: calls must be a non-empty array")
		return reply(map[string]any{"ok": true, "decision": "refused", "reason": "a request has 1 to 32 calls", "by": "registry"})
	}
	class, decline, refuse := "C0", false, false
	for _, c := range calls {
		if c.Action != "tool.exec" {
			f.violate("propose: action %q is not tool.exec", c.Action)
		}
		var tool, operation, desc, hint string
		var argv []string
		var destructive bool
		need := func(k string, dst any) {
			raw, ok := c.Args[k]
			if !ok {
				f.violate("tool.exec: %s is required", k)
				return
			}
			if err := json.Unmarshal(raw, dst); err != nil {
				f.violate("tool.exec: %s: %v", k, err)
			}
		}
		opt := func(k string, dst any) {
			if raw, ok := c.Args[k]; ok {
				if err := json.Unmarshal(raw, dst); err != nil {
					f.violate("tool.exec: %s: %v", k, err)
				}
			}
		}
		need("tool", &tool)
		need("operation", &operation)
		need("argv", &argv)
		opt("description", &desc)
		opt("destructive", &destructive)
		opt("class_hint", &hint)
		for k := range c.Args {
			switch k {
			case "tool", "operation", "argv", "description", "destructive", "class_hint":
			default:
				f.violate("tool.exec: unknown argument %q", k)
			}
		}
		if !toolRe.MatchString(tool) || !toolRe.MatchString(operation) {
			f.violate("tool.exec: tool and operation are lower-case names (%q, %q)", tool, operation)
		}
		if len(argv) == 0 {
			f.violate("tool.exec: argv is empty")
		}
		if len(desc) > 500 {
			f.violate("tool.exec: description longer than 500 bytes")
		}
		if hint == "" {
			hint = "C2"
		}
		if hint < "C0" || hint > "C5" || len(hint) != 2 {
			f.violate("tool.exec: class_hint %q", hint)
			hint = "C2"
		}
		if destructive {
			hint = "C5"
		}
		if hint > class {
			class = hint
		}
		refuse = refuse || destructive || hint == "C5"
		for _, a := range argv {
			decline = decline || a == "--decline"
		}
	}
	f.next++
	id := fmt.Sprintf("g-%012x", f.next)
	switch {
	case refuse:
		return reply(map[string]any{"ok": true, "id": id, "decision": "refused", "class": class,
			"reason": "always protected (tool.disk-wipe): wiping disks or partitions is never automatic or remote",
			"by":     "hard-limit:locked:tool.disk-wipe"})
	case class <= "C1":
		f.asked[id] = fakeReq{class: class}
		return reply(map[string]any{"ok": true, "id": id, "decision": "allowed", "class": class,
			"reason": "allowed by the rule r-tui-status, and you are told", "by": "rule:r-tui-status@1a2b3c4d"})
	}
	f.asked[id] = fakeReq{class: class, decline: decline}
	return reply(map[string]any{"ok": true, "id": id, "decision": "asked", "class": class,
		"reason": "no rule covers it, so a person decides", "by": "default"})
}

func reply(v map[string]any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ErrViolations is returned by Check when a client broke the protocol.
var ErrViolations = errors.New("protocol violations")

// Check returns ErrViolations with the list, or nil.
func (f *FakeServer) Check() error {
	v := f.Violations()
	if len(v) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrViolations, strings.Join(v, "; "))
}

// ValidID reports a request id of the protocol's form.
func ValidID(s string) bool { return idRe.MatchString(s) }
