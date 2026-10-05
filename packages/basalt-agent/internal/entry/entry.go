// Package entry is the container entry point of a session (the
// basalt-agent binary copied into the tool image as basalt-agent-entry).
//
// It opens a loopback forwarder (127.0.0.1:3128) to the session proxy's
// Unix socket (the container has no other network), runs the agent and
// returns its exit status. API keys never enter the container: the agent
// has placeholders and the session proxy adds the real credential.
package entry

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/podman"
)

// Forward accepts on l and connects each client to the Unix socket sock.
func Forward(l net.Listener, sock string) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("unix", sock)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
			<-done
		}()
	}
}

// Run is the entry point's main.
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "basalt-agent-entry: no command")
		return 2
	}
	l, err := net.Listen("tcp", podman.ProxyAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "basalt-agent-entry: proxy forwarder: %v\n", err)
		return 2
	}
	go Forward(l, podman.ProxySocket)

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The terminal delivers ^C to the agent itself (same process group);
	// a stop of the container is passed on.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "basalt-agent-entry: %v\n", err)
		return 127
	}
	go func() {
		for s := range sig {
			if s != syscall.SIGINT && s != syscall.SIGQUIT {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}
