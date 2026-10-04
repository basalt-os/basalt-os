package selinux

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/runner"
)

// Policy runs queries against the loaded SELinux policy (sesearch, seinfo).
//
// The kernel lets one process at a time open /sys/fs/selinux/policy: when
// the daemon and the command line diagnose the same event, one of them gets
// EBUSY ("Device or resource busy"). Such a failure must never read as "no
// rule": Policy retries busy queries with a jittered exponential backoff and
// reports any query that still fails as an error, which the analysis turns
// into "no conclusion" instead of a negative answer.
//
// Successful answers are cached for a short time, so one diagnosis loads the
// policy once per distinct query (each load costs about 2 s). The cache is
// per process on purpose: answers written by the confined daemon are never
// trusted by the root command line (a shared cache would let the less
// privileged side steer the proposals of the more privileged one), and a
// short lifetime picks up policy changes (semanage, a module install).
type Policy struct {
	R runner.Reader
	// Attempts is how many times a busy query is tried (default 10, about
	// 14 s of waiting in all: one policy load takes about 2 s).
	Attempts int
	// Backoff is the first wait between attempts; it doubles each time
	// (default 150 ms, capped at 2 s per wait).
	Backoff time.Duration
	// TTL is how long an answer is reused (default 60 s; negative: never).
	TTL time.Duration
	// Sleep waits between attempts (tests replace it).
	Sleep func(ctx context.Context, d time.Duration)
	// Now is the clock for the cache (tests replace it).
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	out string
	at  time.Time
}

// PolicyError is a policy query that could not be answered.
type PolicyError struct {
	Query    string
	Attempts int
	Detail   string
}

func (e *PolicyError) Error() string {
	if e.Attempts > 1 {
		return fmt.Sprintf("policy query `%s` failed after %d attempts: %s", e.Query, e.Attempts, e.Detail)
	}
	return fmt.Sprintf("policy query `%s` failed: %s", e.Query, e.Detail)
}

// NewPolicy returns a Policy with the default retry and cache settings.
func NewPolicy(r runner.Reader) *Policy { return &Policy{R: r} }

var reBusy = regexp.MustCompile(`(?i)resource busy|EBUSY|device or resource busy`)

// IsBusy reports output of a query that lost the race for the policy file.
func IsBusy(out string) bool { return reBusy.MatchString(out) }

func (p *Policy) attempts() int {
	if p.Attempts > 0 {
		return p.Attempts
	}
	return 10
}

func (p *Policy) backoff() time.Duration {
	if p.Backoff > 0 {
		return p.Backoff
	}
	return 150 * time.Millisecond
}

func (p *Policy) ttl() time.Duration {
	if p.TTL != 0 {
		return p.TTL
	}
	return time.Minute
}

func (p *Policy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Policy) sleep(ctx context.Context, d time.Duration) {
	if p.Sleep != nil {
		p.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Query runs one read-only policy query and returns its output. A query
// that exits non-zero (or could not run) is an error; an empty output with
// exit 0 is a real "nothing matches".
func (p *Policy) Query(ctx context.Context, argv ...string) (string, error) {
	key := runner.Join(argv)
	if ttl := p.ttl(); ttl > 0 {
		p.mu.Lock()
		if c, ok := p.cache[key]; ok && p.now().Sub(c.at) < ttl {
			p.mu.Unlock()
			return c.out, nil
		}
		p.mu.Unlock()
	}
	wait := p.backoff()
	n := p.attempts()
	var last runner.Result
	for i := 1; i <= n; i++ {
		last = p.R.Read(ctx, argv...)
		if last.Err == nil && last.Code == 0 && !IsBusy(last.Out) {
			p.store(key, last.Out)
			return last.Out, nil
		}
		if !IsBusy(last.Out) || ctx.Err() != nil {
			// Not the race: retrying would give the same answer.
			return "", &PolicyError{Query: key, Attempts: i, Detail: detail(last)}
		}
		if i < n {
			// Full jitter around the current step, so two processes that
			// collided do not collide again in lock step.
			d := wait/2 + time.Duration(rand.Int64N(int64(wait)))
			p.sleep(ctx, d)
			wait = min(wait*2, 2*time.Second)
		}
	}
	return "", &PolicyError{Query: key, Attempts: n, Detail: detail(last)}
}

func (p *Policy) store(key, out string) {
	if p.ttl() <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]cached{}
	}
	p.cache[key] = cached{out: out, at: p.now()}
}

func detail(r runner.Result) string {
	s := strings.TrimSpace(r.Out)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r.Err != nil {
		if s != "" {
			return r.Err.Error() + ": " + s
		}
		return r.Err.Error()
	}
	if s == "" {
		s = "no output"
	}
	return fmt.Sprintf("exit %d: %s", r.Code, s)
}

// --- domains -----------------------------------------------------------------

var reTypeTrans = regexp.MustCompile(`^type_transition\s+(\S+)\s+(\S+):process\s+(\S+);`)

// Unconfined domains: SELinux does not stop them, so a "Permission denied"
// of a service running in one of them comes from file modes or something
// else, never from a label.
var unconfinedDomains = map[string]bool{
	"unconfined_service_t": true, "unconfined_t": true, "initrc_t": true, "init_t": true, "kernel_t": true,
	"sysadm_t": true, "spc_t": true, "unconfined_dbusd_t": true,
}

// Unconfined reports a domain SELinux does not confine.
func Unconfined(dom string) bool { return unconfinedDomains[dom] }

// ServiceDomain returns the domain systemd (init_t) starts a program of the
// given executable type in, from the loaded policy's type transitions. A
// program without a transition of its own (a shell, an interpreter, a file
// labeled bin_t) runs in whatever init_t transitions to for that type,
// usually unconfined_service_t. It returns "" when the policy has no
// transition for the type, and an error when the policy cannot be queried.
func (p *Policy) ServiceDomain(ctx context.Context, execType string) (string, error) {
	if execType == "" {
		return "", nil
	}
	out, err := p.Query(ctx, "sesearch", "-T", "-s", "init_t", "-t", execType, "-c", "process")
	if err != nil {
		return "", err
	}
	// sesearch also lists rules written for attributes that contain init_t
	// or the executable type; a rule naming both types exactly wins.
	first := ""
	for _, line := range strings.Split(out, "\n") {
		// Named transitions ("... process X_t \"name\";") do not apply to a
		// service start; the regexp only takes plain ones.
		m := reTypeTrans.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if m[1] == "init_t" && m[2] == execType {
			return m[3], nil
		}
		if first == "" {
			first = m[3]
		}
	}
	return first, nil
}
