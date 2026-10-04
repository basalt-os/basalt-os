package explain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
)

// Prompts for the optional humanize layer. The model writes only the
// prose (what is wrong, why, what applying would do); the template keeps
// the commands, the apply line, the risk and the undo.
const (
	HumanizePrompt = `You explain a finding of the Basalt OS system assistant to the person who looks after this computer. ` +
		`Write two to four short, friendly sentences in plain English: what is wrong, the main evidence, ` +
		`and what applying the planned changes would do (only if there are changes). ` +
		`Use only the facts given and copy names, paths and numbers exactly. ` +
		`Never write commands, never suggest anything that is not in the facts, no markdown.`
	// HumanizeCompactPrompt is the prompt of a model fine-tuned for this
	// (basalt-render-*), as in eval/tools/train-render-lora.py.
	HumanizeCompactPrompt = `Explain this Basalt OS finding in plain, friendly English from the facts only. No commands.`
)

// Input is what the model receives for one finding.
type Input struct {
	Kind    string            `json:"kind"`
	Cause   string            `json:"cause"`
	Subject string            `json:"subject,omitempty"`
	Values  map[string]string `json:"values,omitempty"`
	Flags   []string          `json:"flags,omitempty"`
	Changes []Change          `json:"changes,omitempty"`
}

// ModelInput builds the model's input from facts and planned changes.
func ModelInput(f *Facts, changes []Change) Input {
	in := Input{Kind: f.Kind, Cause: f.Cause, Subject: f.Subject, Values: f.Values, Changes: changes}
	for _, fl := range []struct {
		on   bool
		name string
	}{{f.Hint, "hint_to_confirm"}, {f.Review, "needs_review"}, {f.Incomplete, "incomplete"}, {f.OK, "nothing_wrong"}} {
		if fl.on {
			in.Flags = append(in.Flags, fl.name)
		}
	}
	return in
}

// UserMessage is the user turn: the input as compact JSON.
func UserMessage(in Input) string {
	b, _ := json.Marshal(in)
	return string(b)
}

// Humanizer asks a model (local or, by explicit opt-in, remote) to write
// the prose of a finding, and keeps it only if it passes the faithfulness
// check.
type Humanizer struct {
	C        *llm.Client
	Prompt   string // auto (by the model's name), full or compact
	MaxChars int
	// Model is the served model's name, for Prompt = auto.
	Model string
	// Sent receives the request body before it leaves the process when the
	// endpoint is remote (the person sees what leaves the machine).
	Sent func(endpoint string, body []byte)
}

// Result of one humanize call.
type Result struct {
	Text     string        // the text to show: the model's when accepted, else the template's
	Model    string        // the model's raw text
	Accepted bool          // the model's text passed the check
	Problems []string      // why it was rejected
	Streamed bool          // some of it was already written to the stream
	Elapsed  time.Duration // model time
	Redacted int           // values redacted before a remote request
	Err      error         // the model call failed (the template is used)
}

func (h *Humanizer) prompt() string {
	switch h.Prompt {
	case "compact":
		return HumanizeCompactPrompt
	case "full":
		return HumanizePrompt
	}
	if strings.HasPrefix(h.Model, "basalt-render-") || strings.HasPrefix(h.C.Model, "basalt-render-") {
		return HumanizeCompactPrompt
	}
	return HumanizePrompt
}

func (h *Humanizer) maxChars() int {
	if h.MaxChars > 0 {
		return h.MaxChars
	}
	return DefaultMaxChars
}

// Request builds the model request for a finding (redacted when the
// endpoint is remote) and the facts the check uses.
func (h *Humanizer) Request(f *Facts, acts []action.Action) (llm.Request, *Facts, []action.Action, int) {
	changes := Changes(acts)
	n := 0
	if h.C.Remote() {
		f, changes, n = Redact(f, changes)
		acts = make([]action.Action, len(changes))
		for i, c := range changes {
			acts[i] = action.Action{Kind: c.Kind, Params: c.Params}
		}
	}
	return llm.Request{Messages: []llm.Message{{Role: "system", Content: h.prompt()},
		{Role: "user", Content: UserMessage(ModelInput(f, changes))}}, MaxTokens: 220}, f, acts, n
}

