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
