# Basalt OS configuration for the NVIDIA driver (basalt-nonfree): nouveau
# off, the boot-time choice between the NVIDIA driver and nouveau, the check
# after boot with the fallback, the kernel update guard, PRIME offload, and
# the certificate that signs the NVIDIA kernel modules. Our own files only
# (Apache-2.0); NVIDIA's files are in nvidia-driver-*.
#
# Build: scripts/release/build-nonfree.sh. The module signing certificate
# (Source10) comes from scripts/release/sign-modules.sh; without it
# (--define "nocert 1", unsigned test builds) none is shipped.

%global other_nvidia xorg-x11-drv-nvidia, xorg-x11-drv-nvidia-kmodsrc, akmod-nvidia, kmod-nvidia, nvidia-kmod-common, kmod-nvidia-open-dkms, kmod-nvidia-latest-dkms

Name:           basalt-nvidia
Version:        0.1.0
Release:        1%{?dist}
Summary:        Basalt OS configuration for the NVIDIA driver: nouveau off, boot check, fallback
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

Source0:        LICENSE
Source1:        basalt-nvidia
Source2:        basalt-nvidia.conf
Source3:        90-basalt-nvidia.conf
Source4:        99-zz-basalt-nvidia.install
Source5:        basalt-nvidia-boot.service
Source6:        basalt-nvidia-check.service
Source7:        70-basalt-nvidia.preset
Source8:        basalt-nvidia-run
Source9:        50-basalt-niri.json
%if 0%{!?nocert:1}
Source10:       basalt-nonfree-module-signing.der
%endif

BuildRequires:  systemd-rpm-macros
Requires:       bash
Requires:       kmod
Requires:       dracut
Requires:       grubby
Requires:       systemd
Requires:       coreutils
Requires:       sed
Requires:       grep
Requires:       findutils
# basalt-module-keys.service loads /usr/lib/basalt/module-keys/*.der into
# the kernel at boot; the Basalt module CA must be enrolled (MOK).
Requires:       basalt-security >= 0.1.0-3
Conflicts:      %{other_nvidia}
%{?systemd_requires}

%description
Basalt OS configuration for the NVIDIA driver from the basalt-nonfree
repository. It turns nouveau off (modprobe.d and dracut), chooses at each
boot, before udev, between the NVIDIA driver and nouveau, checks the
driver after the login screen starts and falls back to nouveau when a first
start fails, keeps the default boot entry on a kernel that has its NVIDIA
module, runs programs on the NVIDIA GPU of laptops with two GPUs
(basalt-nvidia-run), and ships the certificate the NVIDIA kernel modules
are signed with. Every step is recorded in the Basalt audit ledger.

%prep
%setup -q -c -T
cp -p %{sources} .

%build

%install
install -Dpm 0755 basalt-nvidia %{buildroot}%{_bindir}/basalt-nvidia
install -Dpm 0755 basalt-nvidia-run %{buildroot}%{_bindir}/basalt-nvidia-run
install -Dpm 0644 basalt-nvidia.conf %{buildroot}%{_prefix}/lib/modprobe.d/basalt-nvidia.conf
install -Dpm 0644 90-basalt-nvidia.conf %{buildroot}%{_prefix}/lib/dracut/dracut.conf.d/90-basalt-nvidia.conf
install -Dpm 0755 99-zz-basalt-nvidia.install %{buildroot}%{_prefix}/lib/kernel/install.d/99-zz-basalt-nvidia.install
install -Dpm 0644 basalt-nvidia-boot.service %{buildroot}%{_unitdir}/basalt-nvidia-boot.service
install -Dpm 0644 basalt-nvidia-check.service %{buildroot}%{_unitdir}/basalt-nvidia-check.service
install -Dpm 0644 70-basalt-nvidia.preset %{buildroot}%{_presetdir}/70-basalt-nvidia.preset
install -Dpm 0644 50-basalt-niri.json %{buildroot}%{_sysconfdir}/nvidia/nvidia-application-profiles-rc.d/50-basalt-niri.json
install -d -m 0755 %{buildroot}%{_sharedstatedir}/basalt-nvidia
%if 0%{!?nocert:1}
install -Dpm 0644 basalt-nonfree-module-signing.der %{buildroot}%{_prefix}/lib/basalt/module-keys/basalt-nonfree-module-signing.der
%endif
install -d licenses && install -pm 0644 LICENSE licenses/

