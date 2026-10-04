// Package native runs an agent on the host in the SELinux domain
// basalt_agent_t.
//
// The launcher (the user's own, unconfined process) starts
// /usr/libexec/basalt-agent/basalt-agent-exec, whose file type
// basalt_agent_exec_t is the domain's entry point: the policy's type
// transition puts the process into basalt_agent_t, and runcon adds the
// session's MCS level. The helper then refuses to continue unless it
// really runs in basalt_agent_t, sets no_new_privs (setuid and file
// capabilities stop working, so sudo, su and pkexec cannot raise
// privileges), changes to the project and executes the agent with a clean
// environment.
package native

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/basalt-os/basalt-os/packages/basalt-agent/internal/selinux"
)

// Paths of the helper programs and SELinux names.
const (
	ExecHelper  = "/usr/libexec/basalt-agent/basalt-agent-exec"
	ProxyHelper = "/usr/libexec/basalt-agent/basalt-agent-proxy"
	Domain      = "basalt_agent_t"
	ProxyDomain = "basalt_agent_proxy_t"
	ProjectType = "basalt_agent_project_t"
	ReadOnlyTyp = "basalt_agent_project_ro_t"
	HomeType    = "basalt_agent_home_t"
	ToolType    = "basalt_agent_tool_t"
	SessionType = "basalt_agent_session_t"
	// ProxyPorts carry the type basalt_agent_proxy_port_t.
	ProxyPorts = "47100-47163"
)

// Runcon returns the command that starts program in domain at level.
func Runcon(domain, level, program string, args ...string) []string {
	return append([]string{"runcon", "-t", domain, "-l", level, "--", program}, args...)
}

// Env builds the clean environment of a native session.
func Env(home, tools string, proxyEnv, profileEnv, secrets map[string]string) []string {
	env := map[string]string{
		"HOME": home, "USER": os.Getenv("USER"), "LOGNAME": os.Getenv("USER"), "SHELL": "/bin/bash",
		"PATH":            strings.Join([]string{filepath.Join(tools, "bin"), filepath.Join(tools, "venv/bin"), "/usr/local/bin", "/usr/bin"}, ":"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"), "XDG_DATA_HOME": filepath.Join(home, ".local/share"),
		"XDG_CACHE_HOME": filepath.Join(home, ".cache"), "XDG_STATE_HOME": filepath.Join(home, ".local/state"),
		"TMPDIR": "/tmp",
	}
	for _, k := range []string{"TERM", "COLORTERM", "LANG", "LC_ALL", "TZ", "COLUMNS", "LINES"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for _, m := range []map[string]string{proxyEnv, profileEnv, secrets} {
		for k, v := range m {
			env[k] = v
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

const prSetNoNewPrivs = 38

// Exec is the helper's main: it runs inside basalt_agent_t, then replaces
// itself with the agent. args: PROJECT COMMAND [ARGS...]; the environment
// is already the session's.
func Exec(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: basalt-agent-exec PROJECT COMMAND [ARGS...]")
	}
	cur, err := selinux.Current()
	if err != nil {
		return fmt.Errorf("cannot read the SELinux context: %w", err)
	}
	c, err := selinux.Parse(cur)
	if err != nil || c.Type != Domain {
		return fmt.Errorf("refusing to run outside %s (current context %q): is the basalt-agent SELinux module installed?", Domain, cur)
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	if err := os.Chdir(args[0]); err != nil {
		return err
	}
	path, err := exec.LookPath(args[1])
	if err != nil {
		return fmt.Errorf("%s not found in the session PATH (basalt-agent install PROFILE): %w", args[1], err)
	}
	return syscall.Exec(path, args[1:], os.Environ())
}
