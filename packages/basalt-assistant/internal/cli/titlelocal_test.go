package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
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
		"disabled":                                               {"desligado"},
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
		`"checks_output_local":["desligado"]`,
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

// basalt status in pt_BR: the labels and the problems in the reader's
// language (they were English under pt_BR, bugs 2026-10-22).
func TestStatusInBrazilianPortuguese(t *testing.T) {
	i18n.Use(&i18n.Catalog{Lang: "pt_BR", Messages: map[string][]string{
		"%d thing needs your attention:":        {"%d coisa precisa da sua atenção:", "%d coisas precisam da sua atenção:"},
		"System":                                {"Sistema"},
		"Denials":                               {"Bloqueios"},
		"%s has failed. See why: basalt why %s": {"%s falhou. Veja por quê: basalt why %s"},
		"none in the last 24 hours":             {"nenhum nas últimas 24 horas"},
		"Everything looks fine.":                {"Está tudo bem."},
	}, Plural: func(n int) int {
		if n > 1 {
			return 1
		}
		return 0
	}})
	t.Cleanup(i18n.Reset)
	var b bytes.Buffer
	st := statusFixture()
	st.Problems = []string{"nginx.service has failed. See why: basalt why nginx.service", "nginx.service has failed. See why: basalt why nginx.service"}
	writeStatus(&b, st, 3, nil)
	out := b.String()
	for _, want := range []string{"2 coisas precisam da sua atenção:", "  - nginx.service falhou. Veja por quê: basalt why nginx.service", "  Sistema ", "  Bloqueios   nenhum nas últimas 24 horas"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	st.Problems = nil
	writeStatus(&b, st, 3, nil)
	if !strings.HasPrefix(b.String(), "Está tudo bem.") {
		t.Errorf("%s", b.String())
	}
}

func statusFixture() diag.Status {
	return diag.Status{OS: diag.OSRelease{Name: "Basalt OS", Version: "44 (Server)"}, SELinux: "Enforcing",
		Disk: diag.FSStat{Total: 32 << 30, Free: 21 << 30}, DiskPct: 34.4, RollbackState: "none", Daemon: "active"}
}

// basalt status --json keeps the English problems and adds problems_local.
func TestStatusJSONLocal(t *testing.T) {
	i18n.Use(&i18n.Catalog{Lang: "pt_BR", Messages: map[string][]string{
		"%s has failed. See why: basalt why %s": {"%s falhou. Veja por quê: basalt why %s"}}})
	t.Cleanup(i18n.Reset)
	st := statusFixture()
	st.Problems = []string{"nginx.service has failed. See why: basalt why nginx.service"}
	b, _ := json.Marshal(statusLocalized(st))
	s := string(b)
	if !strings.Contains(s, `"problems":["nginx.service has failed. See why: basalt why nginx.service"]`) ||
		!strings.Contains(s, `"problems_local":["nginx.service falhou. Veja por quê: basalt why nginx.service"]`) {
		t.Errorf("%s", s)
	}
	i18n.Use(&i18n.Catalog{Messages: map[string][]string{}})
	if b, _ := json.Marshal(statusLocalized(st)); strings.Contains(string(b), "problems_local") {
		t.Errorf("English got problems_local: %s", b)
	}
}