// errReject stops a stream at the first sentence that fails the check.
var errReject = errors.New("rejected")

// Write asks the model for the prose of f. With stream set, accepted
// sentences are passed to it one at a time as they arrive (each one is
// checked before it is shown); the first sentence that fails stops the
// model. Whatever happens, Result.Text is a text that may be shown: the
// model's when the whole of it passed, else the template's.
func (h *Humanizer) Write(ctx context.Context, f *Facts, acts []action.Action, template string, stream func(sentence string)) Result {
	req, rf, racts, nred := h.Request(f, acts)
	res := Result{Text: template, Redacted: nred}
	if h.C.Remote() && h.Sent != nil {
		b, _ := json.MarshalIndent(h.C.Body(req, stream != nil), "", "  ")
		h.Sent(h.C.Endpoint, b)
	}
	al := NewAllowed(rf, racts)
	max := h.maxChars()
	if stream == nil {
		resp, err := h.C.Complete(ctx, req)
		res.Elapsed, res.Model = resp.Elapsed, strings.TrimSpace(resp.Text)
		if err != nil {
			res.Err = err
			return res
		}
		txt := Normalize(res.Model)
		res.Problems = al.Check(txt, max)
		if HasRedaction(txt) {
			res.Problems = append(res.Problems, "repeats a redacted value")
		}
		if len(res.Problems) == 0 {
			res.Text, res.Accepted = txt, true
		}
		return res
	}
	var buf, shown strings.Builder
	emit := func(final bool) error {
		for {
			s := buf.String()
			cut := sentenceEnd(s, final)
			if cut < 0 {
				return nil
			}
			sent := Normalize(s[:cut])
			buf.Reset()
			buf.WriteString(s[cut:])
			if sent == "" {
				continue
			}
			candidate := strings.TrimSpace(shown.String() + " " + sent)
			// Per sentence: nothing outside the facts, no forbidden
			// advice, the running length. Naming the subject is checked
			// on the whole text at the end.
			var bad []string
			for _, p := range al.Check(sent, max) {
				if !strings.HasPrefix(p, "does not name") {
					bad = append(bad, p)
				}
			}
			if len(candidate) > max {
				bad = append(bad, "longer than the limit")
			}
			if HasRedaction(sent) {
				bad = append(bad, "repeats a redacted value")
			}
			if len(bad) > 0 {
				res.Problems = bad
				return errReject
			}
			shown.Reset()
			shown.WriteString(candidate)
			stream(sent)
			res.Streamed = true
		}
	}
	resp, err := h.C.Stream(ctx, req, func(d string) error {
		buf.WriteString(d)
		return emit(false)
	})
	res.Elapsed, res.Model = resp.Elapsed, strings.TrimSpace(resp.Text)
	if err == nil {
		err = emit(true)
	}
	switch {
	case errors.Is(err, errReject):
		return res
	case err != nil:
		res.Err = err
		return res
	}
	full := shown.String()
	if p := al.Check(full, max); len(p) > 0 {
		res.Problems = p
		return res
	}
	res.Text, res.Accepted = full, true
	return res
}

// sentenceEnd returns the index just after the first complete sentence in
// s (a ., ! or ? followed by white space), the whole text when final, or
// -1. A period inside a number, path or name (no space after it) does not
// end a sentence.
func sentenceEnd(s string, final bool) int {
	for i := 0; i < len(s)-1; i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && (s[i+1] == ' ' || s[i+1] == '\n') {
			return i + 1
		}
	}
	if final && strings.TrimSpace(s) != "" {
		return len(s)
	}
	return -1
}
