// Package feedback builds and sends the opt-in feedback report of
// `basalt feedback`: the person's own text plus, only for the parts they
// accept, the OS version, the versions of the basalt-* packages, a
// hardware summary and the assistant's recent findings. Personal data is
// scrubbed before anything is shown or sent (host names, user names, IP and
// MAC addresses, paths under /home and /root, anything shaped like a token,
// key or password), the person sees the exact payload, and it is sent only
// after a confirmation.
//
// It runs in the person's own session (the `basalt` command), never in the
// confined daemon, which has no network access. Nothing here reads files a
// person cannot read already; findings come from the system journal.
package feedback

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/notify"
)

// DefaultEndpoint is the Basalt OS feedback service (source:
// github.com/basalt-os/feedback-worker).
const DefaultEndpoint = "https://basalt-feedback.openbasalt.workers.dev/v1/feedback"

// Limits of the service; the report is checked against them before it is
// shown, so a confirmed report is not refused for its size.
const (
	MaxMessage    = 5000 // characters
	MaxEmail      = 254
	MaxSystemJSON = 16 * 1024 // bytes
	MaxPackages   = 60
	MaxFindings   = 10
)

// Kinds of feedback.
var Kinds = []string{"bug", "idea", "other"}

// Part is an optional piece of system information.
type Part string

// The optional parts, in the order they are offered.
const (
	PartOS       Part = "os"
	PartPackages Part = "packages"
	PartHardware Part = "hardware"
	PartFindings Part = "findings"
)

// Parts lists every optional part.
var Parts = []Part{PartOS, PartPackages, PartHardware, PartFindings}

// ParseParts reads a comma-separated list ("os,packages", "all", "none").
func ParseParts(s string) ([]Part, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "none":
		return nil, nil
	case "all":
		return append([]Part(nil), Parts...), nil
	}
	var out []Part
	seen := map[Part]bool{}
	for _, f := range strings.Split(s, ",") {
		p := Part(strings.TrimSpace(f))
		ok := false
		for _, k := range Parts {
			ok = ok || p == k
		}
		if !ok {
			return nil, fmt.Errorf("unknown part %q (os, packages, hardware, findings, all or none)", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// Report is the exact payload sent to the service.
type Report struct {
	Kind    string  `json:"kind"`
	Message string  `json:"message"`
	Email   string  `json:"email,omitempty"`
	System  *System `json:"system,omitempty"`
	Source  string  `json:"source"`
	Client  string  `json:"client"`
	Lang    string  `json:"lang,omitempty"`
}

// System holds the parts the person accepted.
type System struct {
	OS       *OSInfo   `json:"os,omitempty"`
	Packages []string  `json:"packages,omitempty"`
	Hardware *Hardware `json:"hardware,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
}

// OSInfo comes from /etc/os-release and the kernel release.
type OSInfo struct {
	Name          string `json:"name,omitempty"`
	Version       string `json:"version,omitempty"`
	BasaltVersion string `json:"basalt_version,omitempty"`
	BuildID       string `json:"build_id,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
}

// Hardware is a coarse summary: nothing that identifies the machine
// (no serial numbers, no MAC addresses, no disk identifiers).
type Hardware struct {
	Arch           string  `json:"arch,omitempty"`
	CPU            string  `json:"cpu,omitempty"`
	Cores          int     `json:"cores,omitempty"`
	MemoryGiB      float64 `json:"memory_gib,omitempty"`
	Virtualization string  `json:"virtualization,omitempty"`
	Firmware       string  `json:"firmware,omitempty"`
	TPM            bool    `json:"tpm"`
}

// Finding is one recent finding of the assistant's daemon.
type Finding struct {
	Time     string  `json:"time"`
	Kind     string  `json:"kind"`
	Title    string  `json:"title"`
	Subject  string  `json:"subject,omitempty"`
	Severity float64 `json:"severity,omitempty"`
}

// Sources are how the collectors read the system (replaced in tests).
type Sources struct {
	ReadFile func(path string) ([]byte, error)
	Exists   func(path string) bool
	Run      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// RealSources reads the running system.
func RealSources() Sources {
	return Sources{
		ReadFile: os.ReadFile,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			return cmd.Output()
		},
	}
}

// CollectOS reads /etc/os-release and the kernel release.
func CollectOS(src Sources) (*OSInfo, error) {
	b, err := src.ReadFile("/etc/os-release")
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		if u, err := strconv.Unquote(v); err == nil {
			v = u
		} else {
			v = strings.Trim(v, `"'`)
		}
		kv[k] = v
	}
	o := &OSInfo{Name: kv["NAME"], Version: kv["VERSION"], BasaltVersion: kv["BASALT_VERSION"], BuildID: kv["BUILD_ID"]}
	if k, err := src.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		o.Kernel = strings.TrimSpace(string(k))
	}
	return o, nil
}

// CollectPackages lists the installed basalt-* packages with their versions.
func CollectPackages(ctx context.Context, src Sources) ([]string, error) {
	out, err := src.Run(ctx, "rpm", "-qa", "--qf", "%{NAME} %{VERSION}-%{RELEASE}\n", "basalt-*")
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			pkgs = append(pkgs, l)
		}
	}
	sort.Strings(pkgs)
	if len(pkgs) > MaxPackages {
		pkgs = pkgs[:MaxPackages]
	}
	return pkgs, nil
}

