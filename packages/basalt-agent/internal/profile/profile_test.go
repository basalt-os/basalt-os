package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shipped() Paths {
	return Paths{ProfileDirs: []string{"../../dist/profiles"}, EgressDirs: []string{"../../dist/egress"}}
}

func TestShippedProfiles(t *testing.T) {
	p := shipped()
	names := p.List()
	if strings.Join(names, ",") != "aider,claude,codex,gemini" {
		t.Fatalf("profiles: %v", names)
	}
	for _, n := range names {
		pr, err := p.Load(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if len(pr.SecretEnv) == 0 || len(pr.Egress) == 0 || pr.Method == "none" {
			t.Errorf("%s: incomplete profile %+v", n, pr)
		}
		// Registries are included; GitHub is opt-in only.
		var reg, gh bool
		for _, e := range pr.Egress {
			reg = reg || e.Pattern == "registry.npmjs.org"
			gh = gh || e.Pattern == "github.com"
		}
		if !reg || gh {
			t.Errorf("%s: registries %v github %v", n, reg, gh)
		}
	}
	// The template parses once renamed.
	dir := t.TempDir()
	b, err := os.ReadFile("../../dist/profiles/template.conf.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "myagent.conf"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFile(filepath.Join(dir, "myagent.conf")); err != nil {
		t.Errorf("template: %v", err)
	}
}

func TestUserOverridesSystem(t *testing.T) {
	user := t.TempDir()
	conf := "[agent]\nname = claude\ncommand = claude\n[egress]\nallow = api.anthropic.com\n"
	if err := os.WriteFile(filepath.Join(user, "claude.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	p := shipped()
	p.ProfileDirs = append([]string{user}, p.ProfileDirs...)
	pr, err := p.Load("claude")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Source != filepath.Join(user, "claude.conf") || len(pr.Egress) != 1 {
		t.Errorf("user profile not used: %+v", pr)
	}
}

func TestRejects(t *testing.T) {
	bad := map[string]string{
		"reserved env":  "[agent]\nname = x\ncommand = x\n[env]\nLD_PRELOAD = /tmp/x.so\n",
		"proxy env":     "[agent]\nname = x\ncommand = x\n[env]\nHTTPS_PROXY = http://evil\n",
		"command path":  "[agent]\nname = x\ncommand = /bin/sh\n",
		"ip egress":     "[agent]\nname = x\ncommand = x\n[egress]\nallow = 10.0.0.1\n",
		"bad section":   "[agent]\nname = x\ncommand = x\n[mounts]\nhome = /\n",
		"bad secret":    "[agent]\nname = x\ncommand = x\n[secrets]\nenv = PATH\n",
		"bad package":   "[agent]\nname = x\ncommand = x\n[install]\nmethod = npm\npackage = x; rm -rf /\n",
		"no name":       "[agent]\ncommand = x\n",
		"unknown key":   "[agent]\nname = x\ncommand = x\nmounts = /\n",
		"bad method":    "[agent]\nname = x\ncommand = x\n[install]\nmethod = curl\n",
		"wildcard host": "[agent]\nname = x\ncommand = x\n[egress]\nallow = *\n",
	}
	dir := t.TempDir()
	for what, conf := range bad {
		f := filepath.Join(dir, "x.conf")
		if err := os.WriteFile(f, []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseFile(f); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	p := Paths{ProfileDirs: []string{dir}}
	if _, err := p.Find("../etc/passwd"); err == nil {
		t.Error("path traversal in a profile name accepted")
	}
}

// Every key a shipped profile asks for has a route on a host its egress
// list allows, so no shipped key is ever withheld or given to the agent.
func TestShippedRoutes(t *testing.T) {
	p := shipped()
	for _, n := range p.List() {
		pr, err := p.Load(n)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range pr.SecretEnv {
			r, ok := pr.Route(s)
			if !ok {
				t.Errorf("%s: %s has no route", n, s)
				continue
			}
			allowed := false
			for _, e := range pr.Egress {
				allowed = allowed || e.Matches(r.Host, r.Port)
			}
			if !allowed {
				t.Errorf("%s: %s routes to %s:%d, not on the egress list", n, s, r.Host, r.Port)
			}
		}
	}
}

func TestProfileRoutes(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		f := filepath.Join(dir, "lab.conf")
		if err := os.WriteFile(f, []byte("[agent]\nname = lab\ncommand = bash\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	pr, err := ParseFile(write("[secrets]\nenv = LAB_KEY\nenv = ANTHROPIC_API_KEY\nroute = LAB_KEY mock.lab.test:8443 header=X-Api-Key base_env=LAB_BASE_URL\n" +
		"route = ANTHROPIC_API_KEY mock.lab.test:8443 header=x-api-key base_env=ANTHROPIC_BASE_URL\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := pr.Route("LAB_KEY"); !ok || r.Host != "mock.lab.test" || r.Port != 8443 {
		t.Errorf("LAB_KEY route %+v", r)
	}
	// A profile route replaces the built-in one.
	if r, _ := pr.Route("ANTHROPIC_API_KEY"); r.Host != "mock.lab.test" {
		t.Errorf("override %+v", r)
	}
	if _, ok := pr.Route("NO_SUCH_KEY"); ok {
		t.Error("route for an unknown key")
	}
	for _, bad := range []string{
		"[secrets]\nroute = K 10.1.1.1 header=X\n",
		"[secrets]\nroute = K api.example.com\n",
		"[secrets]\nroute = PATH api.example.com header=X\n",
		"[secrets]\nroute = K api.example.com header=X base_env=HTTPS_PROXY\n",
		"[secrets]\nenv = K\n[env]\nK = sk-in-the-profile\n",
		"[env]\nBASALT_AGENT_SESSION = s-000000000000\n",
	} {
		if _, err := ParseFile(write(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}
