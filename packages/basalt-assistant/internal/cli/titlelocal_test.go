package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/sources"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/titles"
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

// The body: report_local, status_local and checks_local next to the
// English report, status and result, and nothing local in English.
func TestBodyLocalInJSON(t *testing.T) {
	p := &proposal.Proposal{ID: "p-a7c547", Title: "turn the basalt-tools channel off", Status: proposal.Applied,
		Report:  channelReport("basalt-tools", "disable"),
		Actions: []action.Action{{Kind: action.RepoDisable, Params: map[string]string{"repo": "basalt-tools"}}},
		Result: &proposal.Result{OK: true, Steps: []proposal.Step{{What: "dnf config-manager setopt basalt-tools.enabled=0", OK: true}},
			Checks: []proposal.Step{{What: "the basalt-tools channel is off", OK: true, Output: "disabled"}}}}
	i18n.Use(&i18n.Catalog{Lang: "pt_BR", Messages: map[string][]string{
		"Turn the %s channel off: no more updates come from it.": {"Desligar o canal %s: não chegam mais atualizações dele."},
		"What is installed from it stays.":                       {"O que foi instalado a partir dele fica."},
		"applied":                                                {"aplicada"},
		"the %s channel is off":                                  {"o canal %s está desligado"},
	}})
	t.Cleanup(i18n.Reset)
	b, err := json.Marshal(localized(p))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"report":"Turn the basalt-tools channel off: no more updates come from it. What is installed from it stays."`,
		`"report_local":"Desligar o canal basalt-tools: não chegam mais atualizações dele. O que foi instalado a partir dele fica."`,
		`"status":"applied"`, `"status_local":"aplicada"`,
		`"what":"the basalt-tools channel is off"`, `"checks_local":["o canal basalt-tools está desligado"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	i18n.Use(&i18n.Catalog{Messages: map[string][]string{}})
	b, _ = json.Marshal(localized(p))
	if strings.Contains(string(b), "_local") {
		t.Errorf("English got local fields: %s", b)
	}
}

// The applied result of basalt show: the verdict and what each check
// proves in the reader's language, the commands and their output as they
// are.
func TestWriteResultLocal(t *testing.T) {
	i18n.Use(&i18n.Catalog{Lang: "pt_BR", Messages: map[string][]string{
		"it worked and every check passed":                                        {"funcionou e todas as verificações passaram"},
		"Applied on %s UTC: %s (snapshots %d before, %d after; audit record #%d)": {"Aplicada em %s UTC: %s (snapshots %d antes, %d depois; registro de auditoria #%d)"},
		"the %s channel is off":                                                   {"o canal %s está desligado"},
	}})
	t.Cleanup(i18n.Reset)
	var b bytes.Buffer
	writeResult(&b, i18n.Local(), &proposal.Result{OK: true, Time: time.Date(2026, 10, 21, 1, 58, 0, 0, time.UTC), AuditSeq: 164,
		Steps:  []proposal.Step{{What: "dnf config-manager setopt basalt-tools.enabled=0", OK: true}},
		Checks: []proposal.Step{{What: "the basalt-tools channel is off", OK: true, Output: "disabled"}}})
	want := "\nAplicada em 2026-10-21 01:58 UTC: funcionou e todas as verificações passaram (snapshots 0 antes, 0 depois; registro de auditoria #164)\n" +
		"  [ok] dnf config-manager setopt basalt-tools.enabled=0 \n" +
		"  [ok] o canal basalt-tools está desligado disabled\n"
	if b.String() != want {
		t.Errorf("got\n%q\nwant\n%q", b.String(), want)
	}
}

// Every report the command line stores without facts is made of sentences
// internal/titles knows, so basalt show translates all of it.
func TestStoredReportsAreKnown(t *testing.T) {
	reports := []string{
		channelReport("basalt-tools", "disable"), channelReport("basalt-tools", "enable"),
		channelReport(sources.ChanTesting, "enable"), channelReport(sources.ChanNonfreeTesting, "enable"),
		sourceRemoveReport("Docker CE"),
		keyboardReport("Portuguese (Brazil), English (US)", "br-abnt2"),
		auditReport(false), auditReport(true),
		riskAcceptReport("edimar", "encryption"), riskReviewReport("tpm"),
		"Proposed by an MCP client: the port is wrong",
		"Turn the basalt-tools channel off: no more updates come from it. Confirmed as root: proposed by mcp before hints existed.",
	}
	for _, pinned := range []bool{true, false} {
		for _, signed := range []bool{true, false} {
			reports = append(reports, sourceAddReport("Docker CE", "060A 61C5 1B55 8A7F", "Docker Release (CE rpm)", pinned, signed))
		}
	}
	for _, r := range reports {
		if !titles.KnownReport(r) {
			t.Errorf("a sentence of this report is not in internal/titles: %q", r)
		}
	}
}
