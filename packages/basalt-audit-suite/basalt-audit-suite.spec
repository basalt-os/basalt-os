# The public AI audit suite (github.com/basalt-os/ai-audit-suite), packaged
# so Security and Activity can run it on a person's computer ("Run audit",
# the assistant's audit.run, approved at the gate). The suite is unchanged;
# basalt-audit-suite (the wrapper) runs it as the unprivileged account
# basalt-audit with fake test data, never as root.
#
# Build: packages/basalt-audit-suite/build.sh (from the commit pinned in
# source.conf; the version is that commit's VERSION file).

%{!?suite_version: %global suite_version 0.1.0}

Name:           basalt-audit-suite
Version:        %{suite_version}
Release:        1%{?dist}
Summary:        The AI audit suite of Basalt OS, run from Security and Activity
License:        Apache-2.0
URL:            https://github.com/basalt-os/ai-audit-suite
BuildArch:      noarch

Source0:        ai-audit-suite-%{version}.tar.gz
Source1:        basalt-audit-suite
Source2:        basalt-audit-suite.sysusers
Source3:        audit.conf

BuildRequires:  systemd-rpm-macros
Requires:       bash
Requires:       coreutils
Requires:       python3
Requires:       util-linux
Requires:       systemd
Requires:       basalt-agent
%{?sysusers_requires_compat}

%description
The public, adversarial test suite for the AI security layer of Basalt OS:
it tries, like an attacker would, to get around the confinement of AI
agents, their network limits, the activity record and the confirmation
step, and reports whether each attempt was stopped. basalt-audit-suite runs
it on this computer as its own unprivileged account (basalt-audit) with fake
test data, and keeps the results in /var/lib/basalt-audit for Security and
Activity.

%prep
%setup -q -n ai-audit-suite-%{version}

%build

%install
install -d %{buildroot}%{_datadir}/basalt-audit-suite
cp -pr run-audit.sh VERSION lib cases schema agents %{buildroot}%{_datadir}/basalt-audit-suite/
find %{buildroot}%{_datadir}/basalt-audit-suite -name __pycache__ -prune -exec rm -rf {} +
# Scripts with an interpreter line are programs: make them executable
# (the suite's archive keeps some, like the mock endpoint, at 0644).
find %{buildroot}%{_datadir}/basalt-audit-suite -type f -exec sh -c 'for f; do [ "$(head -c 2 "$f")" = "#!" ] && chmod 0755 "$f"; done; true' sh {} +
install -pm 0644 %{SOURCE3} %{buildroot}%{_datadir}/basalt-audit-suite/audit.conf
install -Dpm 0755 %{SOURCE1} %{buildroot}%{_bindir}/basalt-audit-suite
install -Dpm 0644 %{SOURCE2} %{buildroot}%{_sysusersdir}/basalt-audit-suite.conf
install -d -m 0755 %{buildroot}%{_sharedstatedir}/basalt-audit

%pre
%sysusers_create_compat %{SOURCE2}

%files
%license LICENSE
%doc README.md SECURITY.md docs
%{_bindir}/basalt-audit-suite
%{_datadir}/basalt-audit-suite
%{_sysusersdir}/basalt-audit-suite.conf
%dir %{_sharedstatedir}/basalt-audit

%changelog
* Thu Oct 08 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First package: the AI audit suite at its published commit, and the
  basalt-audit-suite wrapper that runs it for Security and Activity as the
  unprivileged account basalt-audit.
