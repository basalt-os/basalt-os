// Package drivers finds the graphics hardware and the driver that fits it
// ("Additional drivers"): today the NVIDIA driver of the opt-in
// basalt-nonfree repository for GPUs of the Turing generation and newer
// (docs/nvidia.md). It reads PCI devices from sysfs, NVIDIA's own list of
// supported GPUs (nvidia-gpus.json, generated from the driver package by
// tools/nvidia-gpus), the state basalt-nvidia keeps, the Secure Boot state
// and the installed packages, and builds the driver.install proposal. It
// never changes the system.
package drivers

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

//go:embed nvidia-gpus.json
var gpuTableJSON []byte

// NvidiaLicense is the NVIDIA Driver License Agreement of the driver
// basalt-nonfree ships, byte for byte (its SHA-256 is
// action.NvidiaLicenseSHA256; a test keeps them together).
//
//go:embed nvidia-driver-license.txt
var NvidiaLicense string

// Docs is where the person reads more.
const Docs = "https://github.com/basalt-os/basalt-os/blob/main/docs/nvidia.md"

// Trademark is the attribution line shown with the driver.
const Trademark = "NVIDIA, GeForce and CUDA are trademarks of NVIDIA Corporation."

// Paths read on the system (variables for tests).
var (
	StateFile    = "/var/lib/basalt-nvidia/state"
	BootFile     = "/run/basalt-nvidia/boot"
	ModuleCA     = "/usr/share/basalt/secureboot/basalt-module-ca.der"
	PCIIDs       = "/usr/share/hwdata/pci.ids"
	RepoFile     = "/etc/yum.repos.d/basalt-nonfree.repo"
	RepoOverride = "/etc/dnf/repos.override.d"
)

// Support of an NVIDIA GPU by the driver basalt-nonfree ships.
const (
	Supported   = "supported"   // Turing and newer: the open kernel modules
	Legacy580   = "legacy-580"  // Maxwell, Pascal, Volta: NVIDIA's 580 branch, not packaged
	Unsupported = "unsupported" // older: no current NVIDIA driver, nouveau only
	Unknown     = "unknown"     // not in this driver's list (newer than the driver, or not a GPU)
)

type gpuTable struct {
	Driver string               `json:"driver"`
	Source string               `json:"source"`
	GPUs   map[string][2]string `json:"gpus"`
}

var table = func() gpuTable {
	var t gpuTable
	if err := json.Unmarshal(gpuTableJSON, &t); err != nil {
		panic("drivers: nvidia-gpus.json: " + err.Error())
	}
	return t
}()

// DriverVersion is the NVIDIA driver version basalt-nonfree ships.
func DriverVersion() string { return table.Driver }

// LicenseSHA256 is the SHA-256 of the embedded Agreement.
func LicenseSHA256() string {
	s := sha256.Sum256([]byte(NvidiaLicense))
	return hex.EncodeToString(s[:])
}

