// Command basalt-gate-exec is the approval gate's executor of the system
// assistant's proposals: basalt-gate-exec@REQUEST.service runs it with the
// gate's request id once a person (or a system rule) approved the request.
// It is `basalt apply REQUEST --gate`: it claims the decision with what it
// is about to run, applies the proposal (snapshots, commands, checks,
// audit record) without asking again, and reports the result to the gate.
// SELinux runs it in basalt_gate_exec_t, the type the gate lets claim.
package main

import (
	"fmt"
	"os"
	"regexp"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/cli"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gatelink"
)

var version = "dev"

var reqRe = regexp.MustCompile(`^g-[0-9a-f]{12}$`)

func main() {
	if len(os.Args) != 2 || !reqRe.MatchString(os.Args[1]) {
		fmt.Fprintln(os.Stderr, "usage: basalt-gate-exec REQUEST (from basalt-gate-exec@REQUEST.service)")
		os.Exit(2)
	}
	gatelink.Version = version
	os.Exit(cli.Main([]string{"apply", os.Args[1], "--gate"}, version))
}
