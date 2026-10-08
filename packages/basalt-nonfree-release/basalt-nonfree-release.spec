# The basalt-nonfree repository definition (off by default) for Basalt OS:
# third-party drivers that may be redistributed but are not free software.
#
# Build: scripts/build-rpms.sh. BASALT_DEFAULT_NONFREE_URL and
# BASALT_DEFAULT_NONFREE_TESTING_URL, when set, replace the default URLs
# (https://obpkg.org/basalt-nonfree, https://obpkg.org/basalt-nonfree-testing)
# for a lab or a mirror; release builds refuse them.

Name:           basalt-nonfree-release
Version:        1
Release:        3%{?dist}
Summary:        Basalt OS non-free drivers repository (off by default)
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

Source0:        LICENSE
Source1:        basalt-nonfree.repo
Source2:        basalt_nonfree_url
Source3:        README
Source4:        basalt_nonfree_testing_url
Source5:        20-basalt-nonfree-signatures.repo

# The repository key and the other repositories.
Requires:       basalt-release
# Owns /usr/share/dnf5/repos.override.d (the enforced signature checks).
Requires:       libdnf5

%description
Defines the basalt-nonfree repository, off by default: drivers that are
not free software but may be redistributed, packaged for Basalt OS (today
the NVIDIA driver for Turing and newer GPUs, with kernel modules built and
signed for every Basalt OS kernel). Turn it on from Settings, Additional
drivers, or with basalt drivers, which show the license first. Also
defines basalt-nonfree-testing, off: preview builds of those drivers for
testers (Settings, Updates and channels).

%prep
%setup -q -c -T
cp -p %{sources} .

%build

%install
install -Dpm 0644 basalt-nonfree.repo %{buildroot}%{_sysconfdir}/yum.repos.d/basalt-nonfree.repo
# The signature checks do not depend on basalt-nonfree.repo, a configuration
# file an upgrade never replaces: dnf applies this vendor override after it.
install -Dpm 0644 20-basalt-nonfree-signatures.repo %{buildroot}%{_datadir}/dnf5/repos.override.d/20-basalt-nonfree-signatures.repo
install -Dpm 0644 basalt_nonfree_url %{buildroot}%{_sysconfdir}/dnf/vars/basalt_nonfree_url
install -Dpm 0644 basalt_nonfree_testing_url %{buildroot}%{_sysconfdir}/dnf/vars/basalt_nonfree_testing_url
install -d licenses && install -pm 0644 LICENSE licenses/

%files
%license licenses/LICENSE
%doc README
%config(noreplace) %{_sysconfdir}/yum.repos.d/basalt-nonfree.repo
%{_datadir}/dnf5/repos.override.d/20-basalt-nonfree-signatures.repo
%config(noreplace) %{_sysconfdir}/dnf/vars/basalt_nonfree_url
%config(noreplace) %{_sysconfdir}/dnf/vars/basalt_nonfree_testing_url

%changelog
* Wed Oct 07 2026 Basalt OS project <noreply@basalt-os.org> - 1-3
- The signature checks of basalt-nonfree, basalt-nonfree-source and
  basalt-nonfree-testing are enforced by a vendor override
  (/usr/share/dnf5/repos.override.d/20-basalt-nonfree-signatures.repo), so
  an older or edited basalt-nonfree.repo cannot weaken them.

* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 1-2
- Add basalt-nonfree-testing (off): preview builds of the non-free drivers,
  tested there before they reach basalt-nonfree.

* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 1-1
- First version: the basalt-nonfree repository, off by default.
