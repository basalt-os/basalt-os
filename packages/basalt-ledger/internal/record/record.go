// Package record defines what basalt-ledger stores: the shared audit
// record schema version 1 (the envelope basalt-agent and basalt-resolver
// write), plus what the ledger adds when it accepts a record: its own
// sequence number and hash chain, the time it received the record, the
// identity of the peer that sent it, a severity, and the producer's own
// chain position when the producer keeps one.
package record

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Version of the record schema.
const Version = 1

// Producer is the ledger's own producer name (seal, continue, refusals).
const Producer = "basalt-ledger"

// Events the ledger writes itself.
const (
	EventSeal        = "ledger.seal"
	EventContinue    = "ledger.continue"
	EventRefused     = "ledger.refused"
	EventRateLimited = "ledger.ratelimited"
	EventStart       = "ledger.start"
	EventRetention   = "ledger.retention"
)

// Severities, in increasing order.
var Severities = []string{"info", "notice", "warning", "critical"}

// SeverityRank returns the position of s in Severities (-1: unknown).
func SeverityRank(s string) int {
	for i, v := range Severities {
		if v == s {
			return i
		}
	}
	return -1
}

// Subject identifies what acted: an agent session (profile, mode, SELinux
// level, project) or an app.
type Subject struct {
	Profile string `json:"profile,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Level   string `json:"level,omitempty"`
	Project string `json:"project,omitempty"`
	App     string `json:"app,omitempty"`
}

// Peer is the identity of the socket client that appended a record, as
// the kernel reports it (SO_PEERCRED, SO_PEERSEC). Records from the
// ledger's own collectors carry Collector instead.
type Peer struct {
	UID       int    `json:"uid"`
	PID       int    `json:"pid"`
	Context   string `json:"context,omitempty"`
	Collector string `json:"collector,omitempty"` // journal, snapper
}

// Src is the producer's own chain position for the record, when it keeps
// a local chain (basalt-agent's session log, the assistant's audit log),
// so the two copies can be compared.
type Src struct {
	Seq  int64  `json:"seq,omitempty"`
	Hash string `json:"hash,omitempty"`
	Prev string `json:"prev,omitempty"`
}

// Record is one stored ledger line.
type Record struct {
	V        int             `json:"v"`
	Seq      int64           `json:"seq"`
	Time     string          `json:"time"`
	Received string          `json:"received"`
	Producer string          `json:"producer"`
	UID      int             `json:"uid"`
	Session  string          `json:"session,omitempty"`
	Event    string          `json:"event"`
	Outcome  string          `json:"outcome"`
	Severity string          `json:"severity"`
	Subject  Subject         `json:"subject"`
	Data     json.RawMessage `json:"data,omitempty"`
	Peer     *Peer           `json:"peer,omitempty"`
	Src      *Src            `json:"src,omitempty"`
	Prev     string          `json:"prev"`
	Hash     string          `json:"hash"`
}

// ZeroHash is the prev of the first record of a chain.
var ZeroHash = strings.Repeat("0", 64)

// HashOf is the SHA-256 (hex) of r serialized with Hash empty.
func HashOf(r Record) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DataMap decodes the data object (empty map when absent).
func (r Record) DataMap() map[string]any {
	m := map[string]any{}
	if len(r.Data) > 0 {
		_ = json.Unmarshal(r.Data, &m)
	}
	return m
}

// Incoming is a record as a producer sends it (schema version 1). Fields
// the ledger owns (received, severity, peer, its chain) are ignored if
// present; a producer's own seq/prev/hash are kept in Src.
type Incoming struct {
	V        int            `json:"v"`
	Seq      int64          `json:"seq,omitempty"`
	Time     string         `json:"time"`
	Producer string         `json:"producer"`
	UID      *int           `json:"uid"`
	Session  string         `json:"session,omitempty"`
	Event    string         `json:"event"`
	Outcome  string         `json:"outcome"`
	Subject  Subject        `json:"subject"`
	Data     map[string]any `json:"data,omitempty"`
	Prev     string         `json:"prev,omitempty"`
	Hash     string         `json:"hash,omitempty"`
}

// Outcomes accepted.
var Outcomes = map[string]bool{"ok": true, "allowed": true, "denied": true, "error": true}

// Severity of an event: computed by the ledger, never taken from the
// producer.
func Severity(event, outcome string) string {
	switch {
	case event == "ledger.chain_error":
		return "critical"
	case strings.HasPrefix(event, "selinux."), event == EventRefused, event == EventRateLimited,
		event == "ledger.dropped", event == "dns.rebinding", event == "auth.failure":
		return "warning"
	case outcome == "denied" || outcome == "error":
		return "warning"
	case event == "driver.fallback", event == "driver.kernel_hold":
		return "warning"
	case strings.HasPrefix(event, "driver."):
		return "notice"
	// A model downloaded from the network as root, after a person's
	// consent: worth seeing in the summary.
	case strings.HasPrefix(event, "model."):
		return "notice"
	case strings.HasPrefix(event, "snapshot.rollback"), strings.HasPrefix(event, "polkit."),
		strings.HasPrefix(event, "escalation."), strings.HasSuffix(event, ".grant"), event == "grant.apply",
		event == "agent.grant.helper", event == "assistant.apply", event == "assistant.confirm",
		event == EventSeal, event == EventStart, event == EventRetention:
		return "notice"
	}
	return "info"
}

// Agent returns the agent (profile) a record belongs to, if any.
func (r Record) Agent() string { return r.Subject.Profile }

// When parses the record's time (producer time, else received).
func (r Record) When() time.Time {
	if t, err := time.Parse(time.RFC3339Nano, r.Time); err == nil {
		return t
	}
	t, _ := time.Parse(time.RFC3339Nano, r.Received)
	return t
}
