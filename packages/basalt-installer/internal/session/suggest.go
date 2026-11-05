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
//	basalt.inst.locale=LOCALE     suggested language (pt_BR.UTF-8)
//	basalt.inst.keymap=KEYMAP     suggested keyboard (br)
//	basalt.inst.timezone=ZONE     suggested time zone (America/Sao_Paulo)
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

// Hints are what a frontend knows about the person's session (the live
// desktop's language and keyboard), used when the plan does not say.
type Hints struct {
	Locale     string `json:"locale,omitempty"`
	XKBLayout  string `json:"xkb_layout,omitempty"`
	XKBVariant string `json:"xkb_variant,omitempty"`
}

// SuggestDisk picks the disk to offer first: an empty disk that can take
// the installation; never one that holds another system, an encrypted
// volume or data (Caution), which the person must choose knowingly. ""
// when every disk holds something.
func SuggestDisk(f probe.Facts) string {
	for _, d := range f.Disks {
		if d.Complex == "" && d.InUse == "" && !d.ReadOnly && !d.Removable && d.SizeGiB() >= plan.MinDiskGiB && d.Caution == "" {
			return d.Path
		}
	}
	return ""
}

// Suggest returns a starting plan for this machine (SuggestWith without
// hints).
func (s *Session) Suggest(ctx context.Context) (plan.Plan, probe.Facts, error) {
	return s.SuggestWith(ctx, Hints{})
}

// SuggestWith returns a starting plan for this machine: an empty disk (see
// SuggestDisk), the language, keyboard and time zone of the live session,
// and the defaults from the kernel command line. A disk that holds
// partitions is never wiped without the person choosing it again (the
// plan's wipe flag stays false); an empty disk has nothing to keep, so the
// plan erases it (the typed confirmation is still required).
func (s *Session) SuggestWith(ctx context.Context, h Hints) (plan.Plan, probe.Facts, error) {
	f, err := s.Facts(ctx, true)
	if err != nil {
		return plan.Plan{}, f, err
	}
	disk := SuggestDisk(f)
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
			// A plan without a disk (the live desktop's default plan)
			// takes the suggested one; the person still confirms it.
			if loaded.Target.Disk == "" {
				loaded.Target.Disk = disk
			}
			s.localize(&loaded, f, h, c)
			emptyWipe(&loaded, f)
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
	s.localize(&p, f, h, c)
	emptyWipe(&p, f)
	return p, f, nil
}

// emptyWipe erases a disk without a partition table: there is nothing to
// keep, and "keep the partitions" is not possible there.
func emptyWipe(p *plan.Plan, f probe.Facts) {
	if d, ok := f.Disk(p.Target.Disk); ok && d.Table == "" && len(d.Partitions) == 0 && d.Contents == "empty" {
		p.Target.Wipe = true
	}
}

// localize fills the language, keyboard and time zone the plan leaves at
// the defaults: from the boot line (basalt.inst.locale, keymap, timezone),
// else the frontend's session, else the live system's configuration; a
// known language also suggests its keyboard and time zone.
func (s *Session) localize(p *plan.Plan, f probe.Facts, h Hints, c map[string]string) {
	first := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	if p.Locale == plan.DefaultLocale {
		if l := plan.NormalLocale(first(c["locale"], h.Locale, f.Live.Locale)); l != "" {
			p.Locale = l
		}
	}
	lang, known := plan.LookupLanguage(p.Locale)
	if p.Keymap == plan.DefaultKeymap {
		km := c["keymap"]
		if km == "" {
			for _, x := range [][2]string{{h.XKBLayout, h.XKBVariant}, {f.Live.Layout, f.Live.Variant}} {
				if x[0] == "" || x[0] == "us" && x[1] == "" {
					continue // the live image's own default says nothing
				}
				if kb, ok := plan.KeyboardForXKB(x[0], x[1]); ok {
					km = kb.Keymap
					break
				}
			}
		}
		if km == "" && f.Live.Keymap != "" && f.Live.Keymap != "us" {
			km = f.Live.Keymap
		}
		if km == "" && known {
			km = lang.Keymap
		}
		if km != "" {
			if kb, ok := plan.LookupKeyboard(km); ok {
				km = kb.Keymap
			}
			p.Keymap = km
		}
	}
	if p.Timezone == plan.DefaultTimezone {
		tz := c["timezone"]
		if tz == "" && f.Live.Timezone != "" && f.Live.Timezone != "UTC" && f.Live.Timezone != "Etc/UTC" {
			tz = f.Live.Timezone
		}
		if tz == "" && known {
			tz = lang.Timezone
		}
		if tz != "" {
			p.Timezone = tz
		}
	}
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
