# nvidia-persistenced from its GitHub source (MIT), the same version as the
# driver: keeps the GPU initialized when no program uses it (servers,
# CUDA). Installed by nvidia-driver-compute and enabled there; desktops do
# not need it.
%global debug_package %{nil}

Name:           nvidia-persistenced
Version:        595.104.02
Release:        1%{?dist}
Summary:        NVIDIA persistence daemon
License:        MIT
URL:            https://github.com/NVIDIA/nvidia-persistenced
Source0:        https://github.com/NVIDIA/nvidia-persistenced/archive/refs/tags/%{version}.tar.gz#/nvidia-persistenced-%{version}.tar.gz
ExclusiveArch:  x86_64

BuildRequires:  gcc
BuildRequires:  make
BuildRequires:  m4
BuildRequires:  libtirpc-devel
BuildRequires:  systemd-rpm-macros
# It loads libnvidia-cfg.so.1 at run time.
Requires:       nvidia-driver-cuda-libs%{?_isa} = %{version}
%{?systemd_requires}

%description
The NVIDIA persistence daemon keeps the NVIDIA GPUs initialized while no
program uses them, so CUDA programs start without waiting for the driver
to set the GPU up again. Built from NVIDIA's open source; packaged by
OpenBasalt.

%prep
%setup -q

%build
%set_build_flags
# The build flags go in the environment: the Makefile adds its own to them.
export CFLAGS="$CFLAGS -I/usr/include/tirpc"
make %{?_smp_mflags} PREFIX=%{_prefix} NV_VERBOSE=1

%install
make install DESTDIR=%{buildroot} PREFIX=%{_prefix} NV_VERBOSE=1 INSTALL="install -p"
chmod 0644 %{buildroot}%{_mandir}/man1/*.1*
install -d %{buildroot}%{_unitdir}
sed -e 's/__USER__/nvidia-persistenced/' -e 's:/var/run/:/run/:' \
  init/systemd/nvidia-persistenced.service.template >%{buildroot}%{_unitdir}/nvidia-persistenced.service
install -d %{buildroot}%{_sysusersdir} %{buildroot}%{_presetdir}
echo 'u nvidia-persistenced - "NVIDIA persistence daemon" /run/nvidia-persistenced /usr/sbin/nologin' \
  >%{buildroot}%{_sysusersdir}/nvidia-persistenced.conf
echo 'enable nvidia-persistenced.service' >%{buildroot}%{_presetdir}/70-nvidia-persistenced.preset

%post
%systemd_post nvidia-persistenced.service

%preun
%systemd_preun nvidia-persistenced.service

%postun
%systemd_postun_with_restart nvidia-persistenced.service

%files
%license COPYING
%{_bindir}/nvidia-persistenced
%{_mandir}/man1/nvidia-persistenced.1*
%{_unitdir}/nvidia-persistenced.service
%{_sysusersdir}/nvidia-persistenced.conf
%{_presetdir}/70-nvidia-persistenced.preset

%changelog
* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 595.104.02-1
- First package, built from the GitHub source of 595.104.02.
