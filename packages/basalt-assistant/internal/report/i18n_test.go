package report

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/action"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/diag"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/proposal"
	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/titles"
)

// pseudo is a catalog that "translates" every message of the template
// into itself between « and », so a rendering in it shows which text went
// through the catalog and which did not.
func pseudo(t *testing.T) i18n.Tr {
	t.Helper()
	b, err := os.ReadFile("../../po/basalt-assistant.pot")
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string][]string{}
	var id, plural string
	var target *string
	flush := func() {
		if id != "" {
			if plural != "" {
				msgs[id] = []string{"«" + id + "»", "«" + plural + "»"}
			} else {
				msgs[id] = []string{"«" + id + "»"}
			}
		}
		id, plural, target = "", "", nil
	}
	unq := func(s string) string {
		v, err := strconv.Unquote(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("bad string %s", s)
		}
		return v
	}
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "msgid_plural "):
			plural = unq(line[len("msgid_plural "):])
			target = &plural
		case strings.HasPrefix(line, "msgid "):
			id = unq(line[len("msgid "):])
			target = &id
		case strings.HasPrefix(line, "msgstr"):
			target = nil
		case strings.HasPrefix(line, `"`) && target != nil:
			*target += unq(line)
		}
	}
	flush()
	return i18n.With(&i18n.Catalog{Lang: "xx", Messages: msgs, Plural: func(n int) int {
		if n == 1 {
			return 0
		}
		return 1
	}})
}

// unmarked removes every «...» span (they nest: a translated value inside
// a translated sentence).
func unmarked(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '«':
			depth++
		case r == '»' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// reEnglish finds words that only English prose has: what is left of a
// rendering once the translated text, the commands and the values are
// taken out must not have any.
var reEnglish = regexp.MustCompile(`(?i)\b(the|is|are|was|to|of|and|it|this|will|be|for|with|not|by|on|in|from|has|its|you|your|that|can|nothing|what|why|when|run|applied|pending|found)\b`)

// TestBodiesAreTranslated renders every scenario, in every status, with
// the pseudo catalog: everything a person reads is a catalog message, a
// command or a value (names, paths, numbers, log lines); the evidence and
// the decisions are data and stay as they are.
func TestBodiesAreTranslated(t *testing.T) {
	tr := pseudo(t)
	ps := scenarios()
	// Reports stored without facts, as the command line and the updates
	// write them.
	channel := base("cli", "channel", "basalt-tools", "channel:disable:basalt-tools")
	channel.Title = "turn the basalt-tools channel off"
	channel.Report = "Turn the basalt-tools channel off: no more updates come from it. What is installed from it stays."
	channel.Actions = []action.Action{act(action.RepoDisable, "repo", "basalt-tools")}
	ps["channel-off"] = channel
	ex := FromExecutor("daemon", &diag.UnitReport{Unit: "basalt-apply@p-f3604a_b7c416ad.service",
		State: map[string]string{"Result": "exit-code"}}, nil)
	ps["executor"] = ex
	all := map[string]*proposal.Proposal{}
	for name, p := range ps {
		all[name] = p
		all[name+"-seen"] = withStatus(p, p.Status, func(q *proposal.Proposal) { q.Seen, q.Severity, q.NeedsReview = 3, 5, true })
		for _, st := range []string{proposal.Applied, proposal.Failed, proposal.Ignored, proposal.Resolved, proposal.Scheduled} {
			all[name+"-"+st] = withStatus(p, st, func(q *proposal.Proposal) {
				if st == proposal.Applied || st == proposal.Failed {
					q.Result = &proposal.Result{OK: st == proposal.Applied, PreSnapshot: 60, PostSnapshot: 61}
				}
			})
		}
	}
	for name, p := range all {
		text := RenderWith(p, Options{Tr: tr, Verbose: true}).String()
		for _, cut := range []string{"«Evidence»", "«How sure the assistant is»"} {
			if i := strings.Index(text, cut); i >= 0 {
				text = text[:i]
			}
		}
		text += "\n" + LineIn(tr, p) + "\n"
		left := unmarked(text)
		for _, v := range values(p) {
			left = strings.ReplaceAll(left, v, " ")
		}
		for _, l := range strings.Split(left, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "$ ") {
				continue
			}
			if w := reEnglish.FindString(l); w != "" {
				t.Errorf("%s: %q is not translated (%q), in:\n%s", name, w, l, text)
				break
			}
		}
	}
}

func withStatus(p *proposal.Proposal, st string, f func(*proposal.Proposal)) *proposal.Proposal {
	q := *p
	q.Status = st
	f(&q)
	return &q
}

