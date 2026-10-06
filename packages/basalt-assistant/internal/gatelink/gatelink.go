// Package gatelink connects the system assistant's proposals to the
// approval gate (basalt-gate, docs/gate.md) when it is installed.
//
// A proposal becomes a gate request with the proposal's typed actions as
// calls, its id as the reference and a preview built here from the
// proposal (its title, one line per action, the exact command lines), so
// the short code in the queue is the fingerprint `basalt show` prints.
// Without a gate, or with an older one, nothing changes: `basalt apply`
// asks at the terminal as before. With a gate that only observes this
// path (shadow mode), `basalt apply` still decides by itself and tells the
// gate what was decided. Where the gate decides (enforce = apply in
// /etc/basalt-gate/gate.conf), the decision is the gate's, and the
// executor unit basalt-gate-exec@ID.service applies it.
package gatelink

import (
	"errors"
	"fmt"
	"os"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gateclient"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// Path is the migration path of the system assistant's proposals.
const Path = "apply"

// Executor is the registry name of the executor that applies them.
const Executor = "basalt-gate-exec"

// Modes of the path.
const (
	Absent  = ""        // no gate: the assistant's own confirmation
	Observe = "observe" // the gate only records what was decided
	Enforce = "enforce" // the gate decides
)

// Link is a connection to the gate.
type Link struct {
	C    *gateclient.Client
	Mode string
}

// Detect connects to the gate (500 ms at most) and reports the mode of
// this path. A missing or older gate is Absent, never an error.
func Detect(role string) *Link {
	if os.Getenv("BASALT_GATE") == "off" {
		return &Link{Mode: Absent}
	}
	c, err := gateclient.Detect("", "basalt-assistant/"+version(), role)
	if err != nil {
		return &Link{Mode: Absent}
	}
	l := &Link{C: c, Mode: Observe}
	if c.Enforced(Path) {
		l.Mode = Enforce
	}
	return l
}

// Version is set by the command (for the hello's client name).
var Version = "dev"

func version() string { return Version }

// Close closes the connection.
func (l *Link) Close() {
	if l != nil && l.C != nil {
		l.C.Close()
	}
}

// Calls are the proposal's actions as gate calls.
func Calls(p *proposal.Proposal) []gateclient.Call {
	out := make([]gateclient.Call, 0, len(p.Actions))
	for _, a := range p.Actions {
		args := map[string]any{}
		for k, v := range a.Params {
			args[k] = v
		}
		out = append(out, gateclient.Call{Action: a.Kind, Args: args})
	}
	return out
}

// Preview is what a person sees in the queue, built from the proposal
// alone (the executor rebuilds it to claim, so it must not depend on
// anything else): the title, one line per action with its parameters, and
// the exact command lines.
func Preview(p *proposal.Proposal) (*gateclient.Preview, error) {
	cmds, err := p.Commands()
	if err != nil {
		return nil, err
	}
	pv := &gateclient.Preview{TitleKey: "assistant.proposal.title",
		TitleArgs: map[string]any{"proposal": p.ID, "title": clip(p.Title, 300)}, Commands: cmds}
	for _, a := range p.Actions {
		args := map[string]any{}
		for k, v := range a.Params {
			args[k] = v
		}
		pv.Lines = append(pv.Lines, gateclient.Line{Key: "assistant.action." + a.Kind, Args: args})
	}
	return pv, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Request is the proposal as a gate request. The system assistant's own
// domain asks as itself; root's command line says it asks as the system
// assistant (the gate accepts that from root only).
func Request(p *proposal.Proposal) (gateclient.Proposal, error) {
	pv, err := Preview(p)
	if err != nil {
		return gateclient.Proposal{}, err
	}
	gp := gateclient.Proposal{Calls: Calls(p), Preview: pv, Ref: p.ID, Taint: "system"}
	if os.Geteuid() == 0 {
		gp.OnBehalf = &gateclient.OnBehalf{Kind: "system-assistant"}
	}
	return gp, nil
}

// Submit queues the proposal; the reply says allowed (a system rule),
// refused (a hard limit, a rule, the registry) or asked, with the id.
func (l *Link) Submit(p *proposal.Proposal) (gateclient.Reply, error) {
	if l == nil || l.C == nil {
		return gateclient.Reply{}, errors.New("no approval gate")
	}
	gp, err := Request(p)
	if err != nil {
		return gateclient.Reply{}, err
	}
	rep, err := l.C.Propose(gp)
	if err != nil {
		return rep, fmt.Errorf("approval gate: %w", err)
	}
	return rep, nil
}

// Observe tells the gate what the assistant's own confirmation decided
// (shadow mode). Best effort: a failure changes nothing.
func (l *Link) Observe(p *proposal.Proposal, outcome, by string) {
	if l == nil || l.C == nil || l.Mode != Observe {
		return
	}
	gp, err := Request(p)
	if err != nil {
		return
	}
	_, _ = l.C.Observe(gp, outcome, by)
}
