// Package runner previews, runs and reads commands. It follows the
// tui-tools trust boundary: a change is a Command (an argv plus a human
// description), the preview shows exactly that argv, and the same value is
// what runs. No shell string is ever assembled.
//
// Adapted from github.com/tui-tools/tui-kit/runner (MIT License, see
// third_party/tui-kit.LICENSE): Command, Quote, Join, children without a
// controlling terminal, and the rule that a confirmed change has no
// wall-clock timeout (SIGTERM first, SIGKILL after a grace period).
package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ReadTimeout bounds one read-only query.
const ReadTimeout = 30 * time.Second

// MutationGrace is how long a cancelled change gets after SIGTERM.
const MutationGrace = 10 * time.Second

// Command is one invocation shown to a person before it runs.
type Command struct {
	Argv        []string `json:"argv"`
	Description string   `json:"description,omitempty"`
	// AfterRecord: run only once the apply's result is recorded (a
	// restart that ends the apply itself). Shown like any other command.
	AfterRecord bool `json:"-"`
}

// String renders the argv the way a POSIX shell reads it back.
func (c Command) String() string { return Join(c.Argv) }

// Result is the outcome of one process. Err is set only when the process
// could not run or was stopped (missing binary, timeout); a non-zero exit is
// reported in Code with Err nil, because diagnosers read the output of
// commands that exit non-zero on purpose (systemctl is-active, nginx -t).
type Result struct {
	Out  string
	Code int
	Err  error
}

// OK reports a clean zero exit.
func (r Result) OK() bool { return r.Err == nil && r.Code == 0 }

// ErrNotFound reports a missing binary.
var ErrNotFound = errors.New("command not found")

// Reader runs read-only queries. Diagnosers depend on it so tests can feed
// fixtures instead of a live system.
type Reader interface {
	Read(ctx context.Context, argv ...string) Result
}

// Exec is the real Reader and the executor for confirmed changes.
type Exec struct {
	// Timeout bounds each read; zero uses ReadTimeout.
	Timeout time.Duration
}

// Read runs a query with a timeout and LANG=C.
func (e Exec) Read(ctx context.Context, argv ...string) Result {
	t := e.Timeout
	if t <= 0 {
		t = ReadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	return run(ctx, argv, "", false)
}

// ReadInput is Read with data on standard input (audit2why reads AVCs there).
func (e Exec) ReadInput(ctx context.Context, input string, argv ...string) Result {
	t := e.Timeout
	if t <= 0 {
		t = ReadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	return run(ctx, argv, input, false)
}

// Run executes a confirmed change: no timeout, a gentle stop on cancel.
func (e Exec) Run(ctx context.Context, cmd Command) Result {
	return run(ctx, cmd.Argv, "", true)
}

func run(ctx context.Context, argv []string, input string, mutation bool) Result {
	if len(argv) == 0 {
		return Result{Err: errors.New("empty command")}
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			if st, e := os.Stat(dir + "/" + argv[0]); e == nil && !st.IsDir() {
				bin, err = dir+"/"+argv[0], nil
				break
			}
		}
	}
	if err != nil {
		return Result{Code: 127, Err: ErrNotFound}
	}
	c := exec.CommandContext(ctx, bin, argv[1:]...) //nolint:gosec // argv, never a shell string
	// A child never gets a controlling terminal: a prompt fails instead of
	// hanging behind the CLI.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if mutation {
		c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
		c.WaitDelay = MutationGrace
	}
	c.Env = append(os.Environ(), "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0")
	if input != "" {
		c.Stdin = strings.NewReader(input)
	}
	out, err := c.CombinedOutput()
	res := Result{Out: strings.TrimRight(string(out), "\n")}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			res.Code = ee.ExitCode()
			return res
		}
		res.Code = -1
		if ctx.Err() != nil {
			res.Err = ctx.Err()
		} else {
			res.Err = err
		}
	}
	return res
}

// Quote renders one argument so a POSIX shell reads it back as that same
// single argument (the rule of Python's shlex.quote).
func Quote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, needsQuote) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

// Join quotes each argument that needs it and joins them with spaces.
func Join(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

func needsQuote(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune("@%+=:,./-_", r):
		return false
	}
	return true
}

// Fake answers reads from a table keyed by the joined argv. Unknown
// commands return Default (or exit 127 when Default is unset).
type Fake struct {
	Answers map[string]Result
	// Prefixes answer any command line that starts with the key (the
	// longest matching key wins), for commands with computed arguments.
	Prefixes map[string]Result
	Default  *Result
	Asked    []string
}

// Read implements Reader.
func (f *Fake) Read(_ context.Context, argv ...string) Result {
	key := Join(argv)
	f.Asked = append(f.Asked, key)
	if r, ok := f.Answers[key]; ok {
		return r
	}
	best := -1
	var res Result
	for k, r := range f.Prefixes {
		if strings.HasPrefix(key, k) && len(k) > best {
			best, res = len(k), r
		}
	}
	if best >= 0 {
		return res
	}
	if f.Default != nil {
		return *f.Default
	}
	return Result{Code: 127, Err: ErrNotFound}
}
