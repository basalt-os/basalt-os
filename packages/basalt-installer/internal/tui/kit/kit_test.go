package kit

import (
	"strings"
	"testing"

	"github.com/tui-tools/tui-kit/theme"
)

// A long command (the package transaction) must leave the output tail and
// the status line on a 24 line serial console.
func TestProgressLongCommandKeepsTailAndStatus(t *testing.T) {
	p := Progress{Title: "Installing Basalt OS on /dev/vda", Fraction: 0.6,
		Step:    "Step 75 of 112: Install the minimal profile",
		Command: "dnf install " + strings.Repeat("some-package-name ", 60),
		Status:  "Installing: running 1m2s"}
	for i := 0; i < 8; i++ {
		p.Add("output line")
	}
	height := 18
	out := p.View(theme.New(), 76, height, true)
	lines := strings.Split(out, "\n")
	if len(lines) > height {
		t.Fatalf("view has %d lines, more than the %d available", len(lines), height)
	}
	if !strings.Contains(out, "Installing: running 1m2s") {
		t.Fatalf("status line missing:\n%s", out)
	}
	if strings.Count(out, "output line") < progressMinTail {
		t.Fatalf("fewer than %d output lines visible:\n%s", progressMinTail, out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("shortened command not marked:\n%s", out)
	}
}

// Output lines with tabs, control characters or emoji must not be wider
// on the terminal than the layout counts.
func TestProgressAddPlainLine(t *testing.T) {
	var p Progress
	p.Add("\t\U0001F510 key\r\x1b")
	if got, want := p.Tail[0], "    * key"; got != want {
		t.Fatalf("Add stored %q, want %q", got, want)
	}
}
