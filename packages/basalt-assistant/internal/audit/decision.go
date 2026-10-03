package audit

import (
	"fmt"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/decide"
)

// LogDecision implements decide.Logger: every decision goes into the chain
// with its question, options, probabilities and the action taken.
func (l *Log) LogDecision(d decide.Decision) error {
	text := fmt.Sprintf("%s [%s] -> %s (p=%.2f, threshold %.2f): %s",
		d.Question.ID, d.Question.Subject, d.Answer.Top, d.Answer.Confidence, d.Threshold, d.Action)
	_, err := l.Append("decision", text, d)
	return err
}
