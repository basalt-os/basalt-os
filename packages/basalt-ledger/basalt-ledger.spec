# basalt-ledger: the append-only system audit service. Producers append
# records over a Unix socket (SELinux and SO_PEERCRED decide who may write
# what); the chain is hash-linked with sealed rotation; users read their
# own records in plain English, administrators everything; signed exports.
# Static Go binary, the SELinux policy in the -selinux subpackage.

%global selinuxtype targeted
%global debug_package %{nil}

Name:           basalt-ledger
Version:        0.1.0
Release:        1%{?dist}
Summary:        Append-only, hash-chained system audit service
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.25
BuildRequires:  systemd-rpm-macros
BuildRequires:  selinux-policy-devel
BuildRequires:  make
BuildRequires:  bzip2

Requires:       polkit
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})
%{?systemd_requires}

%description
basalt-ledger collects the security-relevant events of a Basalt OS
system in one append-only, hash-chained trail: AI agent sessions and their
network decisions (basalt-agent, basalt-resolver), the system assistant's
proposals and applies, SELinux denials, polkit authentications, pkexec and
sudo escalations, failed authentications, logins and snapshot rollbacks.
Producers can only append; nobody can change history through the service,
and edits to the files are detected. Users read their own records in plain
English (basalt-ledger, basalt-ledger summary) with filters by agent,
project, app, severity and time; administrators read everything; signed
exports serve incident reports. A JSON API on the same socket serves the
desktop shell's timeline.

%package selinux
Summary:        SELinux policy for basalt-ledger
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
Requires(post): policycoreutils
%{?selinux_requires}

%description selinux
The SELinux policy for basalt-ledger: the service domain basalt_ledger_t,
its log and runtime types, and the interface producers use to connect.

%prep
%setup -q -c

%build
export GOFLAGS="-mod=mod -trimpath" GOTOOLCHAIN=local GOPROXY=off
for role in daemon cli; do
    go build -buildmode=pie \
        -ldflags "-B gobuildid -X main.version=%{version}-%{release} -X main.roleBuild=$role" \
        -o bin/basalt-ledger-$role ./cmd/basalt-ledger
done
make -C selinux -f %{_datadir}/selinux/devel/Makefile basalt_ledger.pp
bzip2 -9 selinux/basalt_ledger.pp

%check
export GOFLAGS="-mod=mod" GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
install -Dpm 0755 bin/basalt-ledger-daemon %{buildroot}%{_libexecdir}/basalt-ledger/basalt-ledgerd
install -Dpm 0755 bin/basalt-ledger-cli %{buildroot}%{_bindir}/basalt-ledger
install -Dpm 0644 dist/basalt-ledger.service %{buildroot}%{_unitdir}/basalt-ledger.service
install -Dpm 0644 dist/80-basalt-ledger.preset %{buildroot}%{_presetdir}/80-basalt-ledger.preset
install -Dpm 0644 dist/ledger.conf %{buildroot}%{_sysconfdir}/basalt-ledger/ledger.conf
install -Dpm 0644 dist/org.basalt-os.ledger.policy %{buildroot}%{_datadir}/polkit-1/actions/org.basalt-os.ledger.policy
install -d -m 0755 %{buildroot}%{_localstatedir}/log/basalt-ledger
install -Dpm 0644 selinux/basalt_ledger.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_ledger.pp.bz2
install -Dpm 0644 selinux/basalt_ledger.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/basalt_ledger.if
install -d licenses && install -pm 0644 LICENSE licenses/

%post
%systemd_post basalt-ledger.service

%preun
%systemd_preun basalt-ledger.service

%postun
%systemd_postun_with_restart basalt-ledger.service

%pre selinux
%selinux_relabel_pre -s %{selinuxtype}

%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/basalt_ledger.pp.bz2

%postun selinux
if [ $1 -eq 0 ]; then
    %selinux_modules_uninstall -s %{selinuxtype} basalt_ledger
fi

%posttrans selinux
%selinux_relabel_post -s %{selinuxtype}

%files
%license licenses/LICENSE
%{_bindir}/basalt-ledger
%dir %{_libexecdir}/basalt-ledger
%{_libexecdir}/basalt-ledger/basalt-ledgerd
%{_unitdir}/basalt-ledger.service
%{_presetdir}/80-basalt-ledger.preset
%dir %{_sysconfdir}/basalt-ledger
%config(noreplace) %{_sysconfdir}/basalt-ledger/ledger.conf
%{_datadir}/polkit-1/actions/org.basalt-os.ledger.policy
# The chain is never removed with the package.
%dir %{_localstatedir}/log/basalt-ledger

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_ledger.pp.bz2
%{_datadir}/selinux/devel/include/distributed/basalt_ledger.if

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: socket producers with SELinux and SO_PEERCRED rules,
  hash chain with sealed rotation, journal and snapper collectors, plain
  English views and summaries, filters, signed exports, JSON API.
