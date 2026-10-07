// Package engine executes a resolved plan's steps: in order, with progress
// events for the frontends, an append-only hash-chained install log, and a
// rollback of what can be undone (mounts, open encrypted volumes, temporary
// keys) when a step fails or the person cancels.
package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/auditlog"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

// Event is what the engine reports to a frontend. A "secret" event carries
// the recovery key; it is the only place the key ever appears, and it is
// never written to the log.
type Event struct {
	Type     string  `json:"type"`
	Index    int     `json:"index,omitempty"`
	Total    int     `json:"total,omitempty"`
	ID       string  `json:"id,omitempty"`
	Phase    string  `json:"phase,omitempty"`
	Title    string  `json:"title,omitempty"`
	Command  string  `json:"command,omitempty"`
	Text     string  `json:"text,omitempty"`
	Fraction float64 `json:"fraction,omitempty"`
	Kind     string  `json:"kind,omitempty"`
	Secret   string  `json:"secret,omitempty"`
	OK       bool    `json:"ok,omitempty"`
	Error    string  `json:"error,omitempty"`
	LogPath  string  `json:"log_path,omitempty"`
}

// Event types.
const (
	EvStarted    = "started"
	EvStepStart  = "step_start"
	EvOutput     = "output"
	EvStepDone   = "step_done"
	EvWarning    = "warning"
	EvProgress   = "progress"
	EvSecret     = "secret"
	EvRollback   = "rollback"
	EvDone       = "done"
	SecretRecKey = "recovery_key"
)

// Options configure an Engine.
type Options struct {
	Runner Runner
	// LogDir receives install-<time>.jsonl (default /var/log/basalt-installer).
	LogDir string
	// LockPath serializes installations (default /run/basalt-installer.lock),
	// so a TUI on the serial port and a GUI on the screen cannot both write
	// the disk.
	LockPath string
	// Header is recorded at the start of the log (plan, facts, preview).
	Header map[string]any
	// Secrets are extra values scrubbed from every output line (passwords).
	Secrets []string
	// LogNoSync skips the fsync after each log record. For tests and the
	// demo only: an installation keeps every record on disk at once.
	LogNoSync bool
}

// Engine runs one installation.
type Engine struct {
	opt     Options
	log     *auditlog.Log
	emit    func(Event)
	secrets map[string]string
	scrub   []string
	mu      sync.Mutex
}

// New returns an engine.
func New(opt Options) *Engine {
	if opt.LogDir == "" {
		opt.LogDir = "/var/log/basalt-installer"
	}
	if opt.LockPath == "" {
		opt.LockPath = "/run/basalt-installer.lock"
	}
	if opt.Runner == nil {
		opt.Runner = ExecRunner{}
	}
	return &Engine{opt: opt, secrets: map[string]string{}}
}

// ErrBusy means another installation holds the lock.
var ErrBusy = errors.New("another installation is running on this machine (TUI or GUI)")

type undo struct {
	id   string
	argv []string
}

