// Command basalt-agent runs AI coding agents confined by SELinux (see
// docs/agents.md). The same static binary is installed under several
// names; the name it is started as selects the role:
//
//	basalt-agent         the launcher (user command line)
//	basalt-agent-exec    native-mode entry into basalt_agent_t
//	basalt-agent-proxy   the session egress proxy (basalt_agent_proxy_t)
//	basalt-agent-grant   root helper behind pkexec for grants
//	basalt-agent-entry   container entry point inside the tool image
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/cli"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/entry"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/native"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/proxy"
)

var version = "dev"

// roleBuild is set per role at build time only so the installed binaries
// differ in content and are not hard-linked together (they carry distinct
// SELinux types). It has no runtime effect.
var roleBuild string

func main() {
	_ = roleBuild
	cli.Version = version
	switch filepath.Base(os.Args[0]) {
	case "basalt-agent-exec":
		err := native.Exec(os.Args[1:])
		fmt.Fprintf(os.Stderr, "basalt-agent-exec: %v\n", err)
		os.Exit(126)
	case "basalt-agent-proxy":
		if err := proxy.RunProcess(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "basalt-agent-proxy: %v\n", err)
			os.Exit(1)
		}
	case "basalt-agent-grant":
		os.Exit(cli.GrantMain(os.Args[1:]))
	case "basalt-agent-entry":
		os.Exit(entry.Run(os.Args[1:]))
	default:
		os.Exit(cli.Main(os.Args[1:]))
	}
}
