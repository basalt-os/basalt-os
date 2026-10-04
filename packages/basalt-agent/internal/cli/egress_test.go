package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/allowlist"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/native"
	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
)

func TestEditProfile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.conf")
	conf := "[agent]\nname = demo\ncommand = demo\n\n[egress]\nallow = api.example.com\ninclude = registries\n\n[env]\nX = 1\n"
	if err := os.WriteFile(src, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out", "demo.conf")
	e, _ := allowlist.ParseEntry("git.example.org:443,22")
	if err := editProfile(src, dst, "add", e); err != nil {
		t.Fatal(err)
	}
	pr, err := profile.ParseFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Allow) != 2 || pr.Allow[1].String() != "git.example.org:22,443" || pr.Env["X"] != "1" {
		t.Fatalf("%+v", pr)
	}
	rm, _ := allowlist.ParseEntry("api.example.com")
	if err := editProfile(dst, dst, "remove", rm); err != nil {
		t.Fatal(err)
	}
	pr, _ = profile.ParseFile(dst)
	if len(pr.Allow) != 1 || pr.Allow[0].Pattern != "git.example.org" || len(pr.Includes) != 1 {
		t.Fatalf("%+v", pr)
	}
}

func TestAgentEnvStripsLauncherVariables(t *testing.T) {
	env := native.AgentEnv([]string{"HOME=/h", "XDG_RUNTIME_DIR=/run/user/1000", "DBUS_SESSION_BUS_ADDRESS=x", "PATH=/usr/bin"})
	if got := strings.Join(env, " "); got != "HOME=/h PATH=/usr/bin" {
		t.Fatal(got)
	}
}