%post
%systemd_post basalt-nvidia-boot.service basalt-nvidia-check.service
if [ "$1" -eq 1 ] && [ -x /usr/bin/basalt-secureboot ]; then
  # Load the new signing certificate now, so a module can load before the
  # next boot (the boot service does it on every later boot).
  /usr/bin/basalt-secureboot load-module-keys >/dev/null 2>&1 || :
fi

%preun
%systemd_preun basalt-nvidia-boot.service basalt-nvidia-check.service

%postun
%systemd_postun basalt-nvidia-boot.service basalt-nvidia-check.service
if [ "$1" -eq 0 ]; then
  # Removed: drop our fallback file (nouveau is the default again anyway)
  # and rebuild the initramfs without our dracut setting.
  if [ -f /etc/modprobe.d/basalt-nvidia.conf ] && head -1 /etc/modprobe.d/basalt-nvidia.conf | grep -qF '# basalt-nvidia fallback'; then
    rm -f /etc/modprobe.d/basalt-nvidia.conf
  fi
  rm -f %{_sharedstatedir}/basalt-nvidia/initramfs.sum
  [ -x /usr/bin/dracut ] && dracut -f --regenerate-all >/dev/null 2>&1 || :
fi

%posttrans
# nouveau leaves the initramfs of every installed kernel: rebuild them when
# our modprobe.d or dracut settings are new or changed.
sum=$(cat %{_prefix}/lib/modprobe.d/basalt-nvidia.conf %{_prefix}/lib/dracut/dracut.conf.d/90-basalt-nvidia.conf | sha256sum | cut -d' ' -f1)
if [ "$(cat %{_sharedstatedir}/basalt-nvidia/initramfs.sum 2>/dev/null)" != "$sum" ] && [ -x /usr/bin/dracut ]; then
  dracut -f --regenerate-all >/dev/null 2>&1 && echo "$sum" >%{_sharedstatedir}/basalt-nvidia/initramfs.sum || :
fi
/usr/bin/basalt-nvidia select-default >/dev/null 2>&1 || :

%files
%license licenses/LICENSE
%{_bindir}/basalt-nvidia
%{_bindir}/basalt-nvidia-run
%{_prefix}/lib/modprobe.d/basalt-nvidia.conf
%{_prefix}/lib/dracut/dracut.conf.d/90-basalt-nvidia.conf
%{_prefix}/lib/kernel/install.d/99-zz-basalt-nvidia.install
%{_unitdir}/basalt-nvidia-boot.service
%{_unitdir}/basalt-nvidia-check.service
%{_presetdir}/70-basalt-nvidia.preset
%dir %{_sysconfdir}/nvidia
%dir %{_sysconfdir}/nvidia/nvidia-application-profiles-rc.d
%config(noreplace) %{_sysconfdir}/nvidia/nvidia-application-profiles-rc.d/50-basalt-niri.json
%dir %{_sharedstatedir}/basalt-nvidia
%if 0%{!?nocert:1}
%dir %{_prefix}/lib/basalt
%dir %{_prefix}/lib/basalt/module-keys
%{_prefix}/lib/basalt/module-keys/basalt-nonfree-module-signing.der
%endif

%changelog
* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: nouveau off, boot-time choice and check with fallback to
  nouveau, kernel update guard, PRIME offload, niri application profile,
  basalt-nonfree module signing certificate.
