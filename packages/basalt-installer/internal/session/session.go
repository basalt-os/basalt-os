// Package session is the installer's state machine shared by every
// frontend: facts, the plan being edited, its exact preview, the running
// installation, the recovery key and its acknowledgement, and the final
// reboot. The TUI uses it in process; the GUI reaches it over the local
// JSON API (package api).
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/engine"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

// State of a session.
type State string

const (
	Idle       State = "idle"
	Previewed  State = "previewed"
	Installing State = "installing"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
)

// Preview is a resolved plan with its exact step list. Token identifies it:
// an installation must name the token of the preview the person saw.
type Preview struct {
	Resolved steps.Resolved `json:"resolved"`
	Steps    []steps.Step   `json:"steps"`
	Text     string         `json:"text"`
	Issues   plan.Issues    `json:"issues"`
	Token    string         `json:"token"`
}

// Options configure a session.
type Options struct {
	Prober      probe.Prober
	Steps       steps.Options
	Engine      engine.Options
	ZoneinfoDir string
	// Finisher reboots or powers off the machine ("reboot", "poweroff").
	Finisher func(ctx context.Context, action string) error
	// Cmdline is the kernel command line file read for basalt.inst.*
	// defaults (default /proc/cmdline).
	Cmdline string
	// KeyWriter writes the recovery key to removable media (default
	// MountAndWrite).
	KeyWriter KeyWriter
}

// Session is safe for concurrent use.
type Session struct {
	opt Options

	mu       sync.Mutex
	facts    probe.Facts
	probed   bool
	preview  *Preview
	state    State
	history  []engine.Event
	subs     map[int]chan engine.Event
	nextSub  int
	recKey   string
	acked    bool
	lastErr  string
	logPath  string
	cancel   context.CancelFunc
	keySaved []string
	auto     AutoStatus
	planSrc  string
	planErr  error
	started  time.Time
	ended    time.Time
}

// New returns a session.
func New(opt Options) *Session {
	return &Session{opt: opt, state: Idle, subs: map[int]chan engine.Event{}}
}

// Facts probes the machine (once, or again with refresh).
func (s *Session) Facts(ctx context.Context, refresh bool) (probe.Facts, error) {
	s.mu.Lock()
	if s.probed && !refresh {
		f := s.facts
		s.mu.Unlock()
		return f, nil
	}
	s.mu.Unlock()
	f, err := s.opt.Prober.Probe(ctx)
	if err != nil {
		return f, err
	}
	s.mu.Lock()
	s.facts, s.probed = f, true
	s.mu.Unlock()
	return f, nil
}

// Validate checks a plan against this machine.
func (s *Session) Validate(ctx context.Context, p plan.Plan) (plan.Issues, error) {
	f, err := s.Facts(ctx, false)
	if err != nil {
		return nil, err
	}
	p.ApplyDefaults()
	return plan.Validate(p, &f, s.opt.ZoneinfoDir), nil
}

// MakePreview validates and resolves a plan and generates its steps. The
// preview replaces any earlier one; only the newest can be installed.
func (s *Session) MakePreview(ctx context.Context, p plan.Plan) (Preview, error) {
	s.mu.Lock()
	if s.state == Installing {
		s.mu.Unlock()
		return Preview{}, errors.New("an installation is running")
	}
	s.mu.Unlock()
	f, err := s.Facts(ctx, true)
	if err != nil {
		return Preview{}, err
	}
	p.ApplyDefaults()
	issues := plan.Validate(p, &f, s.opt.ZoneinfoDir)
	if err := issues.Err(); err != nil {
		return Preview{Issues: issues}, err
	}
	r, err := steps.Resolve(p, f, s.opt.Steps)
	if err != nil {
		return Preview{Issues: issues}, err
	}
	list, err := steps.Generate(r)
	if err != nil {
		return Preview{Issues: issues}, err
	}
	text := steps.Preview(list)
	sum := sha256.Sum256([]byte(text + "\x00" + r.Plan.Redacted().JSON()))
	pv := Preview{Resolved: r, Steps: list, Text: text, Issues: issues, Token: hex.EncodeToString(sum[:8])}
	pv.Resolved.Plan = r.Plan // keep secrets for execution; Public() strips them
	s.mu.Lock()
	s.preview, s.state = &pv, Previewed
	s.mu.Unlock()
	return pv, nil
}

