// Package proposal is the gate's request format, version 1 (PROTOCOL.md,
// "The proposal"): who asks (filled by the gate from the kernel, never
// trusted from the client), the calls of registered actions, the preview
// built from their arguments, the class and resources computed by the
// gate from the action registry, and the digest that binds a decision to
// exactly this content.
package proposal

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/canon"
)

// Version of the proposal format.
const Version = 1

// Risk classes, lowest to highest (PROTOCOL.md, "Classes").
const (
	C0 = "C0" // look: reads, nothing changes
	C1 = "C1" // undoable: the person's own things, can be undone
	C2 = "C2" // system change, with a snapshot and a rollback
	C3 = "C3" // leaves the machine: cannot be called back
	C4 = "C4" // critical: security, identity, power, the gate itself
	C5 = "C5" // cannot be undone: destroys data
)

// Classes in increasing order.
var Classes = []string{C0, C1, C2, C3, C4, C5}

// ClassNames are the English names people see (identifiers stay C0..C5).
var ClassNames = map[string]string{C0: "Look", C1: "Undoable", C2: "System change", C3: "Leaves the computer",
	C4: "Critical", C5: "Cannot be undone"}

// ClassRank returns the position of c in Classes (-1: unknown).
func ClassRank(c string) int {
	for i, v := range Classes {
		if v == c {
			return i
		}
	}
	return -1
}

// MaxClass returns the higher of two classes (unknown counts as C5).
func MaxClass(a, b string) string {
	ra, rb := ClassRank(a), ClassRank(b)
	if ra < 0 {
		return C5
	}
	if rb < 0 {
		return C5
	}
	if rb > ra {
		return b
	}
	return a
}

// Requester kinds.
const (
	KindPerson          = "person"
	KindAssistant       = "assistant"
	KindSystemAssistant = "system-assistant"
	KindAgent           = "agent"
	KindApp             = "app"
	KindTool            = "tool"
	KindSchedule        = "schedule"
	KindPhone           = "phone"
)

// Kinds lists every requester kind.
var Kinds = map[string]bool{KindPerson: true, KindAssistant: true, KindSystemAssistant: true, KindAgent: true,
	KindApp: true, KindTool: true, KindSchedule: true, KindPhone: true}

// Taints, least to most: a request that follows untrusted content carries
// the content's taint (ADR 0019: web pages, mail and files are data).
var Taints = []string{"none", "system", "personal", "web"}

// TaintRank returns the position of t in Taints (unknown counts as web).
func TaintRank(t string) int {
	if t == "" {
		return 0
	}
	for i, v := range Taints {
		if v == t {
			return i
		}
	}
	return len(Taints) - 1
}

// Origins of a request.
var Origins = map[string]bool{"request": true, "schedule": true, "event": true}

// Party is one member of a request's chain: the requester or an
// intermediary that relayed it.
type Party struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Session string `json:"session,omitempty"`
}

// String is "kind" or "kind:name".
func (p Party) String() string {
	if p.Name == "" {
		return p.Kind
	}
	return p.Kind + ":" + p.Name
}

// Requester is who asks, as the gate established it.
type Requester struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Session string `json:"session,omitempty"`
	UID     int    `json:"uid"`
	PID     int    `json:"pid,omitempty"`
	Context string `json:"context,omitempty"`
	// Via lists the intermediaries, outermost last (confused deputy:
	// rules match the least trusted party of the whole chain).
	Via    []Party `json:"via,omitempty"`
	Taint  string  `json:"taint"`
	Origin string  `json:"origin"`
	// RelayedBy is the SELinux type of the trusted relay that reported the
	// requester (the shell daemon, the assistant), when there was one.
	RelayedBy string `json:"relayed_by,omitempty"`
}

// Chain returns the requester and every intermediary.
func (r Requester) Chain() []Party {
	out := []Party{{Kind: r.Kind, Name: r.Name, Session: r.Session}}
	return append(out, r.Via...)
}