// GPU is one display controller on the PCI bus.
type GPU struct {
	Slot     string `json:"slot"`
	Vendor   string `json:"vendor"` // nvidia, intel, amd, other
	VendorID string `json:"vendor_id"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Class    string `json:"class"`  // vga, 3d, display
	Driver   string `json:"driver"` // the kernel driver bound now ("" for none)
	BootVGA  bool   `json:"boot_vga"`
	// NVIDIA only.
	Support string `json:"support,omitempty"`
	Branch  string `json:"branch,omitempty"` // the legacy branch that supports it
	GeForce bool   `json:"geforce,omitempty"`
}

// Sys is how the package sees the system (tests replace it).
type Sys struct {
	Root      string // "/" normally; sysfs is Root/sys
	R         runner.Reader
	ReadFile  func(string) ([]byte, error)
	Kernel    func() string // uname -r
	Graphical func() bool   // the default target is graphical.target
}

// Real is the running system.
func Real(r runner.Reader) Sys {
	return Sys{Root: "/", R: r, ReadFile: os.ReadFile,
		Kernel: func() string {
			b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
			return strings.TrimSpace(string(b))
		},
		Graphical: func() bool {
			l, err := os.Readlink("/etc/systemd/system/default.target")
			if err != nil {
				l, _ = os.Readlink("/usr/lib/systemd/system/default.target")
			}
			return filepath.Base(l) == "graphical.target"
		}}
}

func (s Sys) read(p string) string {
	b, err := s.ReadFile(filepath.Join(s.Root, p))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Detect lists the display controllers (PCI class 03).
func Detect(s Sys) []GPU {
	devs, _ := filepath.Glob(filepath.Join(s.Root, "sys/bus/pci/devices/*"))
	sort.Strings(devs)
	names := pciNames{s: s}
	out := []GPU{}
	for _, d := range devs {
		rel, _ := filepath.Rel(s.Root, d)
		class := s.read(filepath.Join(rel, "class"))
		if !strings.HasPrefix(class, "0x03") {
			continue
		}
		g := GPU{Slot: filepath.Base(d), VendorID: hex4(s.read(filepath.Join(rel, "vendor"))),
			DeviceID: hex4(s.read(filepath.Join(rel, "device"))), BootVGA: s.read(filepath.Join(rel, "boot_vga")) == "1"}
		switch {
		case strings.HasPrefix(class, "0x0300"):
			g.Class = "vga"
		case strings.HasPrefix(class, "0x0302"):
			g.Class = "3d"
		default:
			g.Class = "display"
		}
		if l, err := os.Readlink(filepath.Join(d, "driver")); err == nil {
			g.Driver = filepath.Base(l)
		}
		switch g.VendorID {
		case "10de":
			g.Vendor = "nvidia"
		case "8086":
			g.Vendor = "intel"
		case "1002":
			g.Vendor = "amd"
		default:
			g.Vendor = "other"
		}
		if g.Vendor == "nvidia" {
			g.Support, g.Branch, g.Name = classify(g.DeviceID)
			up := strings.ToUpper(g.Name)
			g.GeForce = strings.Contains(up, "GEFORCE") || strings.Contains(up, "TITAN")
		}
		if g.Name == "" {
			g.Name = names.lookup(g.VendorID, g.DeviceID)
		}
		out = append(out, g)
	}
	return out
}

func hex4(s string) string { return strings.ToLower(strings.TrimPrefix(s, "0x")) }

// classify places an NVIDIA device id in NVIDIA's list.
func classify(dev string) (support, branch, name string) {
	e, ok := table.GPUs[dev]
	if !ok {
		return Unknown, "", "NVIDIA GPU " + dev
	}
	switch e[1] {
	case "":
		return Supported, "", e[0]
	case "580":
		return Legacy580, "580", e[0]
	default:
		return Unsupported, e[1], e[0]
	}
}

// pciNames reads vendor and device names from hwdata's pci.ids.
type pciNames struct {
	s     Sys
	cache map[string]string
}

func (n *pciNames) lookup(ven, dev string) string {
	if n.cache == nil {
		n.cache = map[string]string{}
		f, err := os.Open(filepath.Join(n.s.Root, PCIIDs))
		if err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			var v, vname string
			for sc.Scan() {
				l := sc.Text()
				switch {
				case l == "" || l[0] == '#' || strings.HasPrefix(l, "C "):
					if strings.HasPrefix(l, "C ") {
						v = ""
					}
				case l[0] != '\t' && len(l) > 6:
					v, vname = l[:4], strings.TrimSpace(l[4:])
					n.cache[v] = vname
				case v != "" && strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, "\t\t") && len(l) > 6:
					n.cache[v+":"+l[1:5]] = vname + " " + strings.TrimSpace(l[5:])
				}
			}
		}
	}
	if s, ok := n.cache[ven+":"+dev]; ok {
		return s
	}
	if s, ok := n.cache[ven]; ok {
		return s + " (" + ven + ":" + dev + ")"
	}
	return "PCI display controller " + ven + ":" + dev
}

// SecureBoot is what matters for signed modules.
type SecureBoot struct {
	EFI        bool `json:"efi"`
	Enabled    bool `json:"enabled"`
	CAEnrolled bool `json:"ca_enrolled"`
	CAPending  bool `json:"ca_pending"`
}

func secureBoot(ctx context.Context, s Sys) SecureBoot {
	var sb SecureBoot
	if _, err := os.Stat(filepath.Join(s.Root, "sys/firmware/efi")); err != nil {
		return sb
	}
	sb.EFI = true
	sb.Enabled = strings.Contains(s.R.Read(ctx, "mokutil", "--sb-state").Out, "SecureBoot enabled")
	if _, err := os.Stat(filepath.Join(s.Root, ModuleCA)); err != nil {
		return sb
	}
	// mokutil --test-key exits 1 for an enrolled key too: read its message.
	sb.CAEnrolled = strings.Contains(s.R.Read(ctx, "mokutil", "--test-key", ModuleCA).Out, "already enrolled")
	if !sb.CAEnrolled {
		subj := s.R.Read(ctx, "openssl", "x509", "-inform", "DER", "-in", ModuleCA, "-noout", "-subject").Out
		if _, cn, ok := strings.Cut(subj, "CN"); ok {
			cn = strings.TrimLeft(cn, " =")
			cn = strings.TrimSpace(strings.SplitN(cn, ",", 2)[0])
			sb.CAPending = cn != "" && strings.Contains(s.R.Read(ctx, "mokutil", "--list-new").Out, cn)
		}
	}
	return sb
}

// State is the NVIDIA driver on this system (basalt-nvidia's state).
type State struct {
	Installed      string `json:"installed"` // nvidia-driver, nvidia-driver-compute or ""
	Version        string `json:"version,omitempty"`
	ReleasePackage bool   `json:"release_package"` // basalt-nonfree-release
	// NonfreeAvailable: basalt-nonfree-release is installed, or the
	// enabled repositories offer it (dnf's cached metadata). Until the
	// basalt-nonfree repository is published nothing offers it, and the
	// driver is "unavailable": no install is recommended or proposed.
	NonfreeAvailable bool   `json:"nonfree_available"`
	RepoEnabled      bool   `json:"repo_enabled"`
	Mode             string `json:"mode"` // none, trial, active, fallback
	Since            string `json:"since,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Snapshot         string `json:"snapshot,omitempty"`
	Proposal         string `json:"proposal,omitempty"`
	HeldKernel       string `json:"held_kernel,omitempty"`
	ThisBoot         string `json:"this_boot,omitempty"` // nvidia or nouveau
	ThisBootReason   string `json:"this_boot_reason,omitempty"`
	Loaded           bool   `json:"loaded"`
}