// Public returns the preview without secrets, for the API.
func (pv Preview) Public() Preview {
	out := pv
	out.Resolved.Plan = pv.Resolved.Plan.Redacted()
	return out
}

// ConfirmWord is what the person types to confirm the wipe: the disk's
// kernel name ("vda", "nvme0n1").
func (pv Preview) ConfirmWord() string { return strings.TrimPrefix(pv.Resolved.Disk.Path, "/dev/") }

// Install starts the installation of the preview with this token. confirm
// must be the disk name (ConfirmWord). It returns at once; events follow.
func (s *Session) Install(ctx context.Context, token, confirm string) error {
	s.mu.Lock()
	pv := s.preview
	switch {
	case pv == nil:
		s.mu.Unlock()
		return errors.New("no preview: make one first")
	case s.state == Installing:
		s.mu.Unlock()
		return errors.New("an installation is already running")
	case s.state == Succeeded:
		s.mu.Unlock()
		return errors.New("the system is installed; reboot")
	case token != pv.Token:
		s.mu.Unlock()
		return errors.New("the plan changed since the preview: review the new preview")
	case confirm != pv.ConfirmWord():
		s.mu.Unlock()
		return fmt.Errorf("type %q to confirm that %s is erased", pv.ConfirmWord(), pv.Resolved.Disk.Path)
	}
	s.mu.Unlock()
	// The disk must still be free: probe again right before writing.
	f, err := s.Facts(ctx, true)
	if err != nil {
		return err
	}
	if err := plan.Validate(pv.Resolved.Plan, &f, s.opt.ZoneinfoDir).Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.state, s.history, s.recKey, s.acked, s.lastErr, s.cancel = Installing, nil, "", false, "", cancel
	s.keySaved, s.started, s.ended = nil, time.Now(), time.Time{}
	s.mu.Unlock()

	eopt := s.opt.Engine
	eopt.Header = map[string]any{
		"plan":    pv.Resolved.Plan.Redacted(),
		"machine": map[string]any{"disk": pv.Resolved.Disk, "virt": pv.Resolved.Virt, "secure_boot": pv.Resolved.SecureBoot, "tpm2": pv.Resolved.TPM2},
		"profile": pv.Resolved.Profile + " (" + pv.Resolved.ProfileReason + ")",
		"token":   pv.Token, "installer": pv.Resolved.Installer,
	}
	eng := engine.New(eopt)
	go func() {
		err := eng.Run(runCtx, pv.Steps, func(e engine.Event) {
			// The plan's removable medium gets the key before the frontends
			// hear that the installation is done, so none of them asks for
			// an acknowledgement that the copy already gave.
			if e.Type == engine.EvDone && e.OK {
				s.autoSaveKey(context.Background(), *pv)
			}
			s.dispatch(e)
		})
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ended = time.Now()
		if err != nil {
			// The volume the recovery key opened was rolled back with
			// the rest: the key is worthless and is not kept.
			s.state, s.lastErr, s.recKey = Failed, err.Error(), ""
			if errors.Is(err, engine.ErrBusy) {
				s.broadcast(engine.Event{Type: engine.EvDone, OK: false, Error: err.Error()})
			}
		} else {
			s.state = Succeeded
		}
		s.cancel = nil
	}()
	return nil
}

// Cancel stops a running installation; the engine rolls back.
func (s *Session) Cancel() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != Installing || s.cancel == nil {
		return errors.New("no installation is running")
	}
	s.cancel()
	return nil
}

func (s *Session) dispatch(e engine.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Type == engine.EvSecret && e.Kind == engine.SecretRecKey {
		s.recKey = e.Secret
	}
	if e.Type == engine.EvDone {
		s.logPath = e.LogPath
	}
	s.broadcast(e)
}

// broadcast keeps a history (without secrets) for late subscribers and
// fans the event out. Callers hold s.mu.
func (s *Session) broadcast(e engine.Event) {
	hist := e
	if hist.Type == engine.EvSecret {
		hist.Secret = ""
	}
	if e.Type != engine.EvOutput || len(s.history) < 5000 {
		s.history = append(s.history, hist)
	}
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default: // a slow subscriber loses events, never blocks the install
		}
	}
}