// values are the parts of a rendering that are not text to translate:
// ids, names, paths, numbers and log lines from the facts and the
// actions, the commands, and the free text of a report stored before
// facts existed, longest first. Titles and hint reasons are not values:
// they must be forms internal/titles knows.
func values(p *proposal.Proposal) []string {
	vs := []string{p.ID, p.Kind, p.Subject, p.Source}
	if p.Facts == nil && !titles.KnownReport(p.Report) {
		vs = append(vs, p.Report) // free text stored before facts existed
	}
	if f := p.Facts; f != nil {
		vs = append(vs, f.Subject)
		for _, k := range f.Keys() {
			vs = append(vs, f.V(k))
		}
	}
	acts := append([]action.Action{}, p.Actions...)
	for _, h := range p.Hints {
		acts = append(acts, h.Actions...)
	}
	for _, a := range acts {
		for _, v := range a.Params {
			vs = append(vs, v)
		}
		cs, _ := a.Commands()
		for _, c := range cs {
			vs = append(vs, c.String())
		}
	}
	var out []string
	for _, v := range vs {
		if len(strings.TrimSpace(v)) > 1 {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// pt_BR: the body the desktop shows for a channel proposal and for the
// same proposal applied, in the words a person reads.
func TestChannelProposalInPortuguese(t *testing.T) {
	tr := ptBR(t)
	p := base("cli", "channel", "basalt-tools", "channel:disable:basalt-tools")
	p.ID = "p-a7c547"
	p.Title = "turn the basalt-tools channel off"
	p.Report = "Turn the basalt-tools channel off: no more updates come from it. What is installed from it stays."
	p.Actions = []action.Action{act(action.RepoDisable, "repo", "basalt-tools")}
	got := RenderWith(p, Options{Tr: tr}).String()
	for _, want := range []string{
		"[p-a7c547] desligar o canal basalt-tools",
		"Aguardando sua decisão, encontrada pelo basalt em ",
		"Desligar o canal basalt-tools: não chegam mais atualizações dele.",
		"O que vai rodar (exatamente estes comandos, como root, nesta ordem)",
		"  $ dnf config-manager setopt basalt-tools.enabled=0",
		"Risco: baixo (não chegam mais atualizações de basalt-tools; o que está",
		"Desfazer: mude o canal de volta em Configurações, Atualizações e canais",
		"Próximo passo",
		"  Aplicar:    sudo basalt apply p-a7c547",
		"--yes --confirm ",
		"  Ignorar:    sudo basalt ignore p-a7c547",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The desktop finds the id and the code in this text.
	if !regexp.MustCompile(`basalt apply (p-[0-9a-f]{6})\s.*--confirm ([0-9a-f]{8})`).MatchString(got) {
		t.Errorf("the apply line the desktop reads is gone:\n%s", got)
	}
	p.Status = proposal.Applied
	got = RenderWith(p, Options{Tr: tr}).String()
	for _, want := range []string{"Aplicada, encontrada pelo basalt", "Os comandos desta proposta"} {
		if !strings.Contains(got, want) {
			t.Errorf("applied: missing %q in:\n%s", want, got)
		}
	}
	if line := LineIn(tr, p); !strings.Contains(line, "aplicada") || !strings.Contains(line, "desligar o canal basalt-tools") {
		t.Errorf("line %q", line)
	}
}

// ptBR loads po/pt_BR.po (single and plural messages).
func ptBR(t *testing.T) i18n.Tr {
	t.Helper()
	b, err := os.ReadFile("../../po/pt_BR.po")
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string][]string{}
	var id string
	var strs []string
	var target *string
	flush := func() {
		if id != "" && len(strs) > 0 {
			msgs[id] = strs
		}
		id, strs, target = "", nil, nil
	}
	unq := func(s string) string {
		v, err := strconv.Unquote(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("bad string %s", s)
		}
		return v
	}
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "msgid_plural "):
			target = nil
		case strings.HasPrefix(line, "msgid "):
			id = unq(line[len("msgid "):])
			target = &id
		case strings.HasPrefix(line, "msgstr"):
			_, v, _ := strings.Cut(line, " ")
			strs = append(strs, unq(v))
			target = &strs[len(strs)-1]
		case strings.HasPrefix(line, `"`) && target != nil:
			*target += unq(line)
		}
	}
	flush()
	return i18n.With(&i18n.Catalog{Lang: "pt_BR", Messages: msgs, Plural: func(n int) int {
		if n > 1 {
			return 1
		}
		return 0
	}})
}
