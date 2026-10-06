// SPDX-License-Identifier: MIT OR Apache-2.0

// Package gate is the reference client of the Basalt approval gate
// protocol (gate/1, PROTOCOL.md): newline-delimited JSON over a Unix
// socket, one request per line, one reply per line. It uses the Go
// standard library only and imports nothing else from Basalt, so other
// programs can copy it (basalt-shell keeps a copy under
// internal/gateclient, checked against the protocol fixtures).
//
// The package is dual-licensed MIT OR Apache-2.0, like PROTOCOL.md.
package gateclient

import "encoding/json"

// Protocol is the protocol name and major version this package speaks.
const Protocol = "gate/1"

// DefaultSocket is where the gate listens; $BASALT_GATE_SOCKET overrides.
const DefaultSocket = "/run/basalt-gate/gate.sock"

// Decisions a request can have.
const (
	Allowed   = "allowed"   // run it (by a rule, or a person approved)
	Asked     = "asked"     // waits for a person
	Refused   = "refused"   // never (a rule, a hard limit, the registry)
	Declined  = "declined"  // a person said no
	Expired   = "expired"   // nobody decided in time
	Cancelled = "cancelled" // the requester withdrew it
)

// Call is one action with its arguments.
type Call struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

// Line is one preview line: a catalog key and its arguments.
type Line struct {
	Key  string         `json:"key"`
	Args map[string]any `json:"args,omitempty"`
}

// Preview is what a person sees.
type Preview struct {
	TitleKey  string         `json:"title_key"`
	TitleArgs map[string]any `json:"title_args,omitempty"`
	Lines     []Line         `json:"lines,omitempty"`
	Diff      string         `json:"diff,omitempty"`
	Commands  []string       `json:"commands,omitempty"`
}

// Resource is something a call touches (computed by the gate).
type Resource struct {
	Kind    string `json:"kind"`
	Value   string `json:"value"`
	Beneath bool   `json:"beneath,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
}

// Party is a requester or an intermediary.
type Party struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Session string `json:"session,omitempty"`
}

// OnBehalf is what a trusted relay (the shell daemon, the system
// assistant) reports about the requester it relays for. The gate accepts
// it only from the SELinux types configured as relays, and only for the
// kinds each may report.
type OnBehalf struct {
	Kind    string  `json:"kind"`
	Name    string  `json:"name,omitempty"`
	Session string  `json:"session,omitempty"`
	Via     []Party `json:"via,omitempty"`
}

// Request is one line a client sends.
type Request struct {
	Op string `json:"op"`

	// hello
	Role     string `json:"role,omitempty"` // requester, decider, executor
	Client   string `json:"client,omitempty"`
	Protocol string `json:"protocol,omitempty"`

	// propose, check
	Calls      []Call    `json:"calls,omitempty"`
	Preview    *Preview  `json:"preview,omitempty"`
	Group      string    `json:"group,omitempty"`
	Ref        string    `json:"ref,omitempty"`
	Taint      string    `json:"taint,omitempty"`
	Origin     string    `json:"origin,omitempty"`
	Deferrable bool      `json:"deferrable,omitempty"`
	ClassHint  string    `json:"class_hint,omitempty"`
	OnBehalf   *OnBehalf `json:"on_behalf,omitempty"`
	Via        []Party   `json:"via,omitempty"`

	// wait, status, cancel, claim, result, decide, history
	ID        string   `json:"id,omitempty"`
	IDs       []string `json:"ids,omitempty"`
	Timeout   int      `json:"timeout,omitempty"` // seconds (wait)
	Digest    string   `json:"digest,omitempty"`
	Executor  string   `json:"executor,omitempty"`
	OK        *bool    `json:"ok,omitempty"`
	Exit      *int     `json:"exit,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Snapshots []string `json:"snapshots,omitempty"`
	Approve   *bool    `json:"approve,omitempty"`
	Remember  bool     `json:"remember,omitempty"`
	Limit     int      `json:"limit,omitempty"`

	// confirm (root at a terminal, the system assistant's proposals): the
	// short code the person typed or was shown, and how they confirmed
	// ("code": --yes --confirm CODE; "terminal": typed yes).
	Code string `json:"code,omitempty"`
	Mode string `json:"mode,omitempty"`

	// observe (shadow mode): what the component's own confirmation
	// decided (approved, declined, expired, refused, failed, cancelled)
	// and who decided it there.
	Outcome   string `json:"outcome,omitempty"`
	DecidedBy string `json:"decided_by,omitempty"`

	// rules.*, stop, resume
	Rule    json.RawMessage `json:"rule,omitempty"`
	Rules   json.RawMessage `json:"rules,omitempty"`
	Scope   string          `json:"scope,omitempty"`
	Since   string          `json:"since,omitempty"`
	Records json.RawMessage `json:"records,omitempty"`
	Reason  string          `json:"reason,omitempty"`
}

