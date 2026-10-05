// Package podman builds the rootless podman command lines for container
// mode and the Containerfile of a profile's tool image.
//
// A session container has:
//   - no network at all (--network none): the only way out is the session
//     proxy's Unix socket, mounted into the container, behind a loopback
//     forwarder started by the entry point;
//   - the SELinux type container_t with an MCS level unique to the
//     session; the project, the agent home and the session files are
//     relabeled to that level (":Z"), so no other session can open them;
//   - a read-only root file system (the tool image layers), tmpfs for /tmp,
//     /run and /var/tmp, no capabilities, no new privileges;
//   - only the project, the profile's own home and the proxy socket
//     mounted; nothing else from the user's home, and no API key (the
//     session proxy holds the keys, the agent sees placeholders).
package podman

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/profile"
)

// In-container paths.
const (
	WorkDir      = "/work"
	AgentHome    = "/home/agent"
	SessionDir   = "/run/basalt-agent"
	ProxySocket  = SessionDir + "/proxy.sock"
	EntryPath    = "/usr/local/libexec/basalt-agent-entry"
	ProxyAddr    = "127.0.0.1:3128"
	ImagePrefix  = "localhost/basalt-agent-"
	BaseImageFmt = "registry.fedoraproject.org/fedora:%s"
)

// Image is the local image name of a profile.
func Image(name string) string { return ImagePrefix + name + ":latest" }

// Spec describes one session container.
type Spec struct {
	Session     string
	Image       string
	Level       string
	Project     string // host path
	AgentHome   string // host path
	ProxySocket string // host path
	Env         map[string]string
	Command     []string
	TTY         bool
	ReadOnly    []string // project-relative paths mounted read-only over the project (.git/hooks, .git/config)
}

// RunArgs returns the arguments after "podman".
func RunArgs(s Spec) []string {
	a := []string{"run", "--rm", "-i", "--name", "basalt-agent-" + s.Session,
		"--network", "none",
		"--read-only", "--read-only-tmpfs",
		"--cap-drop", "all", "--security-opt", "no-new-privileges",
		"--security-opt", "label=level:" + s.Level,
		"--userns", "keep-id",
		"--pids-limit", "4096", "--init",
		"--label", "org.basalt-os.agent.session=" + s.Session,
		"--workdir", WorkDir,
		"--entrypoint", EntryPath,
	}
	if s.TTY {
		a = append(a, "-t")
	}
	a = append(a,
		"-v", s.Project+":"+WorkDir+":Z",
		"-v", s.AgentHome+":"+AgentHome+":Z",
		"-v", s.ProxySocket+":"+ProxySocket+":Z",
	)
	for _, ro := range s.ReadOnly {
		a = append(a, "-v", filepath.Join(s.Project, ro)+":"+filepath.Join(WorkDir, ro)+":ro")
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "--env", k+"="+s.Env[k])
	}
	a = append(a, s.Image)
	return append(a, s.Command...)
}

// SessionEnv is the environment of a container session: the base
// variables, the proxy, session (BASALT_AGENT_SESSION, credential
// placeholders and base URLs), then the profile's [env]. Keys are never
// here: they stay in the session proxy.
func SessionEnv(pr *profile.Profile, session map[string]string) map[string]string {
	env := map[string]string{
		"HOME": AgentHome, "USER": "agent", "LOGNAME": "agent", "SHELL": "/bin/bash",
		"PATH": "/usr/local/bin:/usr/bin:/opt/agent/bin",
	}
	for k, v := range ProxyEnv("http://" + ProxyAddr) {
		env[k] = v
	}
	for k, v := range session {
		env[k] = v
	}
	for k, v := range pr.Env {
		env[k] = v
	}
	return env
}

// ProxyEnv points the usual proxy variables at url.
func ProxyEnv(url string) map[string]string {
	return map[string]string{
		"HTTPS_PROXY": url, "HTTP_PROXY": url, "https_proxy": url, "http_proxy": url,
		"ALL_PROXY": url, "all_proxy": url, "NO_PROXY": "", "no_proxy": "",
		// Node 24+ built-in fetch honors the variables only with this set.
		"NODE_USE_ENV_PROXY": "1",
		// Python tools use the system CA bundle through the tunnel.
		"REQUESTS_CA_BUNDLE": "/etc/pki/tls/certs/ca-bundle.crt",
	}
}

// Containerfile returns the image recipe of a profile. The entry point is
// the basalt-agent binary itself (static), copied into the build context
// as basalt-agent-entry.
func Containerfile(pr *profile.Profile, fedora string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# basalt-agent tool image for profile %s (generated)\n", pr.Name)
	fmt.Fprintf(&b, "FROM "+BaseImageFmt+"\n", fedora)
	pkgs := append([]string{"ca-certificates", "findutils", "procps-ng", "less"}, pr.Packages...)
	fmt.Fprintf(&b, "RUN dnf -y install --setopt=install_weak_deps=False %s && dnf clean all\n", strings.Join(pkgs, " "))
	switch pr.Method {
	case "npm":
		fmt.Fprintf(&b, "RUN npm install -g --no-fund --no-audit %s && npm cache clean --force\n", pr.Package)
	case "pip":
		fmt.Fprintf(&b, "RUN python3 -m venv /opt/agent && /opt/agent/bin/pip install --no-cache-dir %s\n", pr.Package)
	}
	fmt.Fprintf(&b, "COPY basalt-agent-entry %s\n", EntryPath)
	fmt.Fprintf(&b, "LABEL org.basalt-os.agent.profile=%q\n", pr.Name)
	return b.String()
}
