// Package earlyterm keeps the terminal libraries from querying a serial
// console before the text installer starts.
//
// Bubble Tea's package initialization asks Lip Gloss for the terminal's
// background color, and termenv answers that by writing an OSC 11 and a
// cursor position (DSR) query to the terminal, then waiting up to 5
// seconds for the reply. A serial console (a BMC's serial-over-LAN, virsh
// console, minicom) usually never replies: the installer then starts 5
// seconds late, and the reader of that query swallows whatever the person
// typed meanwhile.
//
// The installer draws its own dark palette, so on a serial line the answer
// is known: this package sets it before Bubble Tea's initialization runs.
// Go initializes the packages that are ready in import path order, and
// "github.com/basalt-os/..." sorts before "github.com/charmbracelet/bubbletea",
// so this package's init runs first as long as it imports Lip Gloss and not
// Bubble Tea. Import it (blank) from the main package.
package earlyterm

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func init() {
	if Serial() || os.Getenv("BASALT_INSTALLER_NO_TERM_QUERY") == "1" {
		lipgloss.SetHasDarkBackground(true)
	}
}

// Serial reports whether standard input is a serial line (ttyS*, ttyAMA*,
// hvc*, ttyUSB*).
func Serial() bool {
	name, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		return false
	}
	for _, p := range []string{"/dev/ttyS", "/dev/ttyAMA", "/dev/hvc", "/dev/ttyUSB"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
