package titles

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-assistant/internal/i18n"
)

// usePO loads po/<lang>.po as the current catalog (single messages only,
// which is what titles use).
func usePO(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[string][]string{}
	plain := map[string]string{}
	var id, str string
	var target *string
	unq := func(s string) string {
		v, err := strconv.Unquote(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("%s: bad string %s", path, s)
		}
		return v
	}
	flush := func() {
		if id != "" && str != "" {
			msgs[id] = []string{str}
			plain[id] = str
		}
		id, str, target = "", "", nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "msgid "):
			id = unq(line[len("msgid "):])
			target = &id
		case strings.HasPrefix(line, "msgstr "):
			str = unq(line[len("msgstr "):])
			target = &str
		case strings.HasPrefix(line, `"`) && target != nil:
			*target += unq(line)
		case strings.HasPrefix(line, "msgid_plural") || strings.HasPrefix(line, "msgstr["):
			target = nil
		}
	}
	flush()
	i18n.Use(&i18n.Catalog{Lang: "pt_BR", Messages: msgs, Plural: func(n int) int {
		if n > 1 {
			return 1
		}
		return 0
	}})
	t.Cleanup(i18n.Reset)
	return plain
}

// sample fills the verbs of a form with recognizable values.
func sample(form string) (string, []string) {
	var vals []string
	out := reVerb.ReplaceAllStringFunc(form, func(v string) string {
		if v == "%%" {
			return "%"
		}
		var s string
		switch v[len(v)-1] {
		case 'd':
			s = strconv.Itoa(41 + len(vals))
		case 'f':
			s = "93"
		default:
			s = "val" + strconv.Itoa(len(vals)+1) + ".x"
		}
		vals = append(vals, s)
		return s
	})
	return out, vals
}

// Every form and every value phrase is translated, with its values in
// place, and the English stays English.
func TestEveryFormTranslated(t *testing.T) {
	tr := usePO(t, "../../po/pt_BR.po")
	for _, list := range [][]string{forms, values} {
		for _, f := range list {
			pt, ok := tr[f]
			if !ok {
				t.Errorf("no pt_BR translation of %q", f)
				continue
			}
			in, vals := sample(f)
			got := Local(in)
			if len(list) == len(values) {
				got = value(in, 0) // a phrase inside a title, never a title
			}
			if want := fill(pt, vals); got != want {
				t.Errorf("%q shown as %q, want %q", in, got, want)
			}
		}
	}
	i18n.Use(&i18n.Catalog{Messages: map[string][]string{}})
	for _, f := range forms {
		if in, _ := sample(f); Local(in) != in {
			t.Errorf("English: Local(%q) = %q", in, Local(in))
		}
	}
}

// Titles as the producers write them, values that are phrases or titles
// included, and text no form knows.
func TestLocal(t *testing.T) {
	usePO(t, "../../po/pt_BR.po")
	for in, want := range map[string]string{
		"turn the basalt-tools channel off":                             "desligar o canal basalt-tools",
		"nginx.service stopped: port already in use":                    "nginx.service parou: porta já em uso",
		"nginx.service stopped: something new":                          "nginx.service parou: something new",
		"root file system 93 % full: large journal":                     "sistema de arquivos raiz 93 % cheio: journal grande",
		"root file system 97 % full":                                    "sistema de arquivos raiz 97 % cheio",
		"SELinux blocked httpd_t (read) on /srv/www: wrong file label":  "O SELinux bloqueou httpd_t (read) em /srv/www: rótulo de arquivo errado",
		"Applying \"turn the basalt-tools channel off\" did not finish": "Aplicar \"desligar o canal basalt-tools\" não terminou",
		"Applying \"something; else\" did not finish":                   "Aplicar \"something; else\" não terminou",
		"restart and install 12 updates (40 MB to download)":            "reiniciar e instalar 12 atualizações (40 MB para baixar)",
		"roll back to snapshot 7 (2026-10-20 10:00, \"before dnf\")":    "voltar ao snapshot 7 (2026-10-20 10:00, \"before dnf\")",
		"download 3 updates (checked against the repositories' signing keys); install 3 updates, exactly the versions shown": "baixar 3 atualizações (conferidas com as chaves de assinatura dos repositórios); instalar 3 atualizações, exatamente as versões mostradas",
		"install the definition of the basalt-nonfree repository (it comes off); turn the basalt-nonfree repository on; " +
			"install nvidia-driver (NVIDIA's userspace, unmodified, and the signed open kernel modules; nouveau goes off) and the module for the running kernel 6.17.1-300.fc44.x86_64; " +
			"make the next start a trial: it checks the driver and goes back to nouveau if it fails": "instalar a definição do repositório basalt-nonfree (ele vem desligado); ligar o repositório basalt-nonfree; " +
			"instalar nvidia-driver (o espaço de usuário da NVIDIA, sem modificações, e os módulos abertos do kernel assinados; o nouveau é desligado) e o módulo para o kernel em uso 6.17.1-300.fc44.x86_64; " +
			"fazer da próxima inicialização um teste: ela confere o driver e volta ao nouveau se ele falhar",
		"restart nginx.service; something unknown": "reiniciar nginx.service; something unknown",
		"a title nobody knows":                     "a title nobody knows",
		"":                                         "",
	} {
		if got := Local(in); got != want {
			t.Errorf("Local(%q)\n got  %q\n want %q", in, got, want)
		}
	}
}
