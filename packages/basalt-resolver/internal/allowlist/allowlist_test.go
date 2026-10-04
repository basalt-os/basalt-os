package allowlist

import (
	"strings"
	"testing"
)

func TestParseEntry(t *testing.T) {
	good := map[string]string{
		"api.anthropic.com":          "api.anthropic.com",
		"API.Anthropic.com.":         "api.anthropic.com",
		"registry.npmjs.org:443,80":  "registry.npmjs.org:80,443",
		"*.githubusercontent.com":    "*.githubusercontent.com",
		"files.pythonhosted.org:443": "files.pythonhosted.org",
	}
	for in, want := range good {
		e, err := ParseEntry(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if e.String() != want {
			t.Errorf("%s: got %s, want %s", in, e.String(), want)
		}
	}
	for _, bad := range []string{"", "*", "localhost", "1.2.3.4", "[::1]", "*.com", "a.*.com", "x.com:0", "x.com:http", "bad host.com", "x.com:70000"} {
		if _, err := ParseEntry(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	l := New(nil)
	for _, s := range []string{"api.anthropic.com", "*.githubusercontent.com", "registry.npmjs.org:80"} {
		e, err := ParseEntry(s)
		if err != nil {
			t.Fatal(err)
		}
		l.Add(e)
	}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"api.anthropic.com", 443, true},
		{"API.ANTHROPIC.COM.", 443, true},
		{"api.anthropic.com", 80, false},
		{"evil-api.anthropic.com", 443, false},
		{"anthropic.com", 443, false},
		{"api.anthropic.com.evil.net", 443, false},
		{"raw.githubusercontent.com", 443, true},
		{"githubusercontent.com", 443, false},
		{"a.b.githubusercontent.com", 443, true},
		{"registry.npmjs.org", 80, true},
		{"registry.npmjs.org", 443, false},
		{"160.79.104.10", 443, false},
	}
	for _, c := range cases {
		if got := l.Allows(c.host, c.port); got != c.want {
			t.Errorf("%s:%d: got %v", c.host, c.port, got)
		}
	}
	// Ports merge into the existing pattern.
	e, _ := ParseEntry("registry.npmjs.org:443")
	l.Add(e)
	if !l.Allows("registry.npmjs.org", 443) || len(l.Entries()) != 3 {
		t.Errorf("merge failed: %v", l.Entries())
	}
}

func TestParseFile(t *testing.T) {
	in := "# registries\nregistry.npmjs.org  # npm\n\npypi.org\n"
	es, err := Parse(strings.NewReader(in))
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
	if _, err := Parse(strings.NewReader("ok.example.com\n10.0.0.1\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("want a line 2 error, got %v", err)
	}
	if got := Format(es); got != "registry.npmjs.org\npypi.org\n" {
		t.Errorf("format: %q", got)
	}
}

func TestPrivateMarker(t *testing.T) {
	e, err := ParseEntry("ollama.lab.example:11434 private")
	if err != nil || !e.Private || e.String() != "ollama.lab.example:11434 private" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := ParseEntry("x.example.com public"); err == nil {
		t.Error("unknown marker accepted")
	}
	l := New([]Entry{e})
	if ok, priv := l.Lookup("ollama.lab.example", 11434); !ok || !priv {
		t.Error("lookup")
	}
}
