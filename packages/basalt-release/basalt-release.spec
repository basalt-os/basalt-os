# Basalt OS release identity, repository definition and default services.
#
# Replaces fedora-release the same way generic-release does: it provides
# system-release and conflicts with the other providers. Built once per
# Fedora release (Version = the Fedora release it runs on, like
# fedora-release), so `dnf system-upgrade` moves it along with the base.
#
# Build: scripts/build-rpms.sh (in a Fedora container). The repository
# signing key is a build input: BASALT_GPG_PUBKEY replaces the placeholder
# RPM-GPG-KEY-basalt shipped in this directory.

%global dist_version %{fedora}
%{!?basalt_version:%global basalt_version 0.0.1}
%global basalt_codename pre-alpha

Name:           basalt-release
Version:        %{dist_version}
Release:        2%{?dist}
Summary:        Basalt OS release files
# Apache-2.0: Basalt OS files. MIT: systemd preset files taken from fedora-release.
License:        Apache-2.0 AND MIT
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

Source0:        LICENSE
Source1:        LICENSE.presets
Source2:        basalt.repo
Source3:        RPM-GPG-KEY-basalt
Source4:        basalt_repo_url
Source5:        20-basalt-defaults.conf
Source10:       80-basalt.preset
Source11:       85-display-manager.preset
Source12:       90-default.preset
Source13:       99-default-disable.preset
Source14:       90-default-user.preset
Source20:       10-basalt-hardening.conf
Source21:       basalt.xml
Source22:       firewalld-basalt.conf
Source23:       10-basalt-resolved.conf

Provides:       basalt-release-identity = %{version}-%{release}
Provides:       system-release
Provides:       system-release(%{version})
# dnf derives $releasever from this, so Fedora repositories keep working.
Provides:       system-release(releasever) = %{dist_version}
Conflicts:      system-release
Conflicts:      fedora-release
Conflicts:      fedora-release-common
Conflicts:      fedora-release-identity
Conflicts:      generic-release
Conflicts:      generic-release-common
# Fedora's repository definitions and package signing keys stay in use.
Requires:       fedora-repos(%{dist_version})

%description
Basalt OS release files: /usr/lib/os-release and the other files that
identify the system, the Basalt OS package repository with its signing
key, the dnf defaults and the systemd presets that decide which services
start by default. Basalt OS is based on Fedora and is not affiliated with
the Fedora Project.

%package server
Summary:        Basalt OS server defaults
Requires:       %{name} = %{version}-%{release}
Requires:       firewalld
Requires:       openssh-server
Requires:       audit
Requires:       selinux-policy-targeted
Requires:       policycoreutils
Requires(posttrans): coreutils

%description server
Server defaults for Basalt OS: SSH with public keys only, a firewalld zone
that only lets SSH in (made the default zone on first install), LLMNR and
multicast DNS off in systemd-resolved. SELinux is expected to be enforcing;
installing this package warns when it is not.

%prep
%setup -q -c -T
cp -p %{sources} .

%build

%install
# --- identity ---------------------------------------------------------------
install -d %{buildroot}%{_prefix}/lib %{buildroot}%{_sysconfdir}
cat >%{buildroot}%{_prefix}/lib/os-release <<EOF
NAME="Basalt OS"
VERSION="%{basalt_version} (%{basalt_codename})"
RELEASE_TYPE=development
ID=basalt
ID_LIKE=fedora
VERSION_ID=%{basalt_version}
PRETTY_NAME="Basalt OS %{basalt_version} (%{basalt_codename})"
VARIANT="Server"
VARIANT_ID=server
ANSI_COLOR="0;38;2;163;71;46"
LOGO=basalt-logo-icon
CPE_NAME="cpe:/o:basalt-os:basalt-os:%{basalt_version}"
DEFAULT_HOSTNAME="basalt"
HOME_URL="https://basalt-os.org/"
BUG_REPORT_URL="https://github.com/basalt-os/basalt-os/issues"
EOF
ln -s ../usr/lib/os-release %{buildroot}%{_sysconfdir}/os-release

# Several tools read these names; keep them, with Basalt content.
echo "Basalt OS release %{basalt_version} (%{basalt_codename}, based on Fedora %{dist_version})" \
  >%{buildroot}%{_prefix}/lib/fedora-release
echo "cpe:/o:basalt-os:basalt-os:%{basalt_version}" >%{buildroot}%{_prefix}/lib/system-release-cpe
ln -s ../usr/lib/fedora-release %{buildroot}%{_sysconfdir}/fedora-release
ln -s ../usr/lib/system-release-cpe %{buildroot}%{_sysconfdir}/system-release-cpe
ln -s fedora-release %{buildroot}%{_sysconfdir}/redhat-release
ln -s fedora-release %{buildroot}%{_sysconfdir}/system-release

printf '\\S\nKernel \\r on \\m (\\l)\n\n' >%{buildroot}%{_prefix}/lib/issue
printf '\\S\nKernel \\r on \\m (\\l)\n' >%{buildroot}%{_prefix}/lib/issue.net
ln -s ../usr/lib/issue %{buildroot}%{_sysconfdir}/issue
ln -s ../usr/lib/issue.net %{buildroot}%{_sysconfdir}/issue.net
install -d %{buildroot}%{_sysconfdir}/issue.d

# rpm dist macros: packages are still built "for Fedora N".
install -d %{buildroot}%{_rpmconfigdir}/macros.d
cat >%{buildroot}%{_rpmconfigdir}/macros.d/macros.dist <<EOF
# dist macros.
%%fedora              %{dist_version}
%%fc%{dist_version}                1
%%distcore            .fc%%{fedora}
%%dist                %%{?distprefix}%%{distcore}%%{?with_bootstrap:~bootstrap}
%%basalt              %{basalt_version}
%%dist_vendor         Basalt OS
%%dist_name           Basalt OS
%%dist_home_url       https://basalt-os.org/
%%dist_bug_report_url https://github.com/basalt-os/basalt-os/issues
EOF

