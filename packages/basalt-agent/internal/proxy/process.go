package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
)

// Config is the first line the launcher writes to the proxy process.
type Config struct {
	// Listen is "unix:PATH" or "tcp:127.0.0.1:FIRST-LAST" (the first free
	// port of the range, which carries the SELinux type
	// basalt_agent_proxy_port_t).
	Listen string   `json:"listen"`
	Allow  []string `json:"allow"`
	Token  string   `json:"token,omitempty"`
}

// Command is any later line from the launcher: {"op":"allow","entry":"name:port"}.
type Command struct {
	Op    string `json:"op"`
	Entry string `json:"entry"`
}

// Message is what the proxy process writes to the launcher, one per line.
type Message struct {
	Ready    bool      `json:"ready,omitempty"`
	Addr     string    `json:"addr,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
	Added    string    `json:"added,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// listen opens the listener described by spec.
func listen(spec string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(spec, "unix:"); ok {
		_ = os.Remove(path)
		l, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		// The launcher removes the session directory; in container mode the
		// socket is relabeled for the container and the proxy may not
		// unlink it.
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		return l, os.Chmod(path, 0o600)
	}
	rest, ok := strings.CutPrefix(spec, "tcp:")
	if !ok {
		return nil, fmt.Errorf("bad listen spec %q", spec)
	}
	host, ports, err := net.SplitHostPort(rest)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("the proxy listens on loopback only, not %q", host)
	}
	lo, hi, _ := strings.Cut(ports, "-")
	first, err1 := strconv.Atoi(lo)
	last, err2 := strconv.Atoi(hi)
	if hi == "" {
		last, err2 = first, nil
	}
	if err1 != nil || err2 != nil || first < 1 || last < first || last > 65535 {
		return nil, fmt.Errorf("bad port range %q", ports)
	}
	var lastErr error
	for p := first; p <= last; p++ {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			return l, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no free port in %s: %w", ports, lastErr)
}

// RunProcess is the proxy process: configuration and commands on in,
// readiness and decisions on out. It returns when in is closed (the
// launcher ended), so a session's proxy never outlives its launcher.
func RunProcess(in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	emit := func(m Message) {
		b, _ := json.Marshal(m)
		mu.Lock()
		_, _ = out.Write(append(b, '\n'))
		mu.Unlock()
	}
	br := bufio.NewReader(in)
	first, err := br.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("no configuration on stdin: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(first, &cfg); err != nil {
		return fmt.Errorf("bad configuration: %w", err)
	}
	list := allowlist.New(nil)
	for _, a := range cfg.Allow {
		e, err := allowlist.ParseEntry(a)
		if err != nil {
			return err
		}
		list.Add(e)
	}
	l, err := listen(cfg.Listen)
	if err != nil {
		emit(Message{Error: err.Error()})
		return err
	}
	defer l.Close()
	s := &Server{List: list, Token: cfg.Token, OnDecision: func(d Decision) { emit(Message{Decision: &d}) }}
	go func() { _ = s.Serve(l) }()
	emit(Message{Ready: true, Addr: l.Addr().String()})

	sc := bufio.NewScanner(br)
	for sc.Scan() {
		var c Command
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil || c.Op != "allow" {
			emit(Message{Error: "bad command"})
			continue
		}
		e, err := allowlist.ParseEntry(c.Entry)
		if err != nil {
			emit(Message{Error: err.Error()})
			continue
		}
		list.Add(e)
		emit(Message{Added: e.String()})
	}
	return nil
}
