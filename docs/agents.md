# Confined AI coding agents (basalt-agent)

`basalt-agent` runs an AI coding agent CLI (Claude Code, Codex CLI, Gemini
CLI, Aider) inside a confinement profile, so a prompt-injected or buggy
agent cannot reach your SSH keys, keyrings, browser profiles, other
projects or the network beyond a short per-profile allowlist. It is the
first piece of ADR 0009 (confining AI agents and apps) and uses the
network and audit shapes of ADR 0010.

```
basalt-agent run claude --project ~/src/app           # container mode (default)
basalt-agent run claude --project ~/src/app --mode native
basalt-agent list                                     # profiles, modes, egress
basalt-agent sessions                                 # running sessions
basalt-agent audit [--session ID] [--verify]          # the session audit log
basalt-agent image build claude                       # build a tool image (container mode)
basalt-agent install claude                           # install for native mode
basalt-agent grant SESSION host api.example.com       # widen a session (admin auth)
basalt-agent grant SESSION path ~/src/lib             # (native) add a directory
```

## The model

Every agent session gets:

- only the project directory it was given, read-write, and its own
  per-profile configuration directory; nothing else from `$HOME`;
- the network only to the names in the profile's egress allowlist, through
  a per-session filtering proxy; everything else is refused and recorded;
- API keys injected for that one session, never readable by another;
- a unique SELinux MCS level, so two sessions cannot see each other.

An agent can *request* more (a host, a directory); a person *grants* it
with administrator authentication (polkit), and the grant is audited.
Agents never confirm their own requests, never escalate, never read
credentials. Safety is in the system, not in the agent.

## Two modes

### Container mode (default, strongest, works today)

Rootless podman. The project is mounted with `:Z` so podman relabels it to
`container_t` with a unique MCS category per session; the tool image layers
are read-only; the container runs with `--read-only`, `--cap-drop all`,
`--security-opt no-new-privileges` and a tmpfs for `/tmp`, `/run` and
`/var/tmp`. The container has **no network of its own** (`--network none`):
the only way out is a Unix socket to the session proxy, mounted in, behind a
loopback forwarder the entry point starts. API keys arrive in a
root-only-inside-the-session secrets file bind-mounted read-only; the entry
point loads them into the environment and never puts them on a command line.

Why rootless podman with no container network and an external proxy, rather
than pasta/slirp egress filtering: the robust, simple rootless option is to
give the container no network at all and force every connection through a
proxy we control, in its own SELinux domain. There is nothing to misconfigure
in the data path (no in-netns firewall to get wrong), the filter is the same
code in both modes, and name-based allow/deny and auditing live in one place.

### Native mode

For agents that need host tools or the host's own caches. The launcher
(your normal, unconfined shell) runs `/usr/libexec/basalt-agent/basalt-agent-exec`,
whose SELinux type `basalt_agent_exec_t` is the entry point of the domain
`basalt_agent_t`; a type transition puts the agent in that domain, and
`runcon` adds the session's MCS level. The helper refuses to continue unless
it really runs in `basalt_agent_t`, sets `no_new_privs` (so setuid, file
capabilities, `sudo`, `su` and `pkexec` cannot raise privileges), changes to
the project and executes the agent.

The project directory is relabeled on demand to `basalt_agent_project_t` at
the session level (`.git/hooks` and `.git/config` to a read-only type, so an
agent cannot plant a hook that later runs with your rights); the agent's home
to `basalt_agent_home_t`; its installed tools to `basalt_agent_tool_t`
(read and execute only). SELinux then allows the agent to read and change
the project, run system and installed tools, use its terminal (without
`TIOCSTI`, so it cannot inject keystrokes) and connect only to the session
proxy port; it denies the rest of the home, other users, other sessions, the
assistant's state, signalling or tracing the launcher, and privilege
escalation.

What SELinux cannot do by itself: pin an outbound connection to a particular
host (it reasons about port types, not names). The session proxy enforces the
names; nftables is the backstop (see below). On the proxy port, SELinux only
lets the agent reach the loopback proxy, not the same port on another host.

## Egress: the allowlist and the proxy

The proxy (`basalt_agent_proxy_t`, the only part of a session that resolves
names or opens connections) is an HTTP proxy for CONNECT tunnels and plain
`http://` requests. For each request it applies the session's allowlist, and:

