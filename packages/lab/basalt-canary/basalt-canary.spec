# Lab-only test package for the snapshot and rollback tests. Never published
# outside a lab repository.
#
# One spec, three builds (rpmbuild --define 'canary_version N'):
#   1  healthy: /usr/bin/basalt-canary prints "ok" and exits 0
#   2  broken update: the command fails and its configuration is rewritten
#   3  broken boot: adds an invalid sshd drop-in, so SSH does not come back
#      after the next reboot (recovery goes through the GRUB snapshot menu)

%{!?canary_version: %global canary_version 1}

Name:           basalt-canary
Version:        %{canary_version}.0
Release:        1%{?dist}
Summary:        Basalt OS lab canary package (version %{canary_version})
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

%description
Test fixture for Basalt OS update and rollback tests. Version 1 is healthy,
version 2 is a deliberately broken update, version 3 breaks SSH at the next
boot. Do not install it on a real system.

%prep

%build
cat >basalt-canary <<'EOF'
#!/usr/bin/sh
# Basalt OS lab canary, version %{canary_version}.
. /etc/basalt-canary.conf
if [ "$STATE" = healthy ]; then
  echo "basalt-canary %{version}: ok"
  exit 0
fi
echo "basalt-canary %{version}: BROKEN ($STATE)" >&2
exit 1
EOF
%if %{canary_version} == 1
echo 'STATE=healthy' >basalt-canary.conf
%else
echo 'STATE=broken-by-update-%{canary_version}' >basalt-canary.conf
%endif
%if %{canary_version} == 3
cat >01-basalt-canary-broken.conf <<'EOF'
# Deliberately invalid: sshd refuses to start with this file present.
ThisIsNotAnSshdKeyword yes
EOF
%endif

%install
install -Dpm 0755 basalt-canary %{buildroot}%{_bindir}/basalt-canary
install -Dpm 0644 basalt-canary.conf %{buildroot}%{_sysconfdir}/basalt-canary.conf
%if %{canary_version} == 3
install -Dpm 0600 01-basalt-canary-broken.conf %{buildroot}%{_sysconfdir}/ssh/sshd_config.d/01-basalt-canary-broken.conf
%endif

%files
%{_bindir}/basalt-canary
%config %{_sysconfdir}/basalt-canary.conf
%if %{canary_version} == 3
%config %{_sysconfdir}/ssh/sshd_config.d/01-basalt-canary-broken.conf
%endif

%changelog
* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - %{canary_version}.0-1
- Lab fixture.
