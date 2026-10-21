package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
)

// pending --json and show --json carry the title in the reader's language
// next to the English one, and only when it is translated.
func TestTitleLocalInJSON(t *testing.T) {
	i18n.Use(&i18n.Catalog{Lang: "pt_BR", Messages: map[string][]string{"turn the %s channel off": {"desligar o canal %s"}}})
	t.Cleanup(i18n.Reset)
	b, err := json.Marshal(localized(&proposal.Proposal{ID: "p-000001", Title: "turn the basalt-tools channel off"}))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"title":"turn the basalt-tools channel off"`) || !strings.Contains(s, `"title_local":"desligar o canal basalt-tools"`) || !strings.Contains(s, `"id":"p-000001"`) {
		t.Errorf("%s", s)
	}
	b, _ = json.Marshal(localized(&proposal.Proposal{ID: "p-000002", Title: "restart nginx.service"}))
	if strings.Contains(string(b), "title_local") {
		t.Errorf("untranslated title got a title_local: %s", b)
	}
}
