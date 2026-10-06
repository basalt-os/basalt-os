// SPDX-License-Identifier: MIT OR Apache-2.0

package gateclient

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Socket returns $BASALT_GATE_SOCKET or the default socket.
func Socket() string {
	if s := os.Getenv("BASALT_GATE_SOCKET"); s != "" {
		return s
	}
	return DefaultSocket
}

// ErrAbsent means no gate answers: a tool behaves exactly as it does on
// any other system (its own preview and confirmation). A tool never
// fails because the gate is missing.
var ErrAbsent = errors.New("no approval gate on this system")

// Client is one connection to the gate.
type Client struct {
	c     net.Conn
	br    *bufio.Reader
	Hello Reply // the gate's answer to hello
}

// Detect is the feature detection of the third-party subset: the gate is
// present when the socket exists and answers hello within 500 ms with a
// protocol of the same major version. Any other outcome returns ErrAbsent
// (wrapped with the reason).
func Detect(socket, client, role string) (*Client, error) {
	if socket == "" {
		socket = Socket()
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAbsent, err)
	}
	c, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAbsent, err)
	}
	cl := &Client{c: c, br: bufio.NewReaderSize(c, 1<<20)}
	rep, err := cl.do(Request{Op: "hello", Role: role, Client: client, Protocol: Protocol}, 500*time.Millisecond)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("%w: %v", ErrAbsent, err)
	}
	if !rep.OK || major(rep.Protocol) != major(Protocol) {
		c.Close()
		return nil, fmt.Errorf("%w: protocol %q", ErrAbsent, rep.Protocol)
	}
	cl.Hello = rep
	return cl, nil
}

func major(p string) string {
	name, ver, _ := strings.Cut(p, "/")
	m, _, _ := strings.Cut(ver, ".")
	return name + "/" + m
}

// Close closes the connection.
func (c *Client) Close() error { return c.c.Close() }

// Do sends one request and reads its reply (deadline: d, or 2 minutes).
func (c *Client) Do(req Request) (Reply, error) { return c.do(req, 0) }

func (c *Client) do(req Request, d time.Duration) (Reply, error) {
	var rep Reply
	b, err := json.Marshal(req)
	if err != nil {
		return rep, err
	}
	if d == 0 {
		d = 2 * time.Minute
		if req.Op == "wait" && req.Timeout > 0 {
			d = time.Duration(req.Timeout+30) * time.Second
		}
	}
	_ = c.c.SetDeadline(time.Now().Add(d))
	if _, err := c.c.Write(append(b, '\n')); err != nil {
		return rep, err
	}
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(line, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// replyErr turns a refused request into an error.
func replyErr(rep Reply, err error) (Reply, error) {
	if err != nil {
		return rep, err
	}
	if !rep.OK {
		return rep, errors.New(rep.Error)
	}
	return rep, nil
}

// Proposal is what a requester asks for.
type Proposal struct {
	Calls      []Call
	Preview    *Preview
	Group      string
	Ref        string
	Taint      string
	Deferrable bool
	ClassHint  string
	OnBehalf   *OnBehalf
	Via        []Party
}

// Propose submits a request. The reply's Decision is allowed, refused
// (Reason says why, to show), or asked (wait with the ID).
func (c *Client) Propose(p Proposal) (Reply, error) {
	return replyErr(c.Do(Request{Op: "propose", Calls: p.Calls, Preview: p.Preview, Group: p.Group, Ref: p.Ref,
		Taint: p.Taint, Deferrable: p.Deferrable, ClassHint: p.ClassHint, OnBehalf: p.OnBehalf, Via: p.Via}))
}

// Check asks "would this be allowed now?" without queueing anything.
func (c *Client) Check(p Proposal) (Reply, error) {
	return replyErr(c.Do(Request{Op: "check", Calls: p.Calls, Group: p.Group, Taint: p.Taint, ClassHint: p.ClassHint,
		OnBehalf: p.OnBehalf, Via: p.Via}))
}

// Wait blocks until the request is decided (allowed, declined, refused,
// expired, cancelled) or timeout seconds pass (TimedOut, Decision asked).
func (c *Client) Wait(id string, timeout int) (Reply, error) {
	if timeout <= 0 {
		timeout = 180
	}
	return replyErr(c.Do(Request{Op: "wait", ID: id, Timeout: timeout}))
}

// Status returns one request (own requests, or any for a decider).
func (c *Client) Status(id string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "status", ID: id}))
}

// GateStatus returns the gate's state.
func (c *Client) GateStatus() (Reply, error) { return replyErr(c.Do(Request{Op: "status"})) }

