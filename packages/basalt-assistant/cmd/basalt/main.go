// Command basalt is the Basalt OS system assistant's command line.
package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/cli"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gatelink"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sandbox"
)

var version = "dev"

func main() {
	// Internal: a config checker run without side effects (see package
	// sandbox); the diagnosers start it, never a person.
	if len(os.Args) > 1 && os.Args[1] == sandbox.Command {
		os.Exit(sandbox.Main(os.Args[2:]))
	}
	// `basalt NAME ...` for a NAME the command does not have runs
	// basalt-NAME from /usr/libexec/basalt or /usr/bin (never PATH), so
	// `basalt ledger` is basalt-ledger. The program replaces this process.
	if len(os.Args) > 1 {
		if p, ok := cli.External(os.Args[1]); ok {
			err := syscall.Exec(p, append([]string{p}, os.Args[2:]...), os.Environ())
			fmt.Fprintln(os.Stderr, "basalt:", p+":", err)
			os.Exit(126)
		}
	}
	gatelink.Version = version
	os.Exit(cli.Main(os.Args[1:], version))
}
