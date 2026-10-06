// basalt-gate-tty is the terminal decider: the only program a person's
// terminal session runs to decide gate requests. Executing it moves the
// process into the SELinux domain basalt_gate_tty_t (from user domains
// only; agent domains can never enter it), and the gate checks polkit for
// this process on every approval, through the authentication agent the
// `basalt-gate` command registers for it (pkttyagent).
//
//	basalt-gate-tty REQUEST-JSON
//
// It sends one request (a closed set of decider operations) and prints
// the gate's reply as one JSON line. With BASALT_GATE_TTY_SYNC=FD it
// first waits for one byte on that descriptor (the authentication agent
// is ready).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/pkg/gate"
)

// ops a terminal decider may send.
var ops = map[string]bool{"pending": true, "history": true, "status": true, "decide": true, "resume": true,
	"rules.list": true, "rules.draft": true, "rules.simulate": true, "rules.apply": true, "rules.unpause": true}

// interactive ops need a person at a real terminal (polkit asks there).
var interactive = map[string]bool{"decide": true, "resume": true, "rules.apply": true, "rules.unpause": true}

func main() {
	if err := run(); err != nil {
		b, _ := json.Marshal(gate.Reply{Error: err.Error()})
		fmt.Println(string(b))
		os.Exit(1)
	}
}

func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: basalt-gate-tty REQUEST-JSON (run by basalt-gate)")
	}
	var req gate.Request
	if err := json.Unmarshal([]byte(os.Args[1]), &req); err != nil {
		return fmt.Errorf("request: %v", err)
	}
	if !ops[req.Op] {
		return fmt.Errorf("%q is not a decider operation", req.Op)
	}
	if interactive[req.Op] && !(isTerminal(0) && isTerminal(2)) {
		return fmt.Errorf("deciding needs a person at a terminal")
	}
	if fd := os.Getenv("BASALT_GATE_TTY_SYNC"); fd != "" {
		n, err := strconv.Atoi(fd)
		if err != nil || n < 3 || n > 64 {
			return fmt.Errorf("BASALT_GATE_TTY_SYNC=%q", fd)
		}
		f := os.NewFile(uintptr(n), "sync")
		buf := make([]byte, 1)
		if _, err := f.Read(buf); err != nil {
			return fmt.Errorf("the authentication agent did not start")
		}
		f.Close()
	}
	c, err := gate.Detect("", "basalt-gate-tty", "decider")
	if err != nil {
		return err
	}
	defer c.Close()
	rep, err := c.Do(req)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(rep)
	fmt.Println(string(b))
	if !rep.OK {
		os.Exit(1)
	}
	return nil
}