func keyValues(text string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

func state(ctx context.Context, s Sys) State {
	st := State{Mode: "none"}
	out := s.R.Read(ctx, "rpm", "-q", "--qf", "%{NAME} %{VERSION}\n", "nvidia-driver", "nvidia-driver-compute", "basalt-nonfree-release").Out
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 || strings.Contains(l, "not installed") {
			continue
		}
		switch f[0] {
		case "nvidia-driver", "nvidia-driver-compute":
			if st.Installed != "nvidia-driver" {
				st.Installed, st.Version = f[0], f[1]
			}
		case "basalt-nonfree-release":
			st.ReleasePackage = true
		}
	}
	st.RepoEnabled = repoEnabled(s)
	st.NonfreeAvailable = st.ReleasePackage || releaseOffered(ctx, s)
	kv := keyValues(s.read(StateFile))
	if kv["mode"] != "" {
		st.Mode = kv["mode"]
	} else if st.Installed != "" {
		st.Mode = "active"
	}
	st.Since, st.Reason, st.Snapshot, st.Proposal, st.HeldKernel = kv["since"], kv["reason"], kv["snapshot"], kv["proposal"], kv["held_kernel"]
	b := keyValues(s.read(BootFile))
	st.ThisBoot, st.ThisBootReason = b["driver"], b["reason"]
	_, err := os.Stat(filepath.Join(s.Root, "sys/module/nvidia"))
	st.Loaded = err == nil
	return st
}

// releaseOffered asks dnf, from its cached metadata only (no network, no
// metadata refresh), whether an enabled repository offers
// basalt-nonfree-release. No cache, an error or no match all mean no.
func releaseOffered(ctx context.Context, s Sys) bool {
	res := s.R.Read(ctx, "dnf", "-q", "--cacheonly", "repoquery", "--available", "--queryformat", "%{name}\n", "basalt-nonfree-release")
	if !res.OK() {
		return false
	}
	for _, l := range strings.Split(res.Out, "\n") {
		if strings.TrimSpace(l) == "basalt-nonfree-release" {
			return true
		}
	}
	return false
}

