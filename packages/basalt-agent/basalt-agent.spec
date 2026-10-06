# basalt-agent: run AI coding agents (Claude Code, Codex CLI, Gemini CLI,
# Aider) confined by SELinux, in a rootless podman container or in a native
# SELinux domain, with a per-session egress allowlist, API keys held by the
# session proxy (never by the agent) and a hash-chained session audit log. One static Go binary installed
# under several names; the SELinux policy ships in the -selinux subpackage.

%global selinuxtype targeted
%global goipath github.com/basalt-os/basalt-os/packages/basalt-agent
%global debug_package %{nil}

Name:           basalt-agent
Version:        0.4.1
Release:        1%{?dist}
Summary:        Run AI coding agents confined by SELinux (container or native)
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.25
BuildRequires:  systemd-rpm-macros
BuildRequires:  selinux-policy-devel
BuildRequires:  make
BuildRequires:  bzip2

Requires:       podman
Requires:       nftables
Requires:       systemd
# Native mode is default-deny in the kernel through basalt-resolver.
Requires:       basalt-resolver
Recommends:     basalt-ledger
Requires:       polkit
Requires:       /usr/bin/runcon
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})
%{?systemd_requires}

%description
basalt-agent runs an AI coding agent CLI inside a confinement profile, so
a prompt-injected or buggy agent cannot reach SSH keys, keyrings, browser
profiles, other projects or the network beyond a per-profile allowlist.

Container mode (default) uses rootless podman: the project is mounted with
a unique SELinux MCS category per session, the tool image is read only, the
container has no network of its own and reaches the model API and package
registries only through the session's filtering proxy. API keys never
enter the session: the proxy reads them from the launcher, adds them only
to requests for the provider host each key belongs to (over verified TLS)
and removes credential headers from requests to any other host; the agent
sees placeholders.

Native mode enters the SELinux domain basalt_agent_t by a type transition
from the launcher: the agent reads and writes only the project (relabeled
on demand) and its own configuration, runs system tools, and reaches the
network only through the session proxy. Widening a session (one more host
or directory) is a polkit action with administrator authentication, and is
audited. Profiles ship for Claude Code, Codex CLI, Gemini CLI and Aider,
with a template for others.

%package selinux
Summary:        SELinux policy for basalt-agent
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
Requires(post): policycoreutils
%{?selinux_requires}

%description selinux
The SELinux policy for basalt-agent: the agent family base module
(basalt_agent_base: the shared types and the rules no agent domain may
ever be given), the launcher policy (basalt_agent_t and the session proxy
domain basalt_agent_proxy_t) and the proxy port type.

%prep
%setup -q -c

%build
export GOFLAGS="-mod=mod -trimpath" GOTOOLCHAIN=local GOPROXY=off
# One binary per role (distinct content via main.roleBuild, so the files
# are not hard-linked together and each keeps its own SELinux type).
for role in launcher exec proxy grant; do
    go build -buildmode=pie \
        -ldflags "-B gobuildid -X main.version=%{version}-%{release} -X main.roleBuild=$role" \
        -o bin/basalt-agent-$role ./cmd/basalt-agent
done
# The base module declares the family types; build it first.
make -C selinux -f %{_datadir}/selinux/devel/Makefile basalt_agent_base.pp basalt_agent.pp
bzip2 -9 selinux/basalt_agent_base.pp selinux/basalt_agent.pp

%check
export GOFLAGS="-mod=mod" GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
# The launcher in PATH; the role helpers in libexec (own SELinux types).
install -Dpm 0755 bin/basalt-agent-launcher %{buildroot}%{_bindir}/basalt-agent
for n in exec proxy grant; do
    install -Dpm 0755 bin/basalt-agent-$n %{buildroot}%{_libexecdir}/basalt-agent/basalt-agent-$n
done

