package selinux

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
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
// Costs: loading the policy takes well under a second, but every sesearch
// call walks all of its type enforcement rules (about 1.5 s on a small VM).
// Policy therefore
//   - answers a batch of queries in one walk (Prefetch, through the
//     basalt-policy-query helper), so an analysis that needs five rule
//     lookups pays for one;
//   - caches every answer for as long as the policy it came from is the
//     loaded one: the key is the kernel's policy load counter (LoadID, from
//     /sys/fs/selinux/status), which changes on every policy load and
//     boolean commit (semanage, setsebool -P, a module install). Without a
//     load identity it falls back to a short time to live;
//   - for the root command line only, keeps answers across runs in a
//     root-only store (Store), keyed on the boot and the load counter.
//
// The cache is never shared with the confined daemon: answers it wrote are
// never trusted by the root command line (a shared cache would let the less
// privileged side steer the proposals of the more privileged one). The
// daemon and the MCP server keep their answers in memory only.
type Policy struct {
	R runner.Reader
	// Attempts is how many times a busy query is tried (default 10, about
	// 14 s of waiting in all).
	Attempts int
	// Backoff is the first wait between attempts; it doubles each time
	// (default 150 ms, capped at 2 s per wait).
	Backoff time.Duration
	// TTL is how long an answer is reused when the load identity is unknown
	// (default 60 s; negative: never).
	TTL time.Duration
	// Sleep waits between attempts (tests replace it).
	Sleep func(ctx context.Context, d time.Duration)
	// Now is the clock for the cache (tests replace it).
	Now func() time.Time
	// Helper is the batch query program (DefaultHelper on a real system);
	// empty: every query runs sesearch or seinfo on its own.
	Helper string
	// LoadID returns the identity of the loaded policy (KernelLoadID on a
	// real system); nil or not ok: answers expire after TTL.
	LoadID func() (string, bool)
	// Store keeps answers across runs (root command line only).
	Store *Store

	mu       sync.Mutex
	cache    map[string]cached
	id       string // the load identity the cache belongs to ("": TTL mode)
	loaded   bool   // Store read for id
	noHelper bool   // the helper cannot run here: query one by one
}

type cached struct {
	out string
	err string // a query that failed for a reason retrying cannot change
	at  time.Time
}

// DefaultHelper is where basalt-assistant installs basalt-policy-query.
const DefaultHelper = "/usr/libexec/basalt/basalt-policy-query"

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

// NewPolicy returns a Policy with the default retry and cache settings and
// no batching, load identity or store (diag.Real sets those for a real
// system).
func NewPolicy(r runner.Reader) *Policy { return &Policy{R: r} }

var reBusy = regexp.MustCompile(`(?i)resource busy|EBUSY|device or resource busy`)

// IsBusy reports output of a query that lost the race for the policy file.
func IsBusy(out string) bool { return reBusy.MatchString(out) }

// KernelLoadID reads the policy load counter from the SELinux status page
// (struct selinux_kernel_status: version, sequence, enforcing, policyload,
// deny_unknown, all u32). The kernel bumps policyload on every policy load
// and boolean commit.
func KernelLoadID() (string, bool) {
	b, err := os.ReadFile("/sys/fs/selinux/status")
	if err != nil || len(b) < 16 {
		return "", false
	}
	if binary.NativeEndian.Uint32(b[0:4]) < 1 {
		return "", false
	}
	return fmt.Sprintf("load-%d", binary.NativeEndian.Uint32(b[12:16])), true
}

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

// syncLocked drops the cache when the loaded policy changed and reads the
// store for a new identity. Called with mu held.
func (p *Policy) syncLocked() {
	id, ok := "", false
	if p.LoadID != nil {
		id, ok = p.LoadID()
	}
	if !ok {
		id = ""
	}
	if id != p.id {
		p.cache, p.id, p.loaded = nil, id, false
	}
	if p.id != "" && p.Store != nil && !p.loaded {
		p.loaded = true
		for k, v := range p.Store.Load(p.id) {
			if p.cache == nil {
				p.cache = map[string]cached{}
			}
			if _, ok := p.cache[k]; !ok {
				p.cache[k] = cached{out: v, at: p.now()}
			}
		}
	}
}

// lookup returns a cached answer. Called with mu held.
func (p *Policy) lookupLocked(key string) (cached, bool) {
	c, ok := p.cache[key]
	if !ok {
		return cached{}, false
	}
	if p.id == "" {
		// No load identity: only a short time to live.
		if ttl := p.ttl(); ttl <= 0 || p.now().Sub(c.at) >= ttl {
			return cached{}, false
		}
	}
	return c, true
}

func (p *Policy) cachingLocked() bool { return p.id != "" || p.ttl() > 0 }

// Query runs one read-only policy query and returns its output. A query
// that exits non-zero (or could not run) is an error; an empty output with
// exit 0 is a real "nothing matches".
func (p *Policy) Query(ctx context.Context, argv ...string) (string, error) {
	key := runner.Join(argv)
	if c, ok := p.cachedAnswer(key); ok {
		return answer(key, c)
	}
	// Through the helper when it runs here (one walk of the policy, a bit
	// faster than sesearch itself), else the command on its own.
	p.Prefetch(ctx, argv)
	if c, ok := p.cachedAnswer(key); ok {
		return answer(key, c)
	}
	wait := p.backoff()
	n := p.attempts()
	var last runner.Result
	for i := 1; i <= n; i++ {
		last = p.R.Read(ctx, argv...)
		if last.Err == nil && last.Code == 0 && !IsBusy(last.Out) {
			p.remember(map[string]cached{key: {out: last.Out}})
			return last.Out, nil
		}
		if !IsBusy(last.Out) || ctx.Err() != nil {
			// Not the race: retrying would give the same answer.
			return "", &PolicyError{Query: key, Attempts: i, Detail: detail(last)}
		}
		if i < n {
			p.backoffSleep(ctx, &wait)
		}
	}
	return "", &PolicyError{Query: key, Attempts: n, Detail: detail(last)}
}