// CollectHardware summarizes the CPU, memory, firmware and virtualization.
func CollectHardware(ctx context.Context, src Sources, arch string) *Hardware {
	h := &Hardware{Arch: arch, Firmware: "bios"}
	if b, err := src.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(k) {
			case "processor":
				h.Cores++
			case "model name":
				if h.CPU == "" {
					h.CPU = strings.Join(strings.Fields(v), " ")
				}
			}
		}
	}
	if b, err := src.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "MemTotal:" {
				if kb, err := strconv.ParseFloat(f[1], 64); err == nil {
					h.MemoryGiB = float64(int(kb/1024/1024*10+0.5)) / 10
				}
			}
		}
	}
	// systemd-detect-virt prints "none" and exits 1 on bare metal.
	if out, _ := src.Run(ctx, "systemd-detect-virt"); len(out) > 0 {
		h.Virtualization = strings.TrimSpace(string(out))
	}
	if src.Exists("/sys/firmware/efi") {
		h.Firmware = "uefi"
	}
	h.TPM = src.Exists("/sys/class/tpm/tpm0")
	return h
}

// CollectFindings reads the daemon's recent findings from the system
// journal (readable by root and by members of wheel, adm or
// systemd-journal). A user who may not read it gets no findings: journalctl
// then shows only that user's own journal.
func CollectFindings(ctx context.Context, src Sources, since time.Duration) ([]Finding, error) {
	out, err := src.Run(ctx, "journalctl", "--no-pager", "-q", "-o", "json",
		"_SYSTEMD_UNIT="+notify.DaemonUnit, "BASALT_AUDIT_TYPE=finding",
		"--since", "-"+strconv.Itoa(int(since.Hours()))+"h", "-n", strconv.Itoa(MaxFindings))
	if err != nil && len(out) == 0 {
		return nil, err
	}
	var fs []Finding
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev, ok, err := notify.ParseEntry(line)
		if err != nil || !ok {
			continue
		}
		fs = append(fs, Finding{Time: ev.Time.UTC().Format(time.RFC3339), Kind: ev.Kind, Title: ev.Title,
			Subject: ev.Subject, Severity: ev.Severity})
	}
	if len(fs) > MaxFindings {
		fs = fs[len(fs)-MaxFindings:]
	}
	return fs, nil
}

// --- scrubbing -----------------------------------------------------------

// Scrubber removes personal data. It adds what is personal on this machine
// (its host names, its people's user names) to the general redaction the
// assistant applies before anything goes to a remote model.
type Scrubber struct {
	Hostnames []string
	Users     []string
	names     *regexp.Regexp
}

var (
	reMAC  = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)
	reRoot = regexp.MustCompile(`(/root/)([^/\s"']+)`)
)