- refuses IP literals (entries are names, so the policy survives address
  changes);
- after resolving a name, refuses an address that is loopback, private,
  link-local or otherwise non-global, so an allowed name cannot be pointed at
  a local service (DNS rebinding) unless the entry is marked `private`;
- requires a per-session password in native mode (loopback is shared by the
  user's processes; container mode uses a private Unix socket).

### Allowlist format (shared with ADR 0010's resolver)

One entry per line, `#` starts a comment:

```
api.anthropic.com                 # exact name, port 443
registry.npmjs.org:443,80         # explicit ports
*.githubusercontent.com           # any name below the suffix, not the suffix itself
ollama.lab.example:11434 private  # may resolve to a private address (a local model server)
```

Rules: names only (no IP literals, no bare `*`); a wildcard is the first
label only; ports default to 443; `private` relaxes the non-global-address
refusal for that entry. This is the format the future Basalt resolver will
read to fill per-session nftables sets; the proxy and the resolver share it.

### Backstop with nftables (ADR 0010 default-deny)

`basalt-agent-egress.service` loads an nftables table that drops and audits
any connection to the session proxy ports (`tcp 47100-47163`) that is not on
loopback. The proxy ports reach only the local proxy; the proxy is the filter.
This is the first, static step toward ADR 0010's per-session default-deny
sets; when the Basalt resolver lands, the same allowlist fills per-session
sets and the model becomes default-deny for all of a session's egress, not
only the proxy ports.

## Secrets

A profile lists the environment variables it needs (e.g.
`ANTHROPIC_API_KEY`). They are read from
`~/.config/basalt-agent/secrets/PROFILE.env` (a regular file, mode 0600,
owned by you) or, if absent there, from the desktop keyring via
`secret-tool` (`service basalt-agent profile PROFILE name KEY`). Values are
never logged; the audit log records only the names injected. In container
mode they travel in a bind-mounted file at the session MCS level, readable by
no other session; in native mode they are in the agent's environment, which
other agent sessions cannot read (different MCS level, and SELinux denies
cross-session `/proc`). You can also skip keys and log in inside a session;
the login is stored in the profile's own home.

## Profiles

Shipped in `/usr/share/basalt-agent/profiles` (override per host in
`/etc/basalt-agent/profiles`, per user in `~/.config/basalt-agent/profiles`;
the first match wins):

| Profile | Agent | Install | Needs (besides registries) |
|---|---|---|---|
| `claude` | Claude Code | `npm i -g @anthropic-ai/claude-code` | `api.anthropic.com`, login hosts |
| `codex` | Codex CLI | `npm i -g @openai/codex` | `api.openai.com`, login hosts |
| `gemini` | Gemini CLI | `npm i -g @google/gemini-cli` | `generativelanguage.googleapis.com`, login hosts |
| `aider` | Aider | `pip install aider-chat` | the provider(s) you use |

`template.conf.example` is the starting point for another agent. Package
registries come from the shared list `egress/registries.list` (`include =
registries`); GitHub is a separate opt-in list (`include = github`) because
allowing `github.com` also allows pushing with whatever token is in the
project. Trim each profile's egress to what you actually use.

A profile file:

```ini
[agent]
name = claude
command = claude            ; looked up in PATH (the tool dir or the image)

[install]
method = npm                ; npm | pip | none
package = @anthropic-ai/claude-code
packages = nodejs npm git ripgrep   ; dnf packages for the image and native mode

[env]
DISABLE_AUTOUPDATER = 1     ; HOME, PATH, proxy and loader variables are set by basalt-agent

[secrets]
env = ANTHROPIC_API_KEY

[egress]
allow = api.anthropic.com
include = registries
```

## Audit

Each session appends to `~/.local/state/basalt-agent/audit.jsonl`: JSON
lines, one record per event, each chained to the previous by SHA-256
(`basalt-agent audit --verify` checks the chain). The record schema (below)
is the one ADR 0010's `basalt-ledger` will accept unchanged; `basalt-agent`
is one producer of it.

```jsonc
{
  "v": 1, "seq": 140, "time": "2026-10-04T01:58:24Z",
  "producer": "basalt-agent", "uid": 1000, "session": "s-b4896c08ff03",
  "event": "egress.deny", "outcome": "denied",
  "subject": { "profile": "labshell", "mode": "native",
               "level": "s0:c797,c854", "project": "/home/dev/src/app" },
  "data": { "host": "example.com", "port": 443, "reason": "not in the session allowlist" },
  "prev": "38b4c285…", "hash": "e334e9be…"
}
```

Events: `session.start` (command, image, egress list, secret names, relabel
count), `session.end` (exit code, duration, allowed/denied counts),
`egress.allow` / `egress.deny` (host, port, reason), `grant.request` /
`grant.apply`, `relabel`, `install`. `outcome` is `ok`, `allowed`, `denied`
or `error`. Network decisions are recorded here reliably; privilege-escalation
and SELinux denials also appear in the system audit log and journal
(`basalt-agent audit --avc`). Note that Fedora's stock policy suppresses the
audit record (`dontaudit`) for an application domain reading another user's
home content, so a blocked read of `~/.ssh` is denied but not logged as an
AVC; the denial still happens, and the session audit log is the product's
authoritative trail.

## SELinux types

The agent family (ADR 0009 item 5: the desktop shell's AI client is part of
the same family) shares a base module, `basalt_agent_base`, so one model
holds everywhere. It is created and documented here; the desktop shell's AI
client domain builds against it (`basalt_agent_domain_type()` plus the
interfaces it needs, and the `neverallow` rules apply to it too).

| Type | What |
|---|---|
| attribute `basalt_agent_domain` | every agent domain; carries the family `neverallow` rules |
| `basalt_agent_t` | native-mode agent domain |
| `basalt_agent_proxy_t` | the per-session egress proxy |
| `basalt_agent_project_t` | a project handed to a session (relabeled to the session level) |
| `basalt_agent_project_ro_t` | parts of a project kept read-only (`.git/hooks`, `.git/config`) |
| `basalt_agent_home_t` | an agent's own config/login/cache home |
| `basalt_agent_tool_t` | native-mode agent programs (read + execute only) |
| `basalt_agent_session_t` | a session's runtime dir (proxy/control sockets, secrets file) |
| `basalt_agent_proxy_port_t` | the proxy loopback ports (`tcp 47100-47163`) |

Base interfaces (in `basalt_agent_base.if`): `basalt_agent_domain_type`,
`basalt_agent_exec_tools`, `basalt_agent_allow_jit`,
`basalt_agent_manage_project`, `basalt_agent_manage_home`,
`basalt_agent_use_terminals`, `basalt_agent_connect_proxy`,
`basalt_agent_launcher`. The family `neverallow` rules (checked when the
policy is built) forbid any agent domain from reading `ssh_home_t`,
`gpg_secret_t`, `home_cert_t`, `shadow_t` or the generic user home, executing
`sudo`, loading policy or setting enforce/booleans, and gaining
`setuid`/`sys_admin`/`sys_ptrace`/`dac_*` capabilities.

## Limits (today)

- Native mode and SELinux reason about port types, not hostnames, so the
  per-host egress rule is enforced by the proxy, with nftables as the
  loopback backstop. Full per-session default-deny for all egress (not only
  the proxy ports) arrives with ADR 0010's resolver and nftables sets.
- A profile that allows a package registry allows uploads to it too (`npm
  publish`, `twine`); GitHub is opt-in for the same reason. Treat the egress
  list as the trust boundary and keep it short.
- Blocked reads of another user's home content are denied but not AVC-logged
  (Fedora `dontaudit`); the session audit log is the reliable record.
- One session per profile at a time (the profile's home is relabeled to the
  session level). Run different agents, or copies of a profile, in parallel.

## Testing

`make agent-test` runs the Go unit tests in a Fedora container.
`scripts/lab/agent-test.sh` installs the package on a lab VM and runs
`packages/basalt-agent/tests/driver.sh`: allowed work (builds, tests, git,
an allowed `curl`/`npm install`) and the escape-attempt matrix (reading
`~/.ssh`, writing outside the project, reaching an unlisted host, reading
another session's secret, confirming a shell proposal, `sudo`), in both
modes. All escapes must be denied, and allowed work must leave zero AVC
denials.
