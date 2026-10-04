package report

import (
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
)

// A confined restore renders as a hint with the root command that confirms
// it, never with an apply line.
func TestRenderHint(t *testing.T) {
	r := &diag.UnitReport{Unit: "nginx.service", Cause: "config_error", Explanation: "nginx.service does not start.",
		Features: map[string]bool{}, Hints: []action.Hint{{Reason: "snapshot 27 holds a different copy of /etc/nginx/nginx.conf",
			Actions: []action.Action{
				{Kind: action.FileRestore, Params: map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "27"}},
				{Kind: action.UnitRestart, Params: map[string]string{"unit": "nginx.service"}}}}}}
	p := FromUnit("daemon", r)
	if len(p.Actions) != 0 || len(p.Hints) != 1 || !p.NeedsReview || p.Validate() != nil {
		t.Fatalf("%+v", p)
	}
	out := Render(p)
	for _, want := range []string{"Hint (not a proposal", "cp --preserve=mode,ownership,timestamps /.snapshots/27/snapshot/etc/nginx/nginx.conf",
		"sudo basalt confirm " + p.ID, "none: this is a report"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "basalt apply") {
		t.Errorf("a hint offers apply:\n%s", out)
	}
	if !strings.Contains(Line(p), "[hint]") {
		t.Errorf("line %q", Line(p))
	}
}