// NewScrubber prepares the name list (longest first, whole words only).
func NewScrubber(hostnames, users []string) *Scrubber {
	s := &Scrubber{}
	seen := map[string]bool{}
	var alts []string
	add := func(list *[]string, v string) {
		v = strings.TrimSpace(v)
		// One-letter names and common words would scrub ordinary text.
		if len(v) < 2 || seen[strings.ToLower(v)] || commonWord[strings.ToLower(v)] {
			return
		}
		seen[strings.ToLower(v)] = true
		*list = append(*list, v)
		alts = append(alts, regexp.QuoteMeta(v))
	}
	for _, h := range hostnames {
		add(&s.Hostnames, h)
		if short, _, ok := strings.Cut(h, "."); ok {
			add(&s.Hostnames, short)
		}
	}
	for _, u := range users {
		add(&s.Users, u)
	}
	if len(alts) > 0 {
		sort.Slice(alts, func(i, j int) bool { return len(alts[i]) > len(alts[j]) })
		s.names = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])(` + strings.Join(alts, "|") + `)($|[^A-Za-z0-9_-])`)
	}
	return s
}

// Names that are also system or ordinary words are not scrubbed as names.
var commonWord = map[string]bool{
	"root": true, "localhost": true, "basalt": true, "fedora": true, "admin": true, "user": true,
	"nobody": true, "system": true, "server": true, "test": true, "the": true, "and": true,
}

// Scrub returns s without personal data.
func (s *Scrubber) Scrub(v string) string {
	if s.names != nil {
		// Replace until stable: adjacent names share their separators.
		for i := 0; i < 4; i++ {
			nv := s.names.ReplaceAllString(v, "${1}"+explain.Redacted+"${3}")
			if nv == v {
				break
			}
			v = nv
		}
	}
	v = explain.RedactString(v)
	v = reMAC.ReplaceAllString(v, explain.Redacted)
	v = reRoot.ReplaceAllString(v, "${1}"+explain.Redacted)
	return v
}

// ScrubReport scrubs every free-text value of the report except the reply
// e-mail address the person typed for themselves, and returns the number
// of values that changed.
func (s *Scrubber) ScrubReport(r *Report) int {
	n := 0
	f := func(p *string) {
		if v := s.Scrub(*p); v != *p {
			*p, n = v, n+1
		}
	}
	f(&r.Message)
	if r.System == nil {
		return n
	}
	if o := r.System.OS; o != nil {
		f(&o.Name)
		f(&o.Version)
		f(&o.BasaltVersion)
		f(&o.BuildID)
		f(&o.Kernel)
	}
	for i := range r.System.Packages {
		f(&r.System.Packages[i])
	}
	if h := r.System.Hardware; h != nil {
		f(&h.CPU)
		f(&h.Virtualization)
	}
	for i := range r.System.Findings {
		f(&r.System.Findings[i].Title)
		f(&r.System.Findings[i].Subject)
	}
	return n
}

// LocalNames returns this machine's host names and the user names of its
// people (the current and the sudo user, and accounts with a uid from 1000
// to 59999 in /etc/passwd).
func LocalNames(src Sources) (hosts, users []string) {
	if h, err := os.Hostname(); err == nil {
		hosts = append(hosts, h)
	}
	if b, err := src.ReadFile("/etc/hostname"); err == nil {
		hosts = append(hosts, strings.TrimSpace(string(b)))
	}
	if u, err := user.Current(); err == nil && u.Uid != "0" {
		users = append(users, u.Username)
	}
	if v := os.Getenv("SUDO_USER"); v != "" {
		users = append(users, v)
	}
	if b, err := src.ReadFile("/etc/passwd"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Split(l, ":")
			if len(f) < 5 {
				continue
			}
			uid, err := strconv.Atoi(f[2])
			if err != nil || uid < 1000 || uid >= 60000 {
				continue
			}
			users = append(users, f[0])
			// The full name field (GECOS) names a person too.
			if name, _, _ := strings.Cut(f[4], ","); strings.TrimSpace(name) != "" {
				users = append(users, strings.TrimSpace(name))
			}
		}
	}
	return hosts, users
}

// --- validation, preview and sending -------------------------------------

// Validate checks the report against the service's rules.
func Validate(r *Report) error {
	ok := false
	for _, k := range Kinds {
		ok = ok || r.Kind == k
	}
	if !ok {
		return &Error{Code: "bad_kind"}
	}
	if strings.TrimSpace(r.Message) == "" {
		return &Error{Code: "empty_message"}
	}
	if len([]rune(r.Message)) > MaxMessage {
		return &Error{Code: "message_too_long"}
	}
	if r.Email != "" && (len(r.Email) > MaxEmail || !reEmail.MatchString(r.Email)) {
		return &Error{Code: "bad_email"}
	}
	if r.System != nil {
		b, _ := json.Marshal(r.System)
		if len(b) > MaxSystemJSON {
			return &Error{Code: "system_too_long"}
		}
	}
	return nil
}

var reEmail = regexp.MustCompile(`^[^\s@<>()"',;:]+@[^\s@<>()"',;:]+\.[A-Za-z]{2,}$`)

// Payload is the exact body sent (compact JSON).
func Payload(r *Report) ([]byte, error) { return json.Marshal(r) }

// Code is the confirmation code of a payload: the first 8 hex digits of
// its SHA-256. A non-interactive send must name it, so a confirmation
// given for one preview can never send another payload.
func Code(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:4])
}

// Error is a refusal by the service (or a check before sending), with the
// service's stable code, which the caller translates.
type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("feedback service: %s (HTTP %d)", e.Code, e.Status)
	}
	return "feedback: " + e.Code
}

// CheckEndpoint accepts https, and plain http only to the loopback
// interface (tests, a local service).
func CheckEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("feedback endpoint %q is not a URL", endpoint)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("feedback endpoint %q: only https (or http to localhost)", endpoint)
}

// Send posts the payload and returns the id the service gave it.
func Send(ctx context.Context, client *http.Client, endpoint string, payload []byte) (string, error) {
	if err := CheckEndpoint(endpoint); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var ans struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &ans)
	if resp.StatusCode/100 == 2 && ans.OK {
		return ans.ID, nil
	}
	if ans.Error == "" {
		ans.Error = "http_error"
	}
	return "", &Error{Code: ans.Error, Status: resp.StatusCode}
}

// IsRefusal reports a refusal by the service with the given code.
func IsRefusal(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
