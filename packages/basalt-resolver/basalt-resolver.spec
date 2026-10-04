# basalt-resolver: per-session default-deny egress for confined sessions
# (AI agents, confined apps). A local DNS resolver answers only the names
# on a session's allowlist and fills per-session nftables sets matched by
# cgroup; everything else is dropped and recorded. Static Go binary, the
# SELinux policy in the -selinux subpackage.

%global selinuxtype targeted
%global debug_package %{nil}

Name:           basalt-resolver
Version:        0.1.0
Release:        1%{?dist}
Summary:        Per-session default-deny egress with DNS-aware allowlists
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.25
BuildRequires:  systemd-rpm-macros
BuildRequires:  selinux-policy-devel
BuildRequires:  make
BuildRequires:  bzip2

Requires:       nftables
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})
%{?systemd_requires}

%description
basalt-resolver gives every registered session (an AI agent session, a
confined app) its own network policy: default deny, enforced in the kernel
by nftables rules matched on the session's cgroup. The session's DNS goes
to a resolver port of its own, which answers only names on the session's
allowlist (the basalt-agent allowlist format), refuses names that point at
local or private addresses unless the entry allows it (DNS rebinding), and
adds each address it hands out to the session's set for the allowed ports.
Direct connections to other addresses, other DNS servers and unlisted names
are dropped and recorded in basalt-ledger. Allowlists only grow through
administrator-authorized grants.

%package selinux
Summary:        SELinux policy for basalt-resolver
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
Requires(post): policycoreutils
%{?selinux_requires}

%description selinux
The SELinux policy for basalt-resolver: the service domain
basalt_resolver_t, its runtime directory type and the session resolver
port type (tcp and udp 47200-47263).

%prep
%setup -q -c

%build
export GOFLAGS="-mod=mod -trimpath" GOTOOLCHAIN=local GOPROXY=off
for role in daemon cli; do
    go build -buildmode=pie \
        -ldflags "-B gobuildid -X main.version=%{version}-%{release} -X main.roleBuild=$role" \
        -o bin/basalt-resolver-$role ./cmd/basalt-resolver
done
make -C selinux -f %{_datadir}/selinux/devel/Makefile basalt_resolver.pp
bzip2 -9 selinux/basalt_resolver.pp

%check
export GOFLAGS="-mod=mod" GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
install -Dpm 0755 bin/basalt-resolver-daemon %{buildroot}%{_libexecdir}/basalt-resolver/basalt-resolverd
install -Dpm 0755 bin/basalt-resolver-cli %{buildroot}%{_bindir}/basalt-resolver
install -Dpm 0644 dist/basalt-resolver.service %{buildroot}%{_unitdir}/basalt-resolver.service
install -Dpm 0644 dist/80-basalt-resolver.preset %{buildroot}%{_presetdir}/80-basalt-resolver.preset
install -Dpm 0644 dist/resolver.conf %{buildroot}%{_sysconfdir}/basalt-resolver/resolver.conf
install -Dpm 0644 selinux/basalt_resolver.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_resolver.pp.bz2
install -Dpm 0644 selinux/basalt_resolver_ports.cil %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_resolver_ports.cil
install -Dpm 0644 selinux/basalt_resolver.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/basalt_resolver.if
install -d licenses && install -pm 0644 LICENSE licenses/

%post
%systemd_post basalt-resolver.service

%preun
%systemd_preun basalt-resolver.service

%postun
%systemd_postun_with_restart basalt-resolver.service

%pre selinux
%selinux_relabel_pre -s %{selinuxtype}

%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/basalt_resolver.pp.bz2
semodule -s %{selinuxtype} -i %{_datadir}/selinux/packages/%{selinuxtype}/basalt_resolver_ports.cil 2>/dev/null || :

%postun selinux
if [ $1 -eq 0 ]; then
    semodule -s %{selinuxtype} -r basalt_resolver_ports 2>/dev/null || :
    %selinux_modules_uninstall -s %{selinuxtype} basalt_resolver
fi

%posttrans selinux
%selinux_relabel_post -s %{selinuxtype}

%files
%license licenses/LICENSE
%{_bindir}/basalt-resolver
%dir %{_libexecdir}/basalt-resolver
%{_libexecdir}/basalt-resolver/basalt-resolverd
%{_unitdir}/basalt-resolver.service
%{_presetdir}/80-basalt-resolver.preset
%dir %{_sysconfdir}/basalt-resolver
%config(noreplace) %{_sysconfdir}/basalt-resolver/resolver.conf

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_resolver.pp.bz2
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_resolver_ports.cil
%{_datadir}/selinux/devel/include/distributed/basalt_resolver.if

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: per-session default-deny egress (nftables chains matched
  by cgroup, sets filled from DNS answers for allowlisted names), DNS
  rebinding refusal, direct DNS and unlisted addresses dropped, decisions
  recorded in basalt-ledger, fail-closed restarts.
