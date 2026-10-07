package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
)

// An unattended installation from the boot menu needs two kernel arguments:
// basalt.inst.plan=PATH|URL (what to install; without it, the plan on the
// installer media) and basalt.inst.confirm=DISK (the kernel name of the
// disk the plan erases, as typed in the wizard).
// The confirmation must name the plan's target disk; with the plan alone,
// the plan only fills in the wizard. The engine service (basalt-installer
// serve) runs it, so the text installers on the screen and on the serial
// console only follow it; the recovery key is still shown and acknowledged
// on one of them, unless the plan writes it to removable media
// (encryption.recovery_key_media). Then the plan's end action runs.

// Unattended states.
const (
	AutoWaiting = "waiting" // loading the plan, preparing the preview
	AutoRunning = "running" // installing, or waiting for the recovery key
	AutoRefused = "refused" // not started: the reason is in Error
	AutoFailed  = "failed"  // the installation failed
	AutoDone    = "done"    // installed, the end action ran
)

// AutoStatus is the state of an unattended installation.
type AutoStatus struct {
	Requested bool   `json:"requested"`
	State     string `json:"state,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Confirm   string `json:"confirm,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Cmdline returns the basalt.inst.* kernel arguments (without the prefix).
func (s *Session) Cmdline() map[string]string { return s.cmdline() }

// UnattendedRequested reports whether the boot line asks for an unattended
// installation (basalt.inst.confirm is set).
func (s *Session) UnattendedRequested() bool { return s.cmdline()["confirm"] != "" }

func (s *Session) setAuto(state, errText string) {
	s.mu.Lock()
	s.auto.State, s.auto.Error = state, errText
	s.notifyLocked()
	s.mu.Unlock()
}

// RunUnattended runs the installation the boot line names, if it names
// one, and returns when it has ended (or was refused). It never installs
// unless basalt.inst.confirm names the plan's target disk.
func (s *Session) RunUnattended(ctx context.Context, planWait time.Duration) {
	c := s.cmdline()
	confirm := strings.TrimPrefix(c["confirm"], "/dev/")
	if confirm == "" {
		return
	}
	src := c["plan"]
	if media := s.opt.Steps.MediaDir; src == "" && media != "" {
		// The plan on the installer media (an ISO built with LIVE_PLANS).
		if def := filepath.Join(media, "basalt", "plans", "default.yaml"); fileExists(def) {
			src = def
		}
	}
	s.mu.Lock()
	s.auto = AutoStatus{Requested: true, State: AutoWaiting, Plan: src, Confirm: confirm}
	s.notifyLocked()
	s.mu.Unlock()
	if src == "" {
		s.setAuto(AutoRefused, fmt.Sprintf(i18n.T("basalt.inst.confirm=%s names a disk but there is no plan: add basalt.inst.plan=PATH or URL to the boot line"), confirm))
		return
	}
	// The plan may come over the network, which can take a moment to come up.
	var p plan.Plan
	var err error
	deadline := time.Now().Add(planWait)
	for {
		p, err = loadPlan(ctx, src)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	if err != nil {
		s.setAuto(AutoRefused, fmt.Sprintf(i18n.T("the plan %s could not be read: %v"), src, err))
		return
	}
	if strings.TrimPrefix(p.Target.Disk, "/dev/") != confirm {
		s.setAuto(AutoRefused, fmt.Sprintf(i18n.T("basalt.inst.confirm=%s does not name the disk the plan erases (%s): nothing was written"), confirm, p.Target.Disk))
		return
	}
	pv, err := s.MakePreview(ctx, p)
	if err != nil {
		s.setAuto(AutoRefused, oneLine(err.Error()))
		return
	}
	if err := s.Install(ctx, pv.Token, confirm); err != nil {
		s.setAuto(AutoRefused, oneLine(err.Error()))
		return
	}
	s.setAuto(AutoRunning, "")
	for {
		// Wait for the end of the installation, then for the recovery key
		// acknowledgement: woken up by each state change, no polling.
		changed := s.changes()
		st := s.Status()
		switch {
		case st.State == Failed:
			s.setAuto(AutoFailed, st.Error)
			return
		case st.State == Succeeded && st.CanFinish:
			if err := s.Finish(ctx, ""); err != nil {
				s.setAuto(AutoFailed, err.Error())
				return
			}
			s.setAuto(AutoDone, "")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
