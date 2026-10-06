// Command basalt-apply-exec is how the desktop applies the system
// assistant's proposals when the approval gate does not decide them (no
// gate, or the gate in shadow mode). The desktop never runs dnf, rpm or
// basalt apply itself: it starts a fixed systemd unit, and polkit asks for
// an administrator's password (rule 50-basalt-assistant.rules):
//
//	basalt-apply@ID_CODE.service      basalt apply ID --yes --confirm CODE
//	basalt-updates-check.service      basalt updates check (update.check)
//	basalt-offline-finish.service     basalt __offline-finish (at the start
//	                                  after an offline update)
//	basalt-offline-reboot.service     basalt updates restart (the restart into
//	                                  a staged offline update, after the
//	                                  desktop's countdown)
//	basalt-drivers-refresh.service    basalt drivers refresh (the report the
//	                                  Additional drivers page reads)
//
// SELinux runs it in basalt_apply_t (module basalt_assistant), the
// assistant's executor, which may start dnf in rpm_t like an
// administrator's terminal. The code binds the confirmation to the exact
// commands the person was shown, as with basalt apply at a terminal.
package main

import (
	"fmt"
	"os"
	"regexp"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/cli"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/gatelink"
)

var version = "dev"

var instRe = regexp.MustCompile(`^(p-[0-9a-f]{6})_([0-9a-f]{8})$`)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: basalt-apply-exec apply ID_CODE | check | offline-finish | offline-reboot | drivers-refresh (from the assistant units)")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	gatelink.Version = version
	switch os.Args[1] {
	case "apply":
		if len(os.Args) != 3 {
			usage()
		}
		m := instRe.FindStringSubmatch(os.Args[2])
		if m == nil {
			usage()
		}
		// Recorded with the decision: who confirmed, and where.
		_ = os.Setenv("BASALT_APPLY_VIA", "settings")
		os.Exit(cli.Main([]string{"apply", m[1], "--yes", "--confirm", m[2]}, version))
	case "check":
		if len(os.Args) != 2 {
			usage()
		}
		os.Exit(cli.Main([]string{"updates", "check"}, version))
	case "offline-reboot":
		if len(os.Args) != 2 {
			usage()
		}
		os.Exit(cli.Main([]string{"updates", "restart"}, version))
	case "drivers-refresh":
		if len(os.Args) != 2 {
			usage()
		}
		os.Exit(cli.Main([]string{"drivers", "refresh"}, version))
	case "offline-finish":
		if len(os.Args) != 2 {
			usage()
		}
		os.Exit(cli.Main([]string{"__offline-finish"}, version))
	}
	usage()
}
