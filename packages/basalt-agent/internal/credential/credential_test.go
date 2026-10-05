package credential

import (
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	r := Builtin["ANTHROPIC_API_KEY"]
	if r.Name != "ANTHROPIC_API_KEY" || r.Host != "api.anthropic.com" || r.Port != 443 || r.Header != "X-Api-Key" ||
		r.BaseURL() != "http://api.anthropic.com" {
		t.Errorf("anthropic %+v", r)
	}
	o := Builtin["OPENAI_API_KEY"]
	o.Value = "k"
	if o.HeaderValue() != "Bearer k" || o.BaseURL() != "http://api.openai.com/v1" || o.Upstream() != "api.openai.com:443" {
		t.Errorf("openai %+v", o)
	}
	for n, r := range Builtin {
		if r.Name != n || r.Port != 443 || r.Header == "" || r.Value != "" {
			t.Errorf("%s: %+v", n, r)
		}
	}
}

func TestParse(t *testing.T) {
	r, err := Parse("LAB_KEY Mock.Lab.Test:8443 header=x-api-key scheme=Bearer base_env=LAB_BASE_URL base_path=/v1")
	if err != nil {
		t.Fatal(err)
	}
	want := Route{Name: "LAB_KEY", Host: "mock.lab.test", Port: 8443, Header: "X-Api-Key", Scheme: "Bearer",
		BaseEnv: "LAB_BASE_URL", BasePath: "/v1"}
	if r != want {
		t.Errorf("got %+v", r)
	}
	if again, err := Parse(r.String()); err != nil || again != r {
		t.Errorf("round trip %q: %+v %v", r.String(), again, err)
	}
	for _, bad := range []string{
		"K api.example.com",                            // no header
		"K 10.0.0.1 header=x",                          // address
		"K *.example.com header=x",                     // wildcard
		"K localhost header=x",                         // single label
		"K api.example.com:0 header=x",                 // port
		"K api.example.com header=Proxy-Authorization", // proxy's own
		"K api.example.com header=Host",
		"K api.example.com header=x base_path=v1",   // relative
		"K api.example.com header=x base_path=/a b", // fields
		"K api.example.com header=x foo=bar",
		"1K api.example.com header=x",
		"K api.example.com header=x\r\nEvil",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestHeaders(t *testing.T) {
	h := strings.Join(Headers([]Route{{Header: "x-lab-key"}}), ",")
	for _, want := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "X-Lab-Key", "Proxy-Authorization"} {
		if !strings.Contains(h, want) {
			t.Errorf("missing %s in %s", want, h)
		}
	}
}
