# NVIDIA open GPU kernel modules (MIT/GPLv2), built for one Basalt OS
# kernel (a Fedora kernel) and signed with the basalt-nonfree module key:
# kmod-nvidia-open-<kernel>. No akmods or DKMS: the module is prebuilt and
# signed, so it loads with Secure Boot and lockdown on, without a
# per-machine key.
#
#   rpmbuild --define "kver 7.2.8-200.fc44.x86_64" [--define "signatures 1"] -ba nvidia-open-kmod.spec
#
# Signing happens on the release signer, away from this build, and only
# produces signatures (scripts/release/sign-modules.sh). The build runs
# twice from the same source in the same build image: the first time
# without signatures (scripts/release/build-nonfree.sh modules: the
# unsigned modules go to the signer), the second time with them (Source1):
# %%install checks that each module it built is byte-identical to the one
# that was signed, then appends the signature. A build that is not
# identical fails. Anyone can rebuild this source RPM and get the same
# signed modules.

# kver: the kernel release (uname -r) to build for. "unset" only lets
# tools such as rpmlint read the spec; %%prep refuses it.
%{!?kver:%global kver unset}
%global moddir /usr/lib/modules/%{kver}/extra/nvidia-open
%global modules nvidia nvidia-drm nvidia-modeset nvidia-uvm nvidia-peermem
# Prebuilt, signed modules: nothing may touch them after signing.
%global debug_package %{nil}
%global __os_install_post %{nil}
%global _build_id_links none
# Kernel code: the kernel's own flags, never the userspace build flags
# (NVIDIA's Makefile passes LDFLAGS to ld).
%undefine _auto_set_build_flags

Name:           kmod-nvidia-open-%{kver}
Version:        595.104.02
Release:        1%{?dist}
Summary:        NVIDIA open GPU kernel modules for kernel %{kver}, signed
License:        MIT AND GPL-2.0-only
URL:            https://github.com/NVIDIA/open-gpu-kernel-modules
Source0:        https://github.com/NVIDIA/open-gpu-kernel-modules/archive/refs/tags/%{version}.tar.gz#/open-gpu-kernel-modules-%{version}.tar.gz
%if 0%{?signatures}
# Signatures from scripts/release/sign-modules.sh: <module>.ko.sig (the
# bytes the kernel's sign-file appends) and SIGNATURES (SHA-256 of each
# unsigned module and of its signature).
Source1:        nvidia-open-signatures-%{version}-%{kver}.tar
%endif
ExclusiveArch:  x86_64

BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  make
BuildRequires:  binutils
BuildRequires:  elfutils-libelf-devel
BuildRequires:  kernel-devel-uname-r = %{kver}
BuildRequires:  coreutils

Requires:       kernel-uname-r = %{kver}
Requires:       nvidia-driver-firmware = %{version}
Requires:       basalt-nvidia
Requires(post): kmod
Requires(postun): kmod
Provides:       nvidia-kmod(open) = %{version}
Provides:       kmod-nvidia-open-kernel = %{kver}
Conflicts:      akmod-nvidia, kmod-nvidia, kmod-nvidia-open-dkms, kmod-nvidia-latest-dkms

%description
NVIDIA's open GPU kernel modules %{version} (nvidia, nvidia-drm,
nvidia-modeset, nvidia-uvm, nvidia-peermem), built from the source of
github.com/NVIDIA/open-gpu-kernel-modules for the kernel
%{kver},
for GPUs of the Turing generation and newer. The modules are signed with
the Basalt OS basalt-nonfree module key, whose certificate basalt-nvidia
ships and the Basalt kernel module CA issued. The full corresponding
source is this package's source RPM. Packaged by OpenBasalt, not
supported by NVIDIA.

%prep
[ "%{kver}" != unset ] || { echo "define kver (rpmbuild --define 'kver RELEASE')" >&2; exit 1; }
%setup -q -n open-gpu-kernel-modules-%{version}
%if 0%{?signatures}
mkdir -p ../sigs && tar -C ../sigs -xf %{SOURCE1}
%endif

%build
# Fixed build identity, so the second build is byte-identical to the
# first: the version string embeds the date, user and host.
epoch=${SOURCE_DATE_EPOCH:-1789689600}
unset CFLAGS CXXFLAGS LDFLAGS LT_SYS_LIBRARY_PATH
make %{?_smp_mflags} modules SYSSRC=/usr/src/kernels/%{kver} \
  DATE="date -u -d @$epoch" NV_BUILD_USER=basalt NV_BUILD_HOST=basalt-os-build \
  IGNORE_CC_MISMATCH=1 NV_VERBOSE=0

%install
install -d %{buildroot}%{moddir}
for m in %{modules}; do
  install -p -m 0644 kernel-open/$m.ko %{buildroot}%{moddir}/$m.ko
  strip --strip-debug %{buildroot}%{moddir}/$m.ko
done
%if 0%{?signatures}
for m in %{modules}; do
  f=%{buildroot}%{moddir}/$m.ko
  want=$(awk -v m="$m.ko" '$1 == "module" && $2 == m {print $3}' ../sigs/SIGNATURES)
  sigsum=$(awk -v m="$m.ko" '$1 == "module" && $2 == m {print $4}' ../sigs/SIGNATURES)
  got=$(sha256sum "$f" | cut -d' ' -f1)
  [ -n "$want" ] || { echo "no signature for $m.ko" >&2; exit 1; }
  [ "$got" = "$want" ] || { echo "$m.ko is not the module that was signed (built $got, signed $want): the build is not reproducible" >&2; exit 1; }
  echo "$sigsum  ../sigs/$m.ko.sig" | sha256sum -c --quiet - || { echo "signature file of $m.ko is damaged" >&2; exit 1; }
  tail -c 28 ../sigs/$m.ko.sig | grep -q '~Module signature appended~' || { echo "$m.ko.sig is not a module signature" >&2; exit 1; }
  cat ../sigs/$m.ko.sig >>"$f"
done
%endif
cp -p COPYING COPYING.open-gpu-kernel-modules

%post
/usr/sbin/depmod -a %{kver} >/dev/null 2>&1 || :

%postun
[ -e /usr/lib/modules/%{kver}/modules.dep ] && /usr/sbin/depmod -a %{kver} >/dev/null 2>&1 || :

%posttrans
# The kernel this module is for may now become the default boot entry.
[ -x /usr/bin/basalt-nvidia ] && /usr/bin/basalt-nvidia select-default >/dev/null 2>&1 || :

%files
%license COPYING.open-gpu-kernel-modules
%dir /usr/lib/modules/%{kver}/extra
%dir %{moddir}
%{moddir}/*.ko

%changelog
* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 595.104.02-1
- First package: open GPU kernel modules 595.104.02, unmodified source,
  built per kernel and signed with the basalt-nonfree module key.
