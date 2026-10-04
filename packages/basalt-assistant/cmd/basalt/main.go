// Command basalt is the Basalt OS system assistant's command line.
package main

import (
	"os"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/cli"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sandbox"
)

var version = "dev"

func main() {
	// Internal: a config checker run without side effects (see package
	// sandbox); the diagnosers start it, never a person.
	if len(os.Args) > 1 && os.Args[1] == sandbox.Command {
		os.Exit(sandbox.Main(os.Args[2:]))
	}
	os.Exit(cli.Main(os.Args[1:], version))
}
