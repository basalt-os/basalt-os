package engine

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-installer/internal/steps"
)

// Runner executes one command step. out receives every output line
// (standard error always; standard output unless the step captures it).
// The captured standard output is returned when the step has Capture set.
type Runner interface {
	Run(ctx context.Context, s steps.Step, out func(line string)) (captured string, err error)
}

// ExecRunner runs commands with os/exec: the argv as given, no shell, in a
// process group that is killed when ctx is cancelled.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, s steps.Step, out func(string)) (string, error) {
	if len(s.Argv) == 0 {
		return "", errors.New("empty command")
	}
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "SYSTEMD_COLORS=0", "NO_COLOR=1")
	for _, v := range s.Env {
		cmd.Env = append(cmd.Env, v.Name+"="+v.Value)
	}
	if s.Stdin != "" {
		cmd.Stdin = strings.NewReader(s.Stdin)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	var captured bytes.Buffer
	var stdout io.ReadCloser
	if s.Capture != "" {
		cmd.Stdout = &captured
	} else if stdout, err = cmd.StdoutPipe(); err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	var wg sync.WaitGroup
	var lastMu sync.Mutex
	var last []string
	pump := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		sc.Split(scanLinesCR)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), " \t")
			if line == "" {
				continue
			}
			lastMu.Lock()
			last = append(last, line)
			if len(last) > 5 {
				last = last[1:]
			}
			lastMu.Unlock()
			out(line)
		}
	}
	wg.Add(1)
	go pump(stderr)
	if stdout != nil {
		wg.Add(1)
		go pump(stdout)
	}
	waited := make(chan error, 1)
	go func() {
		wg.Wait()
		waited <- cmd.Wait()
	}()
	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case err = <-waited:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-waited
		}
		return "", ctx.Err()
	}
	if err != nil {
		lastMu.Lock()
		tail := strings.Join(last, " | ")
		lastMu.Unlock()
		if tail != "" {
			return "", fmt.Errorf("%s: %w (last output: %s)", s.Argv[0], err, tail)
		}
		return "", fmt.Errorf("%s: %w", s.Argv[0], err)
	}
	return captured.String(), nil
}

// scanLinesCR splits on \n and on \r, so progress lines that redraw with a
// carriage return arrive one by one.
func scanLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// FakeRunner records commands and answers from a table, for tests and the
// --demo mode of the frontends. Nothing is executed.
type FakeRunner struct {
	mu sync.Mutex
	// Ran lists every argv, joined by spaces.
	Ran []string
	// Fail makes a command fail when its joined argv contains the key.
	Fail map[string]string
	// Output is printed for commands whose joined argv contains the key.
	Output map[string][]string
	// Captured is returned for capturing steps.
	Captured string
	// Delay slows every command down (demo mode).
	Delay time.Duration
}

// Run implements Runner.
func (f *FakeRunner) Run(ctx context.Context, s steps.Step, out func(string)) (string, error) {
	joined := strings.Join(s.Argv, " ")
	f.mu.Lock()
	f.Ran = append(f.Ran, joined)
	f.mu.Unlock()
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	for k, lines := range f.Output {
		if strings.Contains(joined, k) {
			for _, l := range lines {
				out(l)
			}
		}
	}
	for k, msg := range f.Fail {
		if strings.Contains(joined, k) {
			return "", errors.New(msg)
		}
	}
	if s.Capture != "" {
		return f.Captured, nil
	}
	return "", nil
}

// Commands returns a copy of the recorded commands.
func (f *FakeRunner) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Ran...)
}