# Profiles and shared egress lists.
install -d %{buildroot}%{_datadir}/basalt-agent/profiles %{buildroot}%{_datadir}/basalt-agent/egress
install -pm 0644 dist/profiles/*.conf %{buildroot}%{_datadir}/basalt-agent/profiles/
install -pm 0644 dist/profiles/template.conf.example %{buildroot}%{_datadir}/basalt-agent/profiles/
install -pm 0644 dist/egress/*.list %{buildroot}%{_datadir}/basalt-agent/egress/
install -d %{buildroot}%{_sysconfdir}/basalt-agent/profiles %{buildroot}%{_sysconfdir}/basalt-agent/egress

# Egress nftables rule, its unit and preset.
install -Dpm 0644 dist/basalt-agent.nft %{buildroot}%{_datadir}/basalt-agent/basalt-agent.nft
install -Dpm 0644 dist/basalt-agent-egress.service %{buildroot}%{_unitdir}/basalt-agent-egress.service
install -Dpm 0644 dist/80-basalt-agent.preset %{buildroot}%{_presetdir}/80-basalt-agent.preset

# polkit action for grants.
install -Dpm 0644 dist/org.basalt-os.agent.policy %{buildroot}%{_datadir}/polkit-1/actions/org.basalt-os.agent.policy

# SELinux policy packages.
install -Dpm 0644 selinux/basalt_agent_base.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent_base.pp.bz2
install -Dpm 0644 selinux/basalt_agent.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent.pp.bz2
install -Dpm 0644 selinux/basalt_agent_ports.cil %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent_ports.cil
install -Dpm 0644 selinux/basalt_agent_base.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/basalt_agent_base.if
install -Dpm 0644 selinux/basalt_agent.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/basalt_agent.if

install -d licenses && install -pm 0644 LICENSE licenses/

%post
%systemd_post basalt-agent-egress.service

%preun
%systemd_preun basalt-agent-egress.service

%postun
%systemd_postun basalt-agent-egress.service

%pre selinux
%selinux_relabel_pre -s %{selinuxtype}

%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent_base.pp.bz2
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent.pp.bz2
# The port type lives in a CIL module loaded after the base one declares it.
semodule -s %{selinuxtype} -i %{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent_ports.cil 2>/dev/null || :

%postun selinux
if [ $1 -eq 0 ]; then
    semodule -s %{selinuxtype} -r basalt_agent_ports 2>/dev/null || :
    %selinux_modules_uninstall -s %{selinuxtype} basalt_agent
    %selinux_modules_uninstall -s %{selinuxtype} basalt_agent_base
fi

%posttrans selinux
%selinux_relabel_post -s %{selinuxtype}

%files
%license licenses/LICENSE
%{_bindir}/basalt-agent
%dir %{_libexecdir}/basalt-agent
%{_libexecdir}/basalt-agent/basalt-agent-exec
%{_libexecdir}/basalt-agent/basalt-agent-proxy
%{_libexecdir}/basalt-agent/basalt-agent-grant
%dir %{_datadir}/basalt-agent
%dir %{_datadir}/basalt-agent/profiles
%dir %{_datadir}/basalt-agent/egress
%{_datadir}/basalt-agent/profiles/*
%{_datadir}/basalt-agent/egress/*
%{_datadir}/basalt-agent/basalt-agent.nft
%dir %{_sysconfdir}/basalt-agent
%dir %{_sysconfdir}/basalt-agent/profiles
%dir %{_sysconfdir}/basalt-agent/egress
%{_unitdir}/basalt-agent-egress.service
%{_presetdir}/80-basalt-agent.preset
%{_datadir}/polkit-1/actions/org.basalt-os.agent.policy

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent_base.pp.bz2
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent.pp.bz2
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_agent_ports.cil
%{_datadir}/selinux/devel/include/distributed/basalt_agent_base.if
%{_datadir}/selinux/devel/include/distributed/basalt_agent.if

%changelog
* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.4.1-1
- Native mode: the agent may listen on and reach TCP ports 1024 and up
  (SELinux module basalt_agent 0.3.1), so dev and test servers it starts
  (Go's httptest, npm run dev) and Claude Code's sign-in, which waits for
  the browser's redirect on 127.0.0.1, work; the sign-in used to fail
  with "permission denied 127.0.0.1:0". The kernel filter still keeps the
  session on loopback and its allowlist, and the firewall refuses inbound
  connections. Found in the daily-driver lab on the desktop edition.
- Probes Claude Code makes and does not need (/dev, /proc/kcore,
  /proc/sys/fs, the devpts root, /dev/log) are refused quietly, so allowed
  work leaves no denials in the log.
- tests/normal.sh: a local server and a client of it, in both modes.

* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.4.0-1
- The approval gate (ADR 0020 phase 2, docs/gate.md): basalt-agent grant
  and egress propose are requests to the gate when it is installed. Where
  it decides (enforce = agent), the person approves with basalt-gate
  approve (started on the terminal) or in the queue, a rule may decide,
  and the change is then made as before (the root helper keeps polkit);
  otherwise the terminal decides and the gate is told (shadow mode).
  Without the gate nothing changes. Vendored gate client
  (internal/gateclient, MIT OR Apache-2.0, checked against pkg/gate in CI).

* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 0.3.1-1
- Container images build on the Fedora release from the major number of
  os-release's VERSION_ID (Basalt OS 44.0 is Fedora 44), as basalt-release
  44-8 writes VERSION_ID=44.0 (docs/versioning.md).

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.3.0-1
- Security: API keys never enter an agent session. The agent gets a
  placeholder and a plain-HTTP base URL for its provider (for example
  ANTHROPIC_BASE_URL=http://api.anthropic.com); the session proxy removes
  the agent's credential headers, adds the real key only on requests to
  the host the key belongs to and forwards them over TLS verified against
  the system trust store. Requests to other hosts lose their credential
  headers and are refused if they carry a key. Before, the key was in the
  agent's environment (native) or in a file the agent read (container), so
  a prompt-injected agent could send it to an allowlisted host.
- Built-in routes for Anthropic, OpenAI, Gemini and OpenRouter keys;
  profiles add or replace them with "route = ..." in [secrets]. A key
  without a usable route is not used and the session says why.
- The proxy is non-dumpable and runs with a minimal environment; keys
  reach it only on the launcher's pipe. Container sessions no longer
  mount a secrets file.
- SELinux: the secret store (~/.config/basalt-agent/secrets) has its own
  type, basalt_agent_secret_t, which no agent domain may open, read or
  list (neverallow);
  agent domains may not read, trace or signal the proxy (neverallow); the
  proxy reads the system trust store. The family neverallow rules now
  hold under expand-check against the stock policy: they forbid opening
  credential files, not the map/read/append on already open descriptors
  that Fedora's base policy grants every domain.
- Audit: credential.use (the key's name, never its value) and
  credential.strip records; session.start lists the credentials and any
  withheld keys, session.end counts uses.
- BASALT_AGENT_SESSION is set inside every session (both modes), so tools
  such as basalt-prompt can show the session.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.1-1
- The session proxy may connect to any TCP port: the allowlist and the
  kernel filter decide where it goes, so "private" entries on non-HTTP
  ports (a model server on 11434) work through the proxy with direct
  egress off. basalt_agent_direct_egress stays off by default.
- Profile template: documents loopback = yes|no (default yes, which also
  reaches the host's own addresses) and private entries on other ports.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- Sessions run in their own systemd slice; with basalt-resolver the slice
  is default-deny in the kernel (nftables by cgroup, DNS-aware sets), the
  session proxy included. Native mode requires it (fails closed).
- SELinux boolean basalt_agent_direct_egress (off): native agents may also
  connect without the proxy, filtered by the kernel.
- Records are also sent to basalt-ledger; grants widen the kernel filter.
- basalt-agent egress propose: persistent allowlist changes with preview,
  confirmation, polkit for system scope, and audit.
- Profiles: [egress] loopback = yes|no.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: basalt-agent launcher with container (rootless podman,
  per-session MCS, read-only tool image, no container network) and native
  (SELinux domain basalt_agent_t) modes; per-session filtering egress proxy
  with a DNS-aware allowlist; per-session secrets; hash-chained session
  audit log; profiles for Claude Code, Codex CLI, Gemini CLI and Aider;
  polkit-gated, audited grants. SELinux agent family base module
  (basalt_agent_base) shared with the desktop shell's AI client domain.
