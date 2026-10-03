package translate

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/llm"
)

// RenderPrompt asks a model to turn a structured diagnosis into a short
// text. Experimental (milestone 2b measured it); the assistant's own text
// templates stay what people see.
const RenderPrompt = `You explain a diagnosis of the Basalt OS system assistant to an administrator. ` +
	`Write at most three short sentences in the language requested: what is wrong, the main evidence, and the proposed change. ` +
	`Use only facts, paths, units and commands that appear in the diagnosis. Never suggest other commands. ` +
	`If there is no proposed change, say that a person must review it.`

// Render asks the model for a short text about a diagnosis (any JSON
// value) in lang ("en" or "pt-BR").
func Render(ctx context.Context, c *llm.Client, diagnosis any, lang string) (string, time.Duration, error) {
	b, err := json.Marshal(diagnosis)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: "system", Content: RenderPrompt},
			{Role: "user", Content: "Language: " + lang + "\nDiagnosis (JSON): " + string(b)}},
		MaxTokens: 160,
	})
	return strings.TrimSpace(resp.Text), resp.Elapsed, err
}