func answer(key string, c cached) (string, error) {
	if c.err != "" {
		return "", &PolicyError{Query: key, Attempts: 1, Detail: c.err}
	}
	return c.out, nil
}

func (p *Policy) cachedAnswer(key string) (cached, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncLocked()
	return p.lookupLocked(key)
}

// backoffSleep waits a jittered step and doubles it: full jitter around
// the current step, so two processes that collided do not collide again in
// lock step.
func (p *Policy) backoffSleep(ctx context.Context, wait *time.Duration) {
	d := *wait/2 + time.Duration(rand.Int64N(int64(*wait)))
	p.sleep(ctx, d)
	*wait = min(*wait*2, 2*time.Second)
}

// remember stores answers (and persists the successful ones when there is
// a store).
func (p *Policy) remember(entries map[string]cached) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cachingLocked() {
		return
	}
	if p.cache == nil {
		p.cache = map[string]cached{}
	}
	now := p.now()
	for k, c := range entries {
		c.at = now
		p.cache[k] = c
	}
	if p.id == "" || p.Store == nil {
		return
	}
	out := map[string]string{}
	for k, c := range p.cache {
		if c.err == "" {
			out[k] = c.out
		}
	}
	// A store that cannot be written only costs speed.
	_ = p.Store.Save(p.id, out)
}

// helperResult is one answer of basalt-policy-query.
type helperResult struct {
	Out         *string `json:"out"`
	Error       string  `json:"error"`
	Unsupported string  `json:"unsupported"`
}

// Prefetch answers several queries in one walk of the policy (the helper)
// and caches the answers, so the Query calls that follow return at once.
// Queries already cached are left out. It reports nothing: whatever it
// could not answer (no helper, a policy that stays busy, a form the helper
// does not support) is answered by Query on its own, with Query's errors.
func (p *Policy) Prefetch(ctx context.Context, queries ...[]string) {
	p.mu.Lock()
	if p.Helper == "" || p.noHelper {
		p.mu.Unlock()
		return
	}
	p.syncLocked()
	var todo [][]string
	seen := map[string]bool{}
	for _, q := range queries {
		k := runner.Join(q)
		if len(q) == 0 || seen[k] {
			continue
		}
		seen[k] = true
		if _, ok := p.lookupLocked(k); !ok {
			todo = append(todo, q)
		}
	}
	p.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	arg, err := json.Marshal(todo)
	if err != nil {
		return
	}
	wait := p.backoff()
	n := p.attempts()
	for i := 1; i <= n; i++ {
		res := p.R.Read(ctx, p.Helper, string(arg))
		if results, ok := parseHelper(res, len(todo)); ok {
			entries := map[string]cached{}
			for j, r := range results {
				switch {
				case r.Unsupported != "":
					// Left to the command itself.
				case r.Error != "":
					entries[runner.Join(todo[j])] = cached{err: r.Error}
				case r.Out != nil:
					entries[runner.Join(todo[j])] = cached{out: *r.Out}
				}
			}
			p.remember(entries)
			return
		}
		if !IsBusy(res.Out) || ctx.Err() != nil {
			// No helper, no python3-setools, a broken installation: query
			// one by one from now on.
			p.mu.Lock()
			p.noHelper = true
			p.mu.Unlock()
			return
		}
		if i < n {
			p.backoffSleep(ctx, &wait)
		}
	}
}

// parseHelper reads the helper's answer: a JSON object on its last line
// with one result per query.
func parseHelper(res runner.Result, n int) ([]helperResult, bool) {
	if res.Err != nil || res.Code != 0 {
		return nil, false
	}
	out := strings.TrimSpace(res.Out)
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	var v struct {
		Results []helperResult `json:"results"`
	}
	if json.Unmarshal([]byte(out), &v) != nil || len(v.Results) != n {
		return nil, false
	}
	return v.Results, true
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

// --- query forms -----------------------------------------------------------------

// AllowQuery is the sesearch argv for allow rules from dom (to tgt when
// set) for class and perm.
func AllowQuery(dom, tgt, class, perm string) []string {
	q := []string{"sesearch", "-A", "-s", dom}
	if tgt != "" {
		q = append(q, "-t", tgt)
	}
	return append(q, "-c", class, "-p", perm)
}

// TransitionQuery is the sesearch argv for process transitions from init_t
// on executing execType.
func TransitionQuery(execType string) []string {
	return []string{"sesearch", "-T", "-s", "init_t", "-t", execType, "-c", "process"}
}

// PortconQuery is the seinfo argv for the labels of one port.
func PortconQuery(port int) []string {
	return []string{"seinfo", fmt.Sprintf("--portcon=%d", port)}
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
	out, err := p.Query(ctx, TransitionQuery(execType)...)
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
