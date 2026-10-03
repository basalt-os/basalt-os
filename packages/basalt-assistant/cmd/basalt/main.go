// Command basalt is the Basalt OS system assistant's command line.
package main

import (
	"os"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/cli"
)

var version = "dev"

func main() { os.Exit(cli.Main(os.Args[1:], version)) }
