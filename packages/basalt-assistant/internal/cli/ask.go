package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/translate"
)

// ask translates a request in natural language into one `basalt` command
// with the local model, then behaves as if the person had typed that
// command: read-only commands run, a change (apply, rollback) is only
// printed so the person runs it and confirms it as usual. The model never
// runs anything; its answer is constrained by a schema, validated, and
// checked against the request (internal/translate).
func (a *app) ask(ctx context.Context) error {
	text := strings.TrimSpace(strings.Join(a.o.args[1:], " "))
	if text == "" {
		return errors.New(`usage: basalt ask "REQUEST"`)
	}
	if !a.cfg.Translator {
		return errors.New("the translator is off: install basalt-llm, fetch a model, start basalt-llm.service and set [translator] enabled = yes in /etc/basalt/assistant.conf")
	}
	c := &llm.Client{Endpoint: a.cfg.TranslatorEndpoint, Model: a.cfg.TranslatorModel,
		AllowRemote: a.cfg.AllowRemote, Timeout: 60 * time.Second}
	if c.Remote() {
		fmt.Fprintf(a.out, "Note: the request is sent to %s (remote model, allow_remote = yes).\n", c.Endpoint)
	}
	tr := &translate.Translator{C: c, Compact: a.cfg.TranslatorCompact}
	res, err := tr.Translate(ctx, text)
	if err != nil {
		if res.Raw == "" {
			return fmt.Errorf("translator: %v", err)
		}
		res.Intent = translate.Intent{Intent: translate.Clarify}
		res.Grounding = "the model's answer did not validate"
	}
	in := res.Intent
	if a.root {
		_, _ = a.audit.Append("ask", "ask -> "+in.Intent, map[string]any{
			"request": truncate(text, 300), "model_answer": res.Model, "intent": in, "grounding": res.Grounding,
			"endpoint_remote": c.Remote(), "elapsed_ms": res.Elapsed.Milliseconds()})
	}
	switch in.Intent {
	case translate.Clarify:
		fmt.Fprintln(a.out, "I am not sure what to do with that. Please name the service, the proposal id or the snapshot number, for example:")
		fmt.Fprintln(a.out, `  basalt ask "why did nginx fail?"     basalt ask "show p-1a2b3c"     basalt ask "list snapshots"`)
		if res.Grounding != "" {
			fmt.Fprintf(a.out, "(%s)\n", res.Grounding)
		}
		return nil
	case translate.None:
		fmt.Fprintln(a.out, "That is outside what the assistant does. It diagnoses and proposes fixes: status, why UNIT,")
		fmt.Fprintln(a.out, "fix selinux, snapshots, disk, pending proposals. See `basalt help`.")
		return nil
	}
	args := in.Args()
	cmd := "basalt " + strings.Join(args, " ")
	if in.Changes() {
		fmt.Fprintf(a.out, "Understood as: %s\n", cmd)
		fmt.Fprintln(a.out, "This changes the system, so it is not run from a request in natural language.")
		fmt.Fprintf(a.out, "Run it yourself to see the exact commands and confirm them:\n  sudo %s\n", cmd)
		return nil
	}
	fmt.Fprintf(a.out, "Understood as: %s\n\n", cmd)
	if a.o.dryRun {
		return nil
	}
	o, err := parse(args)
	if err != nil {
		return err
	}
	o.json, o.config = a.o.json, a.o.config
	a.o = o
	return a.dispatch(ctx)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