// repoEnabled reads the repository file and dnf's overrides
// (dnf config-manager setopt writes /etc/dnf/repos.override.d/*.repo).
func repoEnabled(s Sys) bool {
	enabled := false
	scan := func(text string) {
		sec := ""
		for _, l := range strings.Split(text, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
				sec = l[1 : len(l)-1]
				continue
			}
			if k, v, ok := strings.Cut(l, "="); ok && sec == "basalt-nonfree" && strings.TrimSpace(k) == "enabled" {
				v = strings.ToLower(strings.TrimSpace(v))
				enabled = v == "1" || v == "true" || v == "yes"
			}
		}
	}
	scan(s.read(RepoFile))
	files, _ := filepath.Glob(filepath.Join(s.Root, RepoOverride, "*.repo"))
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(s.Root, f)
		scan(s.read(rel))
	}
	return enabled
}

// Recommendation is the driver that fits, and what to do.
type Recommendation struct {
	// Action: install (a supported GPU, no driver yet), unavailable (a
	// supported GPU, but the basalt-nonfree repository is not published
	// yet: nothing to install), installed (the NVIDIA driver is in use),
	// fallback (installed but off since a failed start), guided (an
	// NVIDIA GPU of the 580 legacy branch: the assistant guides, nothing
	// is packaged), unsupported, unknown, none (no NVIDIA GPU).
	Action   string   `json:"action"`
	Driver   string   `json:"driver,omitempty"` // nvidia
	Version  string   `json:"version,omitempty"`
	Variant  string   `json:"variant,omitempty"` // display or compute
	GPU      string   `json:"gpu,omitempty"`     // the NVIDIA GPU it is for
	Hybrid   bool     `json:"hybrid"`            // another GPU drives the display
	Primary  string   `json:"primary,omitempty"` // that GPU
	Kernel   string   `json:"kernel,omitempty"`
	Packages []string `json:"packages,omitempty"`
	Changes  []string `json:"changes,omitempty"` // what installing changes, in words
	Blockers []string `json:"blockers,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

// License is the Agreement as shown before an install.
type License struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Text   string `json:"text,omitempty"`
}

// Report is everything `basalt drivers` and the desktop's Additional
// drivers page show.
type Report struct {
	Time           time.Time      `json:"time"`
	GPUs           []GPU          `json:"gpus"`
	Recommendation Recommendation `json:"recommendation"`
	State          State          `json:"state"`
	SecureBoot     SecureBoot     `json:"secure_boot"`
	License        License        `json:"license"`
	Notices        []string       `json:"notices"`
	Docs           string         `json:"docs"`
}

// Build looks at the system.
func Build(ctx context.Context, s Sys, withLicense bool) Report {
	r := Report{Time: time.Now().UTC(), GPUs: Detect(s), State: state(ctx, s), SecureBoot: secureBoot(ctx, s), Docs: Docs,
		License: License{Name: "NVIDIA Driver License Agreement", SHA256: LicenseSHA256()}}
	if withLicense {
		r.License.Text = NvidiaLicense
	}
	r.Recommendation = Recommend(r.GPUs, r.State, r.SecureBoot, s.Kernel(), s.Graphical())
	r.Notices = notices(r)
	return r
}

// Recommend decides what fits.
func Recommend(gpus []GPU, st State, sb SecureBoot, kernel string, graphical bool) Recommendation {
	rec := Recommendation{Action: "none", Kernel: kernel}
	var best, other, boot *GPU
	rank := map[string]int{Supported: 4, Legacy580: 3, Unknown: 2, Unsupported: 1}
	for i := range gpus {
		g := &gpus[i]
		if g.BootVGA {
			boot = g
		}
		if g.Vendor != "nvidia" {
			if other == nil {
				other = g
			}
			continue
		}
		if best == nil || rank[g.Support] > rank[best.Support] {
			best = g
		}
	}
	// Two GPUs, the other one shows the boot screen (or no GPU says): a
	// laptop with an integrated GPU, which keeps the display.
	if best != nil && other != nil && (boot == nil || boot.Vendor != "nvidia") {
		rec.Hybrid, rec.Primary = true, other.Name
		if boot != nil {
			rec.Primary = boot.Name
		}
	}
	if best == nil {
		if st.Installed != "" || st.Mode != "none" {
			rec.Action, rec.Driver = "installed", "nvidia"
			if st.Mode == "fallback" {
				rec.Action = "fallback"
			}
			rec.Notes = append(rec.Notes, "The NVIDIA driver is set up here but no NVIDIA GPU was found.")
		}
		return rec
	}
	rec.GPU = best.Name
	switch best.Support {
	case Supported:
	case Legacy580:
		rec.Action = "guided"
		rec.Notes = append(rec.Notes, best.Name+" needs NVIDIA's 580 legacy driver (Maxwell, Pascal and Volta GPUs). Basalt OS does not package it yet: "+
			"the assistant will guide that install step by step. Until then nouveau drives it. See "+Docs+".")
		return rec
	case Unsupported:
		rec.Action = "unsupported"
		rec.Notes = append(rec.Notes, best.Name+" is supported only by NVIDIA's old "+best.Branch+" driver branch, which no longer gets updates; nouveau drives it.")
		return rec
	default:
		rec.Action = "unknown"
		rec.Notes = append(rec.Notes, "This NVIDIA GPU ("+best.DeviceID+") is not in the list of the NVIDIA driver "+table.Driver+
			": it may be newer than this driver. Nothing is recommended.")
		return rec
	}
	rec.Driver, rec.Version = "nvidia", table.Driver
	if st.Installed == "" && !st.NonfreeAvailable {
		rec.Action = "unavailable"
		rec.Notes = append(rec.Notes, "The NVIDIA driver for "+best.Name+" is not available yet: the basalt-nonfree repository is not published. "+
			"A coming update of Basalt OS installs it from Additional drivers; until then nouveau drives this GPU.")
		return rec
	}
	rec.Variant = "compute"
	if graphical {
		rec.Variant = "display"
	}
	pkg := "nvidia-driver"
	if rec.Variant == "compute" {
		pkg = "nvidia-driver-compute"
	}
	rec.Packages = []string{"basalt-nonfree-release", pkg, "kmod-nvidia-open-" + kernel}
	switch {
	case st.Installed != "" && st.Mode == "fallback":
		rec.Action = "fallback"
	case st.Installed != "":
		rec.Action = "installed"
	default:
		rec.Action = "install"
	}
	rec.Changes = []string{
		"The basalt-nonfree repository is turned on (signed with the OpenBasalt release key, like the other Basalt repositories).",
		"Packages installed: " + pkg + " " + table.Driver + " (NVIDIA's userspace, unmodified) and the NVIDIA open kernel modules, built and signed by Basalt OS for each kernel.",
		"nouveau, the open driver in use now, is turned off from the next start.",
		"A snapshot is taken first. The next start checks the driver; if it fails, the start after it uses nouveau again and you can return to the snapshot.",
		"A newer kernel waits until its signed NVIDIA module is published, so the computer never starts a kernel without it.",
	}
	if rec.Hybrid && rec.Variant == "display" {
		rec.Changes = append(rec.Changes, rec.Primary+" stays the display GPU. Programs run on "+best.Name+
			" when they ask for it (PRIME offload, basalt-nvidia-run PROGRAM); CUDA works on it.")
	}
	if rec.Variant == "compute" {
		rec.Changes = append(rec.Changes, "This system starts without a desktop: only the compute part is installed (CUDA, NVML, OpenCL, nvidia-smi, persistence daemon), no display packages.")
	}
	if sb.Enabled && !sb.CAEnrolled {
		if sb.CAPending {
			rec.Notes = append(rec.Notes, "The Basalt module CA is waiting for its confirmation at the next start (MokManager); the NVIDIA module loads after it.")
		} else {
			rec.Blockers = append(rec.Blockers, "Secure Boot is on and the Basalt kernel module CA is not enrolled, so the kernel would refuse the NVIDIA module. "+
				"Enroll it first (one confirmation at the next start): sudo basalt-secureboot enroll-mok")
		}
	}
	return rec
}

func notices(r Report) []string {
	ns := []string{"The NVIDIA driver is proprietary software under the NVIDIA Driver License Agreement. Packaged by OpenBasalt, not supported by NVIDIA. " + Trademark}
	for _, g := range r.GPUs {
		if g.GeForce {
			ns = append(ns, "GeForce and Titan software is not licensed for datacenter deployment (NVIDIA Driver License Agreement, section 2.8).")
			break
		}
	}
	return ns
}

// Facts of an install proposal.
func installFacts(r Report) *explain.Facts {
	f := explain.New("driver", "install", "nvidia")
	f.Set("gpu", r.Recommendation.GPU).Set("version", r.Recommendation.Version).Set("variant", r.Recommendation.Variant).
		Set("kernel", r.Recommendation.Kernel)
	if r.Recommendation.Hybrid && r.Recommendation.Variant == "display" {
		f.Set("primary", r.Recommendation.Primary)
	}
	return f
}

// ErrNothing: no supported GPU, or the driver is already installed.
type ErrNothing struct{ Why string }

func (e ErrNothing) Error() string { return e.Why }

// InstallProposal builds the driver.install proposal for the recommended
// driver. variant "" takes the recommendation's.
func InstallProposal(r Report, source, variant string) (*proposal.Proposal, error) {
	rec := r.Recommendation
	switch rec.Action {
	case "install":
	case "installed", "fallback":
		return nil, ErrNothing{"the NVIDIA driver is already installed (" + r.State.Installed + " " + r.State.Version + ", state " + r.State.Mode + ")"}
	default:
		why := "no NVIDIA GPU that the basalt-nonfree driver supports"
		if len(rec.Notes) > 0 {
			why = rec.Notes[0]
		}
		return nil, ErrNothing{why}
	}
	if len(rec.Blockers) > 0 {
		return nil, ErrNothing{rec.Blockers[0]}
	}
	if variant == "" {
		variant = rec.Variant
	}
	a := action.Action{Kind: action.DriverInstall, Params: map[string]string{"driver": "nvidia", "variant": variant,
		"kernel": rec.Kernel, "license": LicenseSHA256()}}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r.Recommendation.Variant = variant
	p := &proposal.Proposal{ID: proposal.NewID(), Created: now, Updated: now, LastSeen: now, Seen: 1, Source: source,
		Kind: "driver", Subject: "nvidia", Key: "driver:nvidia:install:" + variant, Status: proposal.Pending,
		Title:   fmt.Sprintf("install the NVIDIA driver %s for %s", rec.Version, rec.GPU),
		Facts:   installFacts(r),
		Actions: []action.Action{a}, Severity: 1,
		// Only a person who asked for it should apply it.
		NeedsReview: source != "cli"}
	for _, g := range r.GPUs {
		p.Evidence = append(p.Evidence, fmt.Sprintf("GPU %s: %s (%s:%s, %s), kernel driver now: %s%s", g.Slot, g.Name, g.VendorID, g.DeviceID,
			g.Class, orNone(g.Driver), map[bool]string{true: ", shows the boot screen", false: ""}[g.BootVGA]))
	}
	p.Evidence = append(p.Evidence, "packages: "+strings.Join(rec.Packages, " "))
	p.Evidence = append(p.Evidence, fmt.Sprintf("license: %s, SHA-256 %s (basalt drivers license nvidia)", r.License.Name, r.License.SHA256))
	p.Evidence = append(p.Evidence, fmt.Sprintf("Secure Boot: %s", sbWords(r.SecureBoot)))
	p.Evidence = append(p.Evidence, rec.Notes...)
	p.Evidence = append(p.Evidence, r.Notices...)
	return p, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func sbWords(sb SecureBoot) string {
	switch {
	case !sb.EFI:
		return "not booted with UEFI"
	case !sb.Enabled:
		return "off"
	case sb.CAEnrolled:
		return "on, Basalt module CA enrolled"
	case sb.CAPending:
		return "on, Basalt module CA waiting for its confirmation at the next start"
	}
	return "on, Basalt module CA NOT enrolled"
}

// SBWords is the Secure Boot state in English words.
func SBWords(sb SecureBoot) string { return sbWords(sb) }