// Cancel withdraws an own pending request.
func (c *Client) Cancel(id string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "cancel", ID: id}))
}

// Claim takes an allowed request once, to run it; digest is what the
// executor computed from what it is about to run.
func (c *Client) Claim(id, digest, executor string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "claim", ID: id, Digest: digest, Executor: executor}))
}

// ClaimCalls takes an allowed request once, sending what the executor is
// about to run (its calls and, when it planned the preview, the preview)
// instead of a digest: the gate validates them through the registry,
// computes the digest exactly as it did for the request and compares.
func (c *Client) ClaimCalls(id string, calls []Call, pv *Preview, executor string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "claim", ID: id, Calls: calls, Preview: pv, Executor: executor}))
}

// Observe records what an approval path that does not use the gate yet
// decided by itself (shadow mode), with what the gate would have decided;
// nothing is queued or run. outcome: approved, declined, expired,
// refused, failed or cancelled; by: who decided it there.
func (c *Client) Observe(p Proposal, outcome, by string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "observe", Calls: p.Calls, Preview: p.Preview, Group: p.Group, Ref: p.Ref,
		Taint: p.Taint, ClassHint: p.ClassHint, OnBehalf: p.OnBehalf, Via: p.Via, Outcome: outcome, DecidedBy: by}))
}

// Confirm is root's confirmation at a terminal of a system assistant
// proposal (basalt apply ID, typed yes, or --yes --confirm CODE): the
// code is the proposal's short code. mode: "code" or "terminal".
func (c *Client) Confirm(id, code, mode string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "confirm", ID: id, Code: code, Mode: mode}))
}

// Enforced reports whether the gate decides for a migration path (from
// the hello reply); false means the component keeps its own
// confirmation and only observes.
func (c *Client) Enforced(path string) bool {
	for _, p := range c.Hello.Enforce {
		if p == path || p == "all" {
			return true
		}
	}
	return false
}

// Result reports the outcome of running a request.
func (c *Client) Result(id string, ok bool, exit int, detail string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "result", ID: id, OK: &ok, Exit: &exit, Detail: detail}))
}

// Report is Result with the snapshots the executor took.
func (c *Client) Report(id string, ok bool, exit int, detail string, snapshots []string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "result", ID: id, OK: &ok, Exit: &exit, Detail: detail, Snapshots: snapshots}))
}

// Stop is the emergency stop: every allow rule is suspended at once.
func (c *Client) Stop(reason string) (Reply, error) {
	return replyErr(c.Do(Request{Op: "stop", Reason: reason}))
}

// Resume ends the emergency stop (deciders only).
func (c *Client) Resume() (Reply, error) { return replyErr(c.Do(Request{Op: "resume"})) }

// Pending lists the requests waiting for a person (a requester sees its own).
func (c *Client) Pending() (Reply, error) { return replyErr(c.Do(Request{Op: "pending"})) }

// History lists decided requests, newest last.
func (c *Client) History(limit int) (Reply, error) {
	return replyErr(c.Do(Request{Op: "history", Limit: limit}))
}

// Decide approves or declines requests (deciders only).
func (c *Client) Decide(ids []string, approve, remember bool) (Reply, error) {
	return replyErr(c.Do(Request{Op: "decide", IDs: ids, Approve: &approve, Remember: remember}))
}

// RulesList lists the rules the client may read, with their sentences.
func (c *Client) RulesList() (Reply, error) { return replyErr(c.Do(Request{Op: "rules.list"})) }

// RulesSimulate replays recorded requests against a rule set (TOML or
// JSON text) and says what it would have allowed, asked and refused.
func (c *Client) RulesSimulate(rules, scope, since string, records json.RawMessage) (Reply, error) {
	raw, _ := json.Marshal(rules)
	return replyErr(c.Do(Request{Op: "rules.simulate", Rules: raw, Scope: scope, Since: since, Records: records}))
}

// Subscribe turns the connection into an event stream; f receives each
// event until it returns false or the connection ends.
func (c *Client) Subscribe(f func(Event) bool) error {
	if _, err := replyErr(c.Do(Request{Op: "subscribe"})); err != nil {
		return err
	}
	for {
		_ = c.c.SetDeadline(time.Time{})
		line, err := c.br.ReadBytes('\n')
		if err != nil {
			return err
		}
		var rep Reply
		if json.Unmarshal(line, &rep) != nil || rep.Event == nil {
			continue
		}
		if !f(*rep.Event) {
			return nil
		}
	}
}
