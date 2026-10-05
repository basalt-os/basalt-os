package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
)

func TestCredentialRoutes(t *testing.T) {
	dir := t.TempDir()
	conf := "[agent]\nname = lab\ncommand = bash\n[secrets]\nenv = ANTHROPIC_API_KEY\nenv = ANTHROPIC_AUTH_TOKEN\nenv = OPENAI_API_KEY\n" +
		"env = NO_ROUTE_KEY\nenv = MISSING_KEY\n[egress]\nallow = api.anthropic.com\n"
	f := filepath.Join(dir, "lab.conf")
	if err := os.WriteFile(f, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	pr, err := profile.ParseFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := (profile.Paths{}).Resolve(pr); err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{"ANTHROPIC_API_KEY": "k1", "ANTHROPIC_AUTH_TOKEN": "k2", "OPENAI_API_KEY": "k3", "NO_ROUTE_KEY": "k4"}
	routes, withheld := credentialRoutes(pr, vals)
	if len(routes) != 1 || routes[0].Name != "ANTHROPIC_API_KEY" || routes[0].Value != "k1" || routes[0].Host != "api.anthropic.com" {
		t.Fatalf("routes %+v", routes)
	}
	why := map[string]string{}
	for _, w := range withheld {
		why[w["name"]] = w["reason"]
	}
	for name, want := range map[string]string{
		"ANTHROPIC_AUTH_TOKEN": "already uses ANTHROPIC_API_KEY",
		"OPENAI_API_KEY":       "not on the session allowlist",
		"NO_ROUTE_KEY":         "no credential route",
	} {
		if !strings.Contains(why[name], want) {
			t.Errorf("%s withheld for %q, want %q", name, why[name], want)
		}
	}
	if _, ok := why["MISSING_KEY"]; ok || len(withheld) != 3 {
		t.Errorf("withheld %v", withheld)
	}
	for _, w := range withheld {
		for _, v := range vals {
			if strings.Contains(w["reason"], v) {
				t.Errorf("a reason carries a key: %v", w)
			}
		}
	}
}

func TestProxyEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-the-user-shell")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	for _, kernel := range []bool{false, true} {
		env := strings.Join(proxyEnv(kernel), "\n")
		if strings.Contains(env, "sk-from") || !strings.Contains(env, "GODEBUG=netdns=go") {
			t.Errorf("proxy env (kernel %v):\n%s", kernel, env)
		}
		if strings.Contains(env, "XDG_RUNTIME_DIR") != kernel {
			t.Errorf("XDG_RUNTIME_DIR with kernel=%v:\n%s", kernel, env)
		}
	}
}