// Who is a plain English name of the requester for records.
func (r Requester) Who() string {
	var s string
	switch r.Kind {
	case KindAgent:
		s = "An agent"
		if r.Name != "" {
			s = "Agent " + r.Name
		}
		if r.Session != "" {
			s += " (session " + r.Session + ")"
		}
	case KindPerson:
		s = fmt.Sprintf("The person (uid %d)", r.UID)
	case KindAssistant:
		s = "The assistant"
	case KindSystemAssistant:
		s = "The system assistant"
	case KindApp:
		s = "The app " + orQ(r.Name)
	case KindTool:
		s = fmt.Sprintf("The tool %s (uid %d)", orQ(r.Name), r.UID)
	case KindSchedule:
		s = "The schedule of rule " + orQ(r.Name)
	case KindPhone:
		s = "The paired phone " + orQ(r.Name)
	default:
		s = "A program"
	}
	if len(r.Via) > 0 {
		v := make([]string, len(r.Via))
		for i, p := range r.Via {
			v[i] = p.String()
		}
		s += " through " + strings.Join(v, ", ")
	}
	return s
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// Call is one action of a registered closed set, with its arguments.
type Call struct {
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

// Line is one preview line: a catalog key and its arguments.
type Line struct {
	Key  string         `json:"key"`
	Args map[string]any `json:"args,omitempty"`
}

// Preview is what a person sees, built from validated arguments by the
// executor's planner (or by the gate from the registry for the generic
// actions) and rendered per locale at display time.
type Preview struct {
	TitleKey  string         `json:"title_key"`
	TitleArgs map[string]any `json:"title_args,omitempty"`
	Lines     []Line         `json:"lines,omitempty"`
	Diff      string         `json:"diff,omitempty"`
	// Commands are the exact command lines of system actions (the system
	// assistant's proposals), shown with the short code.
	Commands []string `json:"commands,omitempty"`
}

// Resource is something a call touches, extracted by the registry.
type Resource struct {
	Kind    string `json:"kind"` // path, unit, package, host, recipient, boolean, user, disk, rule
	Value   string `json:"value"`
	Beneath bool   `json:"beneath,omitempty"` // a path and everything below it
	Bytes   int64  `json:"bytes,omitempty"`
}

// String is "kind:value".
func (r Resource) String() string { return r.Kind + ":" + r.Value }

// Reversible says how a change can be undone.
type Reversible struct {
	How   string `json:"how"` // trash, snapshot, undo-log, none
	Until string `json:"until,omitempty"`
}

// Proposal is one request, as the gate keeps it.
type Proposal struct {
	V          int         `json:"v"`
	ID         string      `json:"id"`
	Group      string      `json:"group,omitempty"`
	Ref        string      `json:"ref,omitempty"` // the system assistant's proposal id (short code compatibility)
	Requester  Requester   `json:"requester"`
	Calls      []Call      `json:"calls"`
	Preview    Preview     `json:"preview"`
	Class      string      `json:"class"`
	Leaves     bool        `json:"leaves,omitempty"`
	Resources  []Resource  `json:"resources"`
	Reversible *Reversible `json:"reversible,omitempty"`
	Snapshot   string      `json:"snapshot,omitempty"`
	Created    time.Time   `json:"created"`
	Expires    time.Time   `json:"expires"`
	Deferrable bool        `json:"deferrable,omitempty"`
	Digest     string      `json:"digest"`
	Code       string      `json:"code"`
}

// Actions returns the action ids of the calls.
func (p *Proposal) Actions() []string {
	out := make([]string, len(p.Calls))
	for i, c := range p.Calls {
		out[i] = c.Action
	}
	return out
}

// digestInput is what the digest covers.
type digestInput struct {
	Calls     []Call     `json:"calls"`
	Preview   Preview    `json:"preview"`
	Resources []Resource `json:"resources"`
	Ref       string     `json:"ref,omitempty"`
}

// DigestOf returns "sha256:<hex>" over the canonical JSON of the calls,
// the preview, the resources and, when set, the reference: what a person
// approves and an executor runs, after the normalization of
// normalizeDigest (empty preview and resource members removed), so a
// client that writes "diff": null and one that leaves it out agree.
func DigestOf(calls []Call, pv Preview, res []Resource, ref string) (string, error) {
	if res == nil {
		res = []Resource{}
	}
	for i := range calls {
		if calls[i].Args == nil {
			calls[i].Args = map[string]any{}
		}
	}
	raw, err := json.Marshal(digestInput{Calls: calls, Preview: pv, Resources: res, Ref: ref})
	if err != nil {
		return "", err
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	v = normalizeDigest(v)
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	c, err := canon.Canonicalize(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// normalizeDigest applies the digest's normalization (PROTOCOL.md,
// "Digest"): in the preview, in each preview line and in each resource,
// members whose value is null, false, 0, "", [] or {} are removed; ref is
// removed when empty; nothing is removed inside call arguments, title
// arguments or line arguments.
func normalizeDigest(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if pv, ok := m["preview"].(map[string]any); ok {
		dropEmpty(pv)
		if l, ok := pv["lines"].([]any); ok {
			for _, e := range l {
				if lm, ok := e.(map[string]any); ok {
					dropEmpty(lm)
				}
			}
		}
	}
	if l, ok := m["resources"].([]any); ok {
		for _, e := range l {
			if rm, ok := e.(map[string]any); ok {
				dropEmpty(rm)
			}
		}
	}
	if r, ok := m["ref"]; ok && (r == nil || r == "") {
		delete(m, "ref")
	}
	return m
}

func dropEmpty(m map[string]any) {
	for k, v := range m {
		empty := false
		switch x := v.(type) {
		case nil:
			empty = true
		case bool:
			empty = !x
		case float64:
			empty = x == 0
		case json.Number:
			empty = x == "0"
		case string:
			empty = x == ""
		case []any:
			empty = len(x) == 0
		case map[string]any:
			empty = len(x) == 0
		}
		if empty {
			delete(m, k)
		}
	}
}

// Seal computes the digest and the short code.
func (p *Proposal) Seal() error {
	d, err := DigestOf(p.Calls, p.Preview, p.Resources, p.Ref)
	if err != nil {
		return err
	}
	p.Digest = d
	p.Code = ShortCode(d, p.Ref, p.Preview.Commands)
	return nil
}

// ShortCode is the 8 hex digits people read and type. For a system
// assistant proposal (a reference and command lines) it is the
// assistant's fingerprint, the first 8 hex digits of the SHA-256 of the
// proposal id and its command lines, so `basalt show` and the queue show
// the same code; otherwise the first 8 hex digits of the digest.
func ShortCode(digest, ref string, commands []string) string {
	if ref != "" && len(commands) > 0 {
		sum := sha256.Sum256([]byte(ref + "\n" + strings.Join(commands, "\n")))
		return hex.EncodeToString(sum[:])[:8]
	}
	h := strings.TrimPrefix(digest, "sha256:")
	if len(h) < 8 {
		return ""
	}
	return h[:8]
}

var (
	idRe    = regexp.MustCompile(`^g-[0-9a-f]{12}$`)
	groupRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	refRe   = regexp.MustCompile(`^[a-z0-9-]{4,40}$`)
	// ActionRe is the form of action ids: area.verb, lower case.
	ActionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
)

// ValidID reports whether s is a request id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidGroup reports whether s is a group name a requester may use.
func ValidGroup(s string) bool { return groupRe.MatchString(s) }

// ValidRef reports whether s is a system assistant proposal id.
func ValidRef(s string) bool { return refRe.MatchString(s) }

// NewID returns a random request id, e.g. "g-7f3a9c21d04b".
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "g-" + hex.EncodeToString(b)
}
