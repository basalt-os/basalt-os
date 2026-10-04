# basalt-agent: run AI coding agents (Claude Code, Codex CLI, Gemini CLI,
# Aider) confined by SELinux, in a rootless podman container or in a native
# SELinux domain, with a per-session egress allowlist, per-session secrets
# and a hash-chained session audit log. One static Go binary installed
# under several names; the SELinux policy ships in the -selinux subpackage.

%global selinuxtype targeted
%global goipath github.com/basalt-os/basalt-os/packages/basalt-agent
%global debug_package %{nil}

Name:           basalt-agent
Version:        0.1.0
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
registries only through the session's filtering proxy, and API keys are
injected per session from a secret file or the keyring.

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
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: basalt-agent launcher with container (rootless podman,
  per-session MCS, read-only tool image, no container network) and native
  (SELinux domain basalt_agent_t) modes; per-session filtering egress proxy
  with a DNS-aware allowlist; per-session secrets; hash-chained session
  audit log; profiles for Claude Code, Codex CLI, Gemini CLI and Aider;
  polkit-gated, audited grants. SELinux agent family base module
  (basalt_agent_base) shared with the desktop shell's AI client domain.
