package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/explain"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/report"
)

// humanizer returns the configured humanize layer, or nil when it is off
// or cannot be set up (the template is used, with a note on stderr).
func (a *app) humanizer() *explain.Humanizer {
	if !a.cfg.Humanize || a.o.json || a.o.plain {
		return nil
	}
	c := &llm.Client{Endpoint: a.cfg.HumanizeEndpoint, Model: a.cfg.HumanizeModel,
		AllowRemote: a.cfg.HumanizeAllowRemote, Timeout: a.cfg.HumanizeTimeout}
	if c.Remote() {
		if !a.cfg.HumanizeAllowRemote {
			fmt.Fprintln(os.Stderr, "basalt: [humanize] endpoint is remote but allow_remote is not yes; using the standard text")
			return nil
		}
		if a.cfg.HumanizeModel == "" {
			fmt.Fprintln(os.Stderr, "basalt: [humanize] a remote endpoint needs model = NAME; using the standard text")
			return nil
		}
		if a.cfg.HumanizeAPIKeyFile != "" {
			b, err := os.ReadFile(a.cfg.HumanizeAPIKeyFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "basalt: [humanize] cannot read the API key file (%v); using the standard text\n", err)
				return nil
			}
			c.APIKey = strings.TrimSpace(string(b))
		}
	}
	h := &explain.Humanizer{C: c, Prompt: a.cfg.HumanizePrompt, MaxChars: a.cfg.HumanizeMaxChars}
	if c.Remote() {
		h.Sent = func(ep string, body []byte) {
			fmt.Fprintf(a.out, "Sent to %s (remote model, allow_remote = yes; secrets and personal details redacted):\n%s\n\n", ep, indentBlock(string(body), "  "))
		}
	} else if h.Prompt == "auto" && c.Model == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		h.Model, _ = c.ServedModel(ctx)
		cancel()
	}
	return h
}

func indentBlock(s, ind string) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range ls {
		ls[i] = ind + ls[i]
	}
	return strings.Join(ls, "\n")
}

// render prints a proposal: the template, or with the humanize layer the
// model's prose between the template's head and tail. Accepted sentences
// are streamed to a terminal as they arrive.
func (a *app) render(ctx context.Context, p *proposal.Proposal) {
	parts := report.RenderWith(p, report.Options{Verbose: a.o.verbose})
	h := a.humanizer()
	if h == nil || parts.Facts == nil {
		fmt.Fprint(a.out, parts.String())
		return
	}
	fmt.Fprint(a.out, parts.Head)
	var stream func(string)
	col := 0
	if a.cfg.HumanizeStream && a.tty {
		stream = func(s string) {
			for _, w := range strings.Fields(s) {
				switch {
				case col == 0:
					fmt.Fprint(a.out, "  "+w)
					col = 2 + len(w)
				case col+1+len(w) > report.Width:
					fmt.Fprint(a.out, "\n  "+w)
					col = 2 + len(w)
				default:
					fmt.Fprint(a.out, " "+w)
					col += 1 + len(w)
				}
			}
		}
	}
	tctx, cancel := context.WithTimeout(ctx, a.cfg.HumanizeTimeout)
	res := h.Write(tctx, parts.Facts, parts.Changes, parts.Text.Prose(), stream)
	cancel()
	if a.root {
		data := map[string]any{"proposal": p.ID, "accepted": res.Accepted, "problems": res.Problems,
			"elapsed_ms": res.Elapsed.Milliseconds(), "endpoint_remote": h.C.Remote(), "redacted_values": res.Redacted}
		if res.Err != nil {
			data["error"] = res.Err.Error()
		}
		_, _ = a.audit.Append("humanize", "humanize "+p.ID+": "+map[bool]string{true: "model text", false: "template"}[res.Accepted], data)
	}
	switch {
	case res.Accepted && res.Streamed:
		fmt.Fprintln(a.out)
	case res.Accepted:
		fmt.Fprintln(a.out, report.Wrap(res.Text, "  ", report.Width))
	default:
		if res.Streamed {
			fmt.Fprint(a.out, "\n\n")
		}
		why := "the model is not available"
		if res.Err == nil {
			why = "its text mentioned something that is not in the facts"
		}
		fmt.Fprintf(a.out, "  (Standard text shown: %s.)\n", why)
		fmt.Fprint(a.out, parts.Prose)
	}
	fmt.Fprint(a.out, parts.Tail)
}
