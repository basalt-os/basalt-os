# Basalt OS security helpers: TPM2 disk unlock (basalt-tpm) and Secure Boot
# with Basalt's own module key (basalt-secureboot).
#
# Build: scripts/build-rpms.sh. basalt-module-ca.der and
# basalt-module-signing.der in this directory are the OpenBasalt kernel module
# CA (the certificate machine owners enroll as a MOK; SHA-256 fingerprint
# 15:5E:5E:7C:FE:7C:19:4C:9C:AD:E7:4F:32:0A:FE:11:57:D7:F8:B2:68:0A:93:A2:25:2F:7D:FA:DC:36:C2:DF)
# and the module signing certificate it issued (79:DF:9F:05:C4:96:30:57:5F:59:1D:C4:2D:FA:28:8D:CF:AC:E0:A5:C6:99:4A:9B:BA:06:B8:BB:D0:E9:96:FE).
# Public material only; the private keys stay offline. Lab and CI builds may
# replace them with their own through BASALT_MODULE_CA_CERT and
# BASALT_MODULE_SIGNING_CERT (an explicit override).

Name:           basalt-security
Version:        0.1.0
Release:        3%{?dist}
Summary:        TPM2 unlock and Secure Boot helpers for Basalt OS
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

Source0:        LICENSE
Source1:        basalt-tpm
Source2:        basalt-secureboot
Source3:        basalt-tpm-resume.service
Source4:        basalt-module-keys.service
Source5:        tpm.conf
Source6:        basalt-module-ca.der
Source7:        basalt-module-signing.der
Source8:        README

BuildRequires:  systemd-rpm-macros
Requires:       systemd
Requires:       systemd-udev
Requires:       cryptsetup
Requires:       mokutil
Requires:       keyutils
Requires:       openssl
Requires:       util-linux
Requires:       gawk
%{?systemd_requires}

%description
basalt-tpm shows the state of the TPM2 unlock of the encrypted root
(key slots, PCR policy, whether the TPM would unlock it now), re-enrolls
the TPM2 key after a change of the boot chain, and can suspend the TPM
policy for one boot before a planned change, re-sealing on that boot.

basalt-secureboot shows the Secure Boot state, enrolls the Basalt kernel
module CA as a MOK through shim, and loads Basalt's module signing
certificates into the kernel at boot, so that kernel modules built and
signed by Basalt OS load with Secure Boot and lockdown on.

%prep
%setup -q -c -T
cp -p %{sources} .

%build

%install
install -Dpm 0755 basalt-tpm %{buildroot}%{_bindir}/basalt-tpm
install -Dpm 0755 basalt-secureboot %{buildroot}%{_bindir}/basalt-secureboot
install -Dpm 0644 basalt-tpm-resume.service %{buildroot}%{_unitdir}/basalt-tpm-resume.service
install -Dpm 0644 basalt-module-keys.service %{buildroot}%{_unitdir}/basalt-module-keys.service
install -Dpm 0644 tpm.conf %{buildroot}%{_sysconfdir}/basalt/tpm.conf
install -Dpm 0644 basalt-module-ca.der %{buildroot}%{_datadir}/basalt/secureboot/basalt-module-ca.der
install -Dpm 0644 basalt-module-signing.der %{buildroot}%{_prefix}/lib/basalt/module-keys/basalt-module-signing.der
install -d licenses && install -pm 0644 LICENSE licenses/

%post
%systemd_post basalt-module-keys.service basalt-tpm-resume.service

%preun
%systemd_preun basalt-module-keys.service basalt-tpm-resume.service

%postun
%systemd_postun basalt-module-keys.service basalt-tpm-resume.service

%files
%license licenses/LICENSE
%doc README
%{_bindir}/basalt-tpm
%{_bindir}/basalt-secureboot
%{_unitdir}/basalt-tpm-resume.service
%{_unitdir}/basalt-module-keys.service
%dir %{_sysconfdir}/basalt
%config(noreplace) %{_sysconfdir}/basalt/tpm.conf
%dir %{_datadir}/basalt
%dir %{_datadir}/basalt/secureboot
%{_datadir}/basalt/secureboot/basalt-module-ca.der
%dir %{_prefix}/lib/basalt
%dir %{_prefix}/lib/basalt/module-keys
%{_prefix}/lib/basalt/module-keys/basalt-module-signing.der

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-3
- Ship the OpenBasalt kernel module CA and module signing certificate
  instead of placeholders.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-2
- basalt-secureboot: read the message of mokutil --test-key (it exits 1 for
  an enrolled key too), so an enrolled module CA is reported as enrolled.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: basalt-tpm (status, pcrs, reenroll, suspend, resume),
  basalt-secureboot (status, enroll-mok, unenroll-mok, load-module-keys).