# --- repository ---------------------------------------------------------------
install -Dpm 0644 basalt.repo %{buildroot}%{_sysconfdir}/yum.repos.d/basalt.repo
install -Dpm 0644 RPM-GPG-KEY-basalt %{buildroot}%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-basalt
install -Dpm 0644 basalt_repo_url %{buildroot}%{_sysconfdir}/dnf/vars/basalt_repo_url
install -Dpm 0644 20-basalt-defaults.conf %{buildroot}%{_datadir}/dnf5/libdnf.conf.d/20-basalt-defaults.conf

# --- presets --------------------------------------------------------------------
install -d %{buildroot}%{_prefix}/lib/systemd/system-preset %{buildroot}%{_prefix}/lib/systemd/user-preset
install -pm 0644 80-basalt.preset 85-display-manager.preset 90-default.preset 99-default-disable.preset \
  %{buildroot}%{_prefix}/lib/systemd/system-preset/
install -pm 0644 90-default-user.preset %{buildroot}%{_prefix}/lib/systemd/user-preset/
# The user-level "disable everything else" rule, as in fedora-release.
install -pm 0644 99-default-disable.preset %{buildroot}%{_prefix}/lib/systemd/user-preset/

# --- server defaults ------------------------------------------------------------
install -Dpm 0600 10-basalt-hardening.conf %{buildroot}%{_sysconfdir}/ssh/sshd_config.d/10-basalt-hardening.conf
install -Dpm 0644 basalt.xml %{buildroot}%{_prefix}/lib/firewalld/zones/basalt.xml
install -Dpm 0644 firewalld-basalt.conf %{buildroot}%{_sysconfdir}/firewalld/firewalld-basalt.conf
install -Dpm 0644 10-basalt-resolved.conf %{buildroot}%{_prefix}/lib/systemd/resolved.conf.d/10-basalt.conf

install -d licenses
install -pm 0644 LICENSE LICENSE.presets licenses/

%posttrans server
# Make the "basalt" zone the default on first install. firewalld chooses
# its configuration in its own %%posttrans (a symlink to one of its
# firewalld-*.conf files, picked from VARIANT_ID); a missing file or one of
# those stock symlinks means nobody customized it yet.
conf=%{_sysconfdir}/firewalld/firewalld.conf
target=$(readlink "$conf" 2>/dev/null || :)
if [ ! -e "$conf" ] || case "$target" in firewalld-server.conf|firewalld-standard.conf|firewalld-workstation.conf) true ;; *) false ;; esac; then
    ln -sf firewalld-basalt.conf "$conf" || :
    if [ -d /run/systemd/system ] && systemctl -q is-active firewalld.service 2>/dev/null; then
        firewall-cmd --reload >/dev/null 2>&1 || :
    fi
fi
# SELinux must be enforcing on Basalt OS. Warn, do not change it silently.
if [ -r %{_sysconfdir}/selinux/config ] && ! grep -qE '^SELINUX=enforcing' %{_sysconfdir}/selinux/config; then
    echo "basalt-release-server: warning: SELinux is not set to enforcing in /etc/selinux/config" >&2
fi
:

%files
%license licenses/LICENSE licenses/LICENSE.presets
%{_prefix}/lib/os-release
%{_sysconfdir}/os-release
%{_prefix}/lib/fedora-release
%{_prefix}/lib/system-release-cpe
%{_sysconfdir}/fedora-release
%{_sysconfdir}/redhat-release
%{_sysconfdir}/system-release
%{_sysconfdir}/system-release-cpe
%attr(0644,root,root) %{_prefix}/lib/issue
%config(noreplace) %{_sysconfdir}/issue
%attr(0644,root,root) %{_prefix}/lib/issue.net
%config(noreplace) %{_sysconfdir}/issue.net
%dir %{_sysconfdir}/issue.d
%attr(0644,root,root) %{_rpmconfigdir}/macros.d/macros.dist
%config(noreplace) %{_sysconfdir}/yum.repos.d/basalt.repo
%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-basalt
%config(noreplace) %{_sysconfdir}/dnf/vars/basalt_repo_url
%dir %{_datadir}/dnf5
%dir %{_datadir}/dnf5/libdnf.conf.d
%{_datadir}/dnf5/libdnf.conf.d/20-basalt-defaults.conf
%dir %{_prefix}/lib/systemd/system-preset
%dir %{_prefix}/lib/systemd/user-preset
%{_prefix}/lib/systemd/system-preset/80-basalt.preset
%{_prefix}/lib/systemd/system-preset/85-display-manager.preset
%{_prefix}/lib/systemd/system-preset/90-default.preset
%{_prefix}/lib/systemd/system-preset/99-default-disable.preset
%{_prefix}/lib/systemd/user-preset/90-default-user.preset
%{_prefix}/lib/systemd/user-preset/99-default-disable.preset

%files server
%config(noreplace) %{_sysconfdir}/ssh/sshd_config.d/10-basalt-hardening.conf
%{_prefix}/lib/firewalld/zones/basalt.xml
%config(noreplace) %{_sysconfdir}/firewalld/firewalld-basalt.conf
%dir %{_prefix}/lib/systemd/resolved.conf.d
%{_prefix}/lib/systemd/resolved.conf.d/10-basalt.conf

%changelog
* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 44-2
- Preset: enable basalt-initial-snapshot.service.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 44-1
- Basalt OS 0.0.1 (pre-alpha): identity, repository, presets, server defaults.
