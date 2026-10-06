// Package polkit asks polkit whether a process may perform one of the
// gate's actions (org.basalt-os.gate.decide, .decide-admin, .unlock),
// letting polkit authenticate the person through the authentication
// agent registered for that process (pkttyagent in a terminal, the
// desktop's agent in a session). It runs pkcheck, so the gate needs no
// D-Bus library.
package polkit

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-gate/internal/peer"
)

// Actions.
const (
	Decide      = "org.basalt-os.gate.decide"       // C1 to C3 (auth_self_keep)
	DecideAdmin = "org.basalt-os.gate.decide-admin" // C2 system, C4, C5 (auth_admin)
	Unlock      = "org.basalt-os.gate.unlock"       // a locked action, once (auth_admin, plus a second factor)
)

// Checker decides whether a process is authorized for an action.
type Checker interface {
	Check(ctx context.Context, pid, uid int, action string) error
}

// ErrNotAuthorized is returned when polkit says no (or the person
// dismissed the prompt).
var ErrNotAuthorized = errors.New("not authorized")

// Pkcheck is the Checker that runs pkcheck.
type Pkcheck struct {
	Path    string        // default /usr/bin/pkcheck
	Timeout time.Duration // default 2 minutes (the person types a password)
}

// Check runs pkcheck for the process with user interaction allowed.
func (p Pkcheck) Check(ctx context.Context, pid, uid int, action string) error {
	start, err := peer.StartTime(pid)
	if err != nil {
		return fmt.Errorf("%w: the deciding process is gone (%v)", ErrNotAuthorized, err)
	}
	path := p.Path
	if path == "" {
		path = "/usr/bin/pkcheck"
	}
	to := p.Timeout
	if to == 0 {
		to = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	subject := strconv.Itoa(pid) + "," + strconv.FormatUint(start, 10) + "," + strconv.Itoa(uid)
	cmd := exec.CommandContext(ctx, path, "--action-id", action, "--process", subject, "--allow-user-interaction")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 1:
			return fmt.Errorf("%w: polkit refused %s", ErrNotAuthorized, action)
		case 2:
			return fmt.Errorf("%w: the authentication was dismissed", ErrNotAuthorized)
		case 3:
			return fmt.Errorf("%w: no authentication agent answered for the deciding process", ErrNotAuthorized)
		}
	}
	return fmt.Errorf("%w: pkcheck: %v %s", ErrNotAuthorized, err, string(out))
}