// Reply is the answer line.
type Reply struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	// hello: what the gate classified the client as.
	Roles []string `json:"roles,omitempty"`
	Kind  string   `json:"kind,omitempty"`
	// hello: the migration paths where Basalt's own components let the
	// gate decide (apply, shell, skills, models, consent, agent); on the
	// others they keep their own confirmation and only observe.
	Enforce []string `json:"enforce,omitempty"`

	// propose, wait, status, check, decide, cancel
	ID       string `json:"id,omitempty"`
	Decision string `json:"decision,omitempty"`
	Class    string `json:"class,omitempty"`
	Reason   string `json:"reason,omitempty"`
	By       string `json:"by,omitempty"`
	Digest   string `json:"digest,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`

	Request    *View      `json:"request,omitempty"`
	Requests   []View     `json:"requests,omitempty"`
	Rules      []RuleView `json:"rules,omitempty"`
	Simulation *Sim       `json:"simulation,omitempty"`
	Status     *Status    `json:"status,omitempty"`
	Event      *Event     `json:"event,omitempty"`
	// Decided lists the outcome per id of a batch decide.
	Decided map[string]string `json:"decided,omitempty"`
}

// View is a request as a client may see it.
type View struct {
	ID         string     `json:"id"`
	Group      string     `json:"group,omitempty"`
	Ref        string     `json:"ref,omitempty"` // the system assistant's proposal id
	Decision   string     `json:"decision"`
	Class      string     `json:"class"`
	ClassName  string     `json:"class_name"`
	Actions    []string   `json:"actions"`
	Who        string     `json:"who"`
	Requester  Party      `json:"requester"`
	Via        []Party    `json:"via,omitempty"`
	UID        int        `json:"uid"`
	Taint      string     `json:"taint"`
	Origin     string     `json:"origin"`
	Calls      []Call     `json:"calls,omitempty"`
	Preview    Preview    `json:"preview"`
	Resources  []Resource `json:"resources"`
	Leaves     bool       `json:"leaves,omitempty"`
	Reversible string     `json:"reversible,omitempty"`
	Created    string     `json:"created"`
	Expires    string     `json:"expires"`
	Deferrable bool       `json:"deferrable,omitempty"`
	By         string     `json:"by,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Remember   string     `json:"remember,omitempty"`    // "Approve and remember" is offered for this long
	NeedsAdmin bool       `json:"needs_admin,omitempty"` // approving asks for administrator authentication
	Digest     string     `json:"digest,omitempty"`
	// Code is shown to deciders only, never returned to a requester.
	Code     string `json:"code,omitempty"`
	Executor string `json:"executor,omitempty"`
	Claimed  bool   `json:"claimed,omitempty"`
	Result   string `json:"result,omitempty"`
}

// RuleView is a rule with its sentence.
type RuleView struct {
	Rule      json.RawMessage `json:"rule"`
	Sentence  string          `json:"sentence"`
	Ref       string          `json:"ref"`
	Scope     string          `json:"scope"`
	UID       int             `json:"uid,omitempty"`
	Paused    string          `json:"paused,omitempty"`
	ExpiresAt string          `json:"expires_at,omitempty"`
	Preset    string          `json:"preset,omitempty"`
	Error     string          `json:"error,omitempty"` // rules.draft: why it cannot be saved
}

// Sim is the result of a dry run.
type Sim struct {
	Since   string    `json:"since"`
	Total   int       `json:"total"`
	Allowed int       `json:"allowed"`
	Asked   int       `json:"asked"`
	Refused int       `json:"refused"`
	Changed int       `json:"changed"` // decisions that differ from what happened
	Items   []SimItem `json:"items"`
	Note    string    `json:"note,omitempty"`
}

// SimItem is one replayed request.
type SimItem struct {
	Time    string   `json:"time"`
	ID      string   `json:"id"`
	Actions []string `json:"actions"`
	Who     string   `json:"who"`
	Would   string   `json:"would"`
	By      string   `json:"by"`
	Was     string   `json:"was,omitempty"`
}

// Status is the gate's state.
type Status struct {
	Protocol         string `json:"protocol"`
	Version          string `json:"version"`
	Stopped          bool   `json:"stopped"`
	StoppedBy        string `json:"stopped_by,omitempty"`
	StoppedAt        string `json:"stopped_at,omitempty"`
	Preset           string `json:"preset"`
	Rules            int    `json:"rules"`
	Pending          int    `json:"pending"`
	SealOK           bool   `json:"seal_ok"`
	AllowCodeConfirm bool   `json:"allow_code_confirm"`
	Actions          int    `json:"actions"`
}

// Event is one line of a subscription.
type Event struct {
	Type string `json:"type"` // request, decision, stop, resume, rules
	ID   string `json:"id,omitempty"`
	Time string `json:"time"`
	Note string `json:"note,omitempty"`
}
