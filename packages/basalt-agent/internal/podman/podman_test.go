package podman

import (
	"strings"
	"testing"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
)

func TestRunArgs(t *testing.T) {
	pr := &profile.Profile{Name: "claude", Env: map[string]string{"DISABLE_AUTOUPDATER": "1"}}
	s := Spec{Session: "s-0123456789ab", Image: Image("claude"), Level: "s0:c7,c42",
		Project: "/home/u/src/app", AgentHome: "/home/u/.local/share/basalt-agent/home/claude",
		ProxySocket: "/run/user/1000/basalt-agent/s-0123456789ab/proxy.sock",
		SecretsFile: "/run/user/1000/basalt-agent/s-0123456789ab/secrets.env",
		Env:         SessionEnv(pr), Command: []string{"claude", "--version"}, TTY: true,
		ReadOnly: []string{".git/hooks"}}
	a := strings.Join(RunArgs(s), " ")
	for _, want := range []string{
		"--network none", "--read-only --read-only-tmpfs", "--cap-drop all", "no-new-privileges",
		"label=level:s0:c7,c42", "--userns keep-id",
		"-v /home/u/src/app:/work:Z", "-v /home/u/.local/share/basalt-agent/home/claude:/home/agent:Z",
		"proxy.sock:/run/basalt-agent/proxy.sock:Z", "secrets.env:/run/basalt-agent/secrets.env:Z,ro",
		"-v /home/u/src/app/.git/hooks:/work/.git/hooks:ro",
		"--env HTTPS_PROXY=http://127.0.0.1:3128", "--env DISABLE_AUTOUPDATER=1",
		"localhost/basalt-agent-claude:latest claude --version",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %q in\n%s", want, a)
		}
	}
	for _, bad := range []string{"API_KEY", "--privileged", "label=disable", "--network host", ":z"} {
		if strings.Contains(a, bad) {
			t.Errorf("unexpected %q in\n%s", bad, a)
		}
	}
}

func TestContainerfile(t *testing.T) {
	p := profile.Paths{ProfileDirs: []string{"../../dist/profiles"}, EgressDirs: []string{"../../dist/egress"}}
	for name, want := range map[string]string{"claude": "npm install -g --no-fund --no-audit @anthropic-ai/claude-code",
		"aider": "/opt/agent/bin/pip install --no-cache-dir aider-chat"} {
		pr, err := p.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		cf := Containerfile(pr, "44")
		if !strings.Contains(cf, want) || !strings.Contains(cf, "FROM registry.fedoraproject.org/fedora:44") ||
			!strings.Contains(cf, "COPY basalt-agent-entry "+EntryPath) {
			t.Errorf("%s:\n%s", name, cf)
		}
	}
}