// Run executes the steps. emit receives every event (it must not block for
// long). The returned error is the first failure of a required step, after
// the rollback; ctx cancellation counts as a failure.
func (e *Engine) Run(ctx context.Context, list []steps.Step, emit func(Event)) (err error) {
	if emit == nil {
		emit = func(Event) {}
	}
	e.emit = emit
	e.scrub = append(e.scrub, e.opt.Secrets...)
	for _, s := range list {
		if s.Stdin != "" && s.StdinShown != s.Stdin {
			e.scrub = append(e.scrub, secretParts(s.Stdin)...)
		}
		for _, v := range s.Env {
			if v.Value != v.Shown {
				e.scrub = append(e.scrub, v.Value)
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(e.opt.LockPath), 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(e.opt.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrBusy
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	if err := os.MkdirAll(e.opt.LogDir, 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(e.opt.LogDir, "install-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	if e.log, err = auditlog.Create(logPath); err != nil {
		return err
	}
	e.log.SetSync(!e.opt.LogNoSync)
	defer e.log.Close()
	e.record("start", "", "installation started", e.opt.Header)
	emit(Event{Type: EvStarted, Total: len(list), LogPath: logPath})

	total := 0
	for _, s := range list {
		total += max(s.Weight, 1)
	}
	done := 0
	var undos []undo
	start := time.Now()
	for i, s := range list {
		ev := Event{Type: EvStepStart, Index: i + 1, Total: len(list), ID: s.ID, Phase: s.Phase, Title: s.Title, Command: s.Command()}
		emit(ev)
		e.record("step", s.ID, s.Title, map[string]any{"phase": s.Phase, "command": s.Command()})
		t0 := time.Now()
		base, weight := done, max(s.Weight, 1)
		progress := func(f float64) {
			f = min(max(f, 0), 1)
			emit(Event{Type: EvProgress, Index: i + 1, Total: len(list), Fraction: (float64(base) + f*float64(weight)) / float64(total)})
		}
		stepErr := e.runStep(ctx, s, progress)
		if stepErr != nil {
			msg := e.clean(stepErr.Error())
			if s.Optional && ctx.Err() == nil {
				e.record("warning", s.ID, msg, nil)
				emit(Event{Type: EvWarning, Index: i + 1, ID: s.ID, Title: s.Title, Text: "optional step failed: " + msg})
			} else {
				e.record("failed", s.ID, msg, nil)
				emit(Event{Type: EvStepDone, Index: i + 1, ID: s.ID, Title: s.Title, OK: false, Error: msg})
				e.rollback(undos)
				final := fmt.Errorf("step %d (%s) failed: %s", i+1, s.Title, msg)
				if ctx.Err() != nil {
					final = fmt.Errorf("cancelled during step %d (%s)", i+1, s.Title)
				}
				e.record("end", "", final.Error(), map[string]any{"ok": false, "seconds": int(time.Since(start).Seconds())})
				emit(Event{Type: EvDone, OK: false, Error: final.Error(), LogPath: logPath})
				return final
			}
		}
		if s.Release != "" {
			for j := len(undos) - 1; j >= 0; j-- {
				if undos[j].id == s.Release {
					undos = append(undos[:j], undos[j+1:]...)
					break
				}
			}
		}
		if len(s.Undo) > 0 && stepErr == nil {
			undos = append(undos, undo{id: s.ID, argv: s.Undo})
		}
		done += weight
		e.record("done", s.ID, "", map[string]any{"seconds": round1(time.Since(t0).Seconds())})
		emit(Event{Type: EvStepDone, Index: i + 1, ID: s.ID, Title: s.Title, OK: stepErr == nil})
		emit(Event{Type: EvProgress, Index: i + 1, Total: len(list), Fraction: float64(done) / float64(total)})
	}
	e.record("end", "", "installation finished", map[string]any{"ok": true, "seconds": int(time.Since(start).Seconds())})
	emit(Event{Type: EvDone, OK: true, LogPath: logPath})
	return nil
}

func (e *Engine) runStep(ctx context.Context, s steps.Step, progress func(float64)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.Write != nil {
		return e.writeFile(s.Write)
	}
	parse := progressParser(s.Progress)
	out := func(line string) {
		line = e.clean(line)
		e.emit(Event{Type: EvOutput, ID: s.ID, Text: line})
		e.record("output", s.ID, line, nil)
		if parse != nil {
			if f, ok := parse(line); ok {
				progress(f)
			}
		}
	}
	if s.KeepAttrsUnder != "" {
		restore, n, err := clearFileAttrs(s.KeepAttrsUnder)
		if err != nil {
			return err
		}
		if n > 0 {
			out(fmt.Sprintf("cleared the append-only or immutable attribute of %d file(s) for this step", n))
		}
		defer func() {
			if rerr := restore(); rerr != nil {
				out("could not restore file attributes: " + rerr.Error())
			}
		}()
	}
	captured, err := e.opt.Runner.Run(ctx, s, out)
	for try := 1; err != nil && try <= s.Retries && ctx.Err() == nil; try++ {
		e.record("retry", s.ID, e.clean(err.Error()), map[string]any{"try": try})
		e.emit(Event{Type: EvOutput, ID: s.ID, Text: fmt.Sprintf("retrying in 2s (%d of %d): %s", try, s.Retries, e.clean(err.Error()))})
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		captured, err = e.opt.Runner.Run(ctx, s, out)
	}
	if err != nil {
		return err
	}
	if s.Capture != "" {
		v := strings.TrimSpace(captured)
		if v == "" {
			return fmt.Errorf("%s: no output to capture", s.Argv[0])
		}
		e.mu.Lock()
		e.secrets[s.Capture] = v
		e.scrub = append(e.scrub, v)
		e.mu.Unlock()
		e.record("secret", s.ID, strings.ReplaceAll(s.Capture, "_", " ")+" generated (shown once to the person, never logged)", nil)
		e.emit(Event{Type: EvSecret, ID: s.ID, Kind: s.Capture, Secret: v})
	}
	return nil
}

func (e *Engine) writeFile(w *steps.FileWrite) error {
	if w.Symlink != "" {
		if err := os.Remove(w.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(w.Symlink, w.Path)
	}
	content := []byte(w.Content)
	switch {
	case w.RandomChars > 0:
		s, err := randomChars(w.RandomChars)
		if err != nil {
			return err
		}
		content = []byte(s)
	case w.FromSecret != "":
		e.mu.Lock()
		v, ok := e.secrets[w.FromSecret]
		e.mu.Unlock()
		if !ok {
			return fmt.Errorf("secret %s was not generated", w.FromSecret)
		}
		content = []byte(v + "\n")
	case w.FromLog:
		data, err := os.ReadFile(e.log.Path())
		if err != nil {
			return err
		}
		content = data
	}
	f, err := os.OpenFile(w.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(w.Mode))
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(os.FileMode(w.Mode)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (e *Engine) rollback(undos []undo) {
	if len(undos) == 0 {
		e.record("rollback", "", "nothing to undo", nil)
		e.emit(Event{Type: EvRollback, Text: "nothing to undo"})
		return
	}
	// A fresh context: the rollback also runs after a cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for i := len(undos) - 1; i >= 0; i-- {
		s := steps.Step{ID: "rollback-" + undos[i].id, Title: "Undo " + undos[i].id, Argv: undos[i].argv}
		e.emit(Event{Type: EvRollback, ID: s.ID, Command: s.Command()})
		_, err := e.opt.Runner.Run(ctx, s, func(line string) {
			e.emit(Event{Type: EvOutput, ID: s.ID, Text: e.clean(line)})
		})
		status := "ok"
		if err != nil {
			status = e.clean(err.Error())
		}
		e.record("rollback", undos[i].id, s.Command(), map[string]any{"result": status})
	}
	e.emit(Event{Type: EvRollback, Text: "rollback finished: mounts released, encrypted volume closed, temporary key removed; the disk is partitioned but holds no bootable system"})
}

// Secret returns a captured secret (for frontends that missed the event).
func (e *Engine) Secret(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.secrets[name]
}

func (e *Engine) record(typ, step, text string, fields map[string]any) {
	if e.log != nil {
		_ = e.log.Append(typ, step, text, fields)
	}
}

// clean scrubs secrets from text.
func (e *Engine) clean(s string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range e.scrub {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	return s
}

// secretParts splits "user:secret\n" stdin into the secret part.
func secretParts(stdin string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(stdin), "\n") {
		if _, v, ok := strings.Cut(line, ":"); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func randomChars(n int) (string, error) {
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		if err != nil {
			return "", err
		}
		b[i] = alnum[k.Int64()]
	}
	return string(b), nil
}

func round1(f float64) float64 { return float64(int(f*10)) / 10 }

var dnfCounter = regexp.MustCompile(`^\[\s*(\d+)/(\d+)\]\s*(.*)$`)

// progressParser returns a function that reads a fraction from a line.
func progressParser(name string) func(string) (float64, bool) {
	if name != "dnf" {
		return nil
	}
	// dnf5 prints "[n/N] name" while downloading and "[n/N] Installing name"
	// while installing: downloads count for 35 %, the transaction for 65 %.
	dl, inst := 0.0, 0.0
	return func(line string) (float64, bool) {
		m := dnfCounter.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			return 0, false
		}
		n, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		if total == 0 {
			return 0, false
		}
		f := float64(n) / float64(total)
		rest := m[3]
		switch {
		case strings.HasPrefix(rest, "Installing") || strings.HasPrefix(rest, "Upgrading") ||
			strings.HasPrefix(rest, "Running") || strings.HasPrefix(rest, "Verify") || strings.HasPrefix(rest, "Prepare"):
			inst = max(inst, f)
		default:
			dl = max(dl, f)
		}
		return 0.35*dl + 0.65*inst, true
	}
}
