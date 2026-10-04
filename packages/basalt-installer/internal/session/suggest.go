package session

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/plan"
	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/probe"
)

// Kernel command line options of the live installer image:
//
//	basalt.inst.repo=URL          Basalt repository for the install (default: the media)
//	basalt.inst.installed-repo=URL  repository URL for the installed system
//	basalt.inst.hostname=NAME     suggested host name
//	basalt.inst.unlock=METHOD     suggested unlock method
//	basalt.inst.finish=ACTION     suggested end action
//	basalt.inst.plan=PATH|URL     start from this plan file (a path on the live
//	                              system or the media, or an http(s) URL);
//	                              the person still reviews it and types the
//	                              disk name before anything is written
//	basalt.inst.ui=auto|tui|gui   which frontend starts on the screen (read by the units)
//
// Without basalt.inst.plan, a plan file on the installer media at
// basalt/plans/default.yaml (an ISO built with LIVE_PLANS) is the start.
func (s *Session) cmdline() map[string]string {
	path := s.opt.Cmdline
	if path == "" {
		path = "/proc/cmdline"
	}
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, arg := range strings.Fields(string(data)) {
		if k, v, ok := strings.Cut(arg, "="); ok && strings.HasPrefix(k, "basalt.inst.") {
			out[strings.TrimPrefix(k, "basalt.inst.")] = v
		}
	}
	return out
}

// Suggest returns a starting plan for this machine: the first disk that can
// take the installation, and the defaults from the kernel command line.
// The disk is never wiped without the person choosing it again in the
// review (the plan's wipe flag stays false).
func (s *Session) Suggest(ctx context.Context) (plan.Plan, probe.Facts, error) {
	f, err := s.Facts(ctx, true)
	if err != nil {
		return plan.Plan{}, f, err
	}
	disk := ""
	for _, d := range f.Disks {
		if d.Complex == "" && d.InUse == "" && !d.ReadOnly && !d.Removable && d.SizeGiB() >= plan.MinDiskGiB {
			disk = d.Path
			break
		}
	}
	p := plan.Default(disk)
	c := s.cmdline()
	src := c["plan"]
	if media := s.opt.Steps.MediaDir; src == "" && media != "" {
		if def := filepath.Join(media, "basalt", "plans", "default.yaml"); fileExists(def) {
			src = def
		}
	}
	if src != "" {
		// A plan from the network may need a moment: the link comes up
		// while the installer starts.
		loaded, err := loadPlan(ctx, src)
		for i := 0; err != nil && isURL(src) && i < 10 && ctx.Err() == nil; i++ {
			time.Sleep(3 * time.Second)
			loaded, err = loadPlan(ctx, src)
		}
		s.mu.Lock()
		s.planSrc, s.planErr = src, err
		s.mu.Unlock()
		if err == nil {
			return loaded, f, nil
		}
		// The wizard starts from the defaults and says why.
	}
	if v := c["repo"]; v != "" {
		p.Repos.Basalt.URL = v
	}
	if v := c["installed-repo"]; v != "" {
		p.Repos.Basalt.InstalledURL = v
	}
	if v := c["hostname"]; v != "" {
		p.Hostname = v
	}
	if v := c["unlock"]; v != "" {
		p.Encryption.Unlock = v
	}
	if v := c["finish"]; v != "" {
		p.Finish = v
	}
	if !f.TPM2 && p.Encryption.Unlock == "tpm2" {
		p.Encryption.Unlock = "recovery-only"
	}
	return p, f, nil
}

// loadPlan reads a plan from a file or an http(s) URL (at most 1 MiB).
func loadPlan(ctx context.Context, src string) (plan.Plan, error) {
	if isURL(src) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return plan.Plan{}, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return plan.Plan{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return plan.Plan{}, fmt.Errorf("HTTP %s", resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return plan.Plan{}, err
		}
		return plan.Parse(data, filepath.Ext(req.URL.Path))
	}
	return plan.Load(src)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func isURL(s string) bool { return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") }

// PlanSource returns the plan file the suggestion came from ("" for the
// defaults) and the error that kept it from loading, if any.
func (s *Session) PlanSource() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planSrc, s.planErr
}
