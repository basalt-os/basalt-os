# nvidia-modprobe from its GitHub source (GPL-2.0-only), the same version
# as the driver: loads the NVIDIA kernel modules and creates the /dev/nvidia*
# device files for programs that are not root (CUDA). Built from source
# instead of taking the binary in NVIDIA's .run.
%global debug_package %{nil}

Name:           nvidia-modprobe
Version:        595.104.02
Release:        1%{?dist}
Summary:        Load the NVIDIA kernel modules and create the NVIDIA device files
License:        GPL-2.0-only
URL:            https://github.com/NVIDIA/nvidia-modprobe
Source0:        https://github.com/NVIDIA/nvidia-modprobe/archive/refs/tags/%{version}.tar.gz#/nvidia-modprobe-%{version}.tar.gz
ExclusiveArch:  x86_64

BuildRequires:  gcc
BuildRequires:  make
BuildRequires:  m4

%description
nvidia-modprobe is a setuid root helper that loads the NVIDIA kernel
modules (for example nvidia-uvm, which CUDA needs) and creates the
/dev/nvidia* device files, so that programs run by ordinary users can use
the GPU. Built from NVIDIA's open source; packaged by OpenBasalt.

%prep
%setup -q

%build
%set_build_flags
# The build flags go in the environment: the Makefile adds its own to them.
make %{?_smp_mflags} PREFIX=%{_prefix} NV_VERBOSE=1

%install
make install DESTDIR=%{buildroot} PREFIX=%{_prefix} NV_VERBOSE=1 INSTALL="install -p"
chmod 0644 %{buildroot}%{_mandir}/man1/*.1*

%files
%license COPYING
%attr(4755, root, root) %{_bindir}/nvidia-modprobe
%{_mandir}/man1/nvidia-modprobe.1*

%changelog
* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 595.104.02-1
- First package, built from the GitHub source of 595.104.02.