// Subscribe returns a channel of events and a function to stop. The
// history so far is replayed first (secrets left out: use RecoveryKey).
func (s *Session) Subscribe() (<-chan engine.Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan engine.Event, 4096)
	for _, e := range s.history {
		select {
		case ch <- e:
		default:
		}
	}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
	}
}

// Status is a snapshot for frontends.
type Status struct {
	State          State  `json:"state"`
	Error          string `json:"error,omitempty"`
	LogPath        string `json:"log_path,omitempty"`
	RecoveryKey    bool   `json:"recovery_key_pending"`
	RecoveryAcked  bool   `json:"recovery_key_acknowledged"`
	Encrypted      bool   `json:"encrypted"`
	Finish         string `json:"finish,omitempty"`
	PreviewToken   string `json:"preview_token,omitempty"`
	CanFinish      bool   `json:"can_finish"`
	FinishBlockers string `json:"finish_blockers,omitempty"`
	// Disk and Summary describe the plan of the preview.
	Disk    string `json:"disk,omitempty"`
	Summary string `json:"summary,omitempty"`
	// KeySaved lists where copies of the recovery key were written.
	KeySaved []string `json:"recovery_key_saved,omitempty"`
	// Seconds is how long the installation ran (so far).
	Seconds int `json:"seconds,omitempty"`
	// Unattended is the state of an installation started from the boot
	// menu (basalt.inst.plan with basalt.inst.confirm).
	Unattended AutoStatus `json:"unattended"`
}

// Status reports the session state.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{State: s.state, Error: s.lastErr, LogPath: s.logPath, RecoveryKey: s.recKey != "" && !s.acked, RecoveryAcked: s.acked,
		KeySaved: append([]string(nil), s.keySaved...), Unattended: s.auto}
	if s.preview != nil {
		st.Encrypted = s.preview.Resolved.Plan.Encrypted()
		st.Finish = s.preview.Resolved.Plan.Finish
		st.PreviewToken = s.preview.Token
		st.Disk = s.preview.Resolved.Disk.Path
		st.Summary = s.preview.Resolved.Plan.Summary()
	}
	if !s.started.IsZero() {
		end := s.ended
		if end.IsZero() {
			end = time.Now()
		}
		st.Seconds = int(end.Sub(s.started).Seconds())
	}
	st.CanFinish, st.FinishBlockers = s.canFinishLocked()
	return st
}

// RecoveryKey returns the recovery key while it is not acknowledged. After
// the acknowledgement it is gone from memory.
func (s *Session) RecoveryKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acked {
		return ""
	}
	return s.recKey
}

// AckRecoveryKey confirms the person has stored the key: proof is the
// key's first group (the characters before the first dash), retyped.
func (s *Session) AckRecoveryKey(proof string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recKey == "" {
		return errors.New("no recovery key has been generated")
	}
	if s.acked {
		return nil
	}
	first, _, _ := strings.Cut(s.recKey, "-")
	if !strings.EqualFold(strings.TrimSpace(proof), first) {
		return errors.New("that is not the first group of the recovery key")
	}
	s.acked = true
	s.recKey = ""
	s.broadcast(engine.Event{Type: EvKeyAcked})
	return nil
}

func (s *Session) canFinishLocked() (bool, string) {
	if s.state != Succeeded {
		return false, "the installation has not finished"
	}
	if s.preview != nil && s.preview.Resolved.Plan.Encrypted() && !s.acked {
		return false, "confirm that you stored the recovery key"
	}
	return true, ""
}

// Finish reboots or powers off as the plan says (after the recovery key is
// acknowledged). action overrides the plan ("reboot", "poweroff", "none").
func (s *Session) Finish(ctx context.Context, action string) error {
	s.mu.Lock()
	ok, why := s.canFinishLocked()
	if action == "" && s.preview != nil {
		action = s.preview.Resolved.Plan.Finish
	}
	s.mu.Unlock()
	if !ok {
		return errors.New(why)
	}
	if action == "none" || s.opt.Finisher == nil {
		return nil
	}
	return s.opt.Finisher(ctx, action)
}

// CurrentPreview returns the newest preview, if any.
func (s *Session) CurrentPreview() (Preview, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preview == nil {
		return Preview{}, false
	}
	return *s.preview, true
}
