package cli

// The approval gate (basalt-gate, docs/gate.md): on Basalt OS the one
// place where a request becomes a decision. basalt-agent grant and egress
// propose are requests of the person's own tool:
//
//   - no gate: nothing changes (the terminal's confirmation and polkit);
//   - the gate only observes this path (shadow mode): nothing changes,
//     and the outcome is reported to the gate;
//   - the gate decides (enforce = agent in /etc/basalt-gate/gate.conf):
//     the request goes to the gate; a rule may decide at once; otherwise
//     the person approves it, here through the terminal decider
//     (basalt-gate approve, their own password) or anywhere else in the
//     queue; once allowed, basalt-agent carries it out as before (the root
//     helper still asks for an administrator through pkexec).
//
// An agent session never gets here (inAgentDomain, and the gate refuses
// these actions from agent domains).

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/gateclient"
)

// gatePath is this tool's migration path in the gate's configuration.
const gatePath = "agent"

// gateDecider is the terminal decider's command.
var gateDecider = "/usr/bin/basalt-gate"

// gateLink is a connection to the gate and the mode of this path.
type gateLink struct {
	c    *gateclient.Client
	mode string // "", "observe", "enforce"
}

func dialGate() *gateLink {
	if os.Getenv("BASALT_GATE") == "off" {
		return &gateLink{}
	}
	c, err := gateclient.Detect("", "basalt-agent/"+Version, "requester")
	if err != nil {
		return &gateLink{}
	}
	l := &gateLink{c: c, mode: "observe"}
	if c.Enforced(gatePath) {
		l.mode = "enforce"
	}
	return l
}

func (l *gateLink) close() {
	if l != nil && l.c != nil {
		l.c.Close()
	}
}

func (l *gateLink) enforced() bool { return l != nil && l.mode == "enforce" }

// observe reports what the terminal decided (shadow mode only).
func (l *gateLink) observe(calls []gateclient.Call, outcome, by string) {
	if l == nil || l.c == nil || l.mode != "observe" {
		return
	}
	_, _ = l.c.Observe(gateclient.Proposal{Calls: calls}, outcome, by)
}

// errDeclined: the request was not approved.
var errDeclined = errors.New("not approved")

// decide asks the gate and returns the request id once it is allowed.
// interactive: approve it here with the terminal decider.
func (l *gateLink) decide(calls []gateclient.Call, interactive bool) (string, error) {
	rep, err := l.c.Propose(gateclient.Proposal{Calls: calls})
	if err != nil {
		return "", fmt.Errorf("approval gate: %w", err)
	}
	switch rep.Decision {
	case gateclient.Allowed:
		fmt.Printf("Approved by %s (request %s).\n", rep.By, rep.ID)
		return rep.ID, nil
	case gateclient.Refused:
		return "", fmt.Errorf("refused by the approval gate: %s", rep.Reason)
	case gateclient.Asked:
	default:
		return "", fmt.Errorf("the approval gate answered %q", rep.Decision)
	}
	if interactive {
		fmt.Printf("Approve request %s (your password):\n", rep.ID)
		cmd := exec.Command(gateDecider, "approve", rep.ID)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			_, _ = l.c.Cancel(rep.ID)
			return "", fmt.Errorf("%w (%v)", errDeclined, err)
		}
	} else {
		fmt.Printf("Waiting for approval: request %s (basalt-gate approve %s, or the desktop shell).\n", rep.ID, rep.ID)
	}
	w, err := l.c.Wait(rep.ID, 300)
	if err != nil {
		return "", fmt.Errorf("approval gate: %w", err)
	}
	if w.Decision != gateclient.Allowed {
		reason := w.Reason
		if w.TimedOut {
			_, _ = l.c.Cancel(rep.ID)
			reason = "nobody decided within 5 minutes"
		}
		return "", fmt.Errorf("%w (%s: %s)", errDeclined, w.Decision, reason)
	}
	return rep.ID, nil
}

// result reports what running the request did.
func (l *gateLink) result(id string, err error) {
	if l == nil || l.c == nil || id == "" {
		return
	}
	exit, detail := 0, "done"
	if err != nil {
		exit, detail = 1, err.Error()
	}
	_, _ = l.c.Result(id, err == nil, exit, detail)
}
