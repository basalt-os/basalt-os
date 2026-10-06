# NVIDIA driver userspace for Basalt OS, in the opt-in basalt-nonfree
# repository: the files of NVIDIA's official .run, byte-identical, split
# into packages (packages/nvidia/files.list says which file goes where), for
# GPUs the open kernel modules support (Turing and newer). The kernel
# modules come from kmod-nvidia-open-<kernel> (nvidia-open-kmod.spec), the
# Basalt configuration (nouveau off, boot check, fallback) from basalt-nvidia.
#
# Build: scripts/release/build-nonfree.sh, which checks the .run against
# packages/nvidia/source.conf (SHA-256 of the .run and of its LICENSE).
#
# NVIDIA's Driver License Agreement allows distribution when the binaries
# are not modified (except for uncompressing) and the Agreement goes to
# every recipient. So: no stripping, no debuginfo, no build-id links, no
# brp processing at all, no compression, and LICENSE in every package.
# packages/nvidia/check-identical.sh proves it on the built RPMs.

%global debug_package %{nil}
%global __strip /bin/true
%global __os_install_post %{nil}
%global _build_id_links none
%undefine _missing_build_ids_terminate_build
# The DLSS DLLs are Windows PE files for Wine/Proton: no ELF dependencies.
%global __provides_exclude_from ^%{_libdir}/nvidia/wine/.*$
%global __requires_exclude_from ^%{_libdir}/nvidia/wine/.*$

# Other NVIDIA driver packagings must never be installed next to this one:
# RPM Fusion's (akmod or kmod and xorg-x11-drv-nvidia) and NVIDIA's own CUDA
# repository (nvidia-kmod-common and the DKMS modules).
%global other_nvidia xorg-x11-drv-nvidia, xorg-x11-drv-nvidia-libs, xorg-x11-drv-nvidia-cuda, xorg-x11-drv-nvidia-cuda-libs, xorg-x11-drv-nvidia-kmodsrc, xorg-x11-drv-nvidia-power, akmod-nvidia, kmod-nvidia, nvidia-kmod-common, kmod-nvidia-open-dkms, kmod-nvidia-latest-dkms, nvidia-open-kmod-common

Name:           nvidia-driver
Version:        595.104.02
Release:        1%{?dist}
Summary:        NVIDIA driver for Turing and newer GPUs (open kernel modules)
License:        LicenseRef-NVIDIA-Driver-License
URL:            https://www.nvidia.com/en-us/drivers/unix/
Source0:        https://download.nvidia.com/XFree86/Linux-x86_64/%{version}/NVIDIA-Linux-x86_64-%{version}.run
Source1:        files.list
Source2:        NOTICE.nvidia
# Ours: turns on NVIDIA's suspend, resume and hibernate units.
Source3:        70-nvidia-power.preset
ExclusiveArch:  x86_64

BuildRequires:  coreutils
BuildRequires:  gawk
# The .run is a makeself archive: tar and zstd unpack it.
BuildRequires:  tar
BuildRequires:  zstd
BuildRequires:  systemd-rpm-macros

Requires:       nvidia-driver-libs%{?_isa} = %{version}-%{release}
Requires:       nvidia-driver-cuda-libs%{?_isa} = %{version}-%{release}
Requires:       nvidia-driver-cuda%{?_isa} = %{version}-%{release}
Requires:       nvidia-driver-firmware = %{version}-%{release}
Requires:       nvidia-driver-power%{?_isa} = %{version}-%{release}
Requires:       kmod-nvidia-open = %{version}
Requires:       basalt-nvidia
Requires:       nvidia-modprobe = %{version}
Conflicts:      %{other_nvidia}

%description
The NVIDIA driver for GeForce, RTX, Quadro and datacenter GPUs of the
Turing generation and newer, with NVIDIA's open GPU kernel modules built
and signed for every Basalt OS kernel: OpenGL, EGL, Vulkan, GBM, VDPAU,
CUDA, OpenCL, NVENC and NVDEC, nvidia-smi and power management. nouveau
is turned off while it is installed.

Packaged by OpenBasalt from NVIDIA's unmodified driver package; not
supported by NVIDIA. Distributed under the NVIDIA Driver License
Agreement (LICENSE). NVIDIA, GeForce and CUDA are trademarks of NVIDIA
Corporation.

%package compute
Summary:        NVIDIA driver for servers: CUDA, NVML and OpenCL, no display stack
Requires:       nvidia-driver-cuda-libs%{?_isa} = %{version}-%{release}
Requires:       nvidia-driver-cuda%{?_isa} = %{version}-%{release}
Requires:       nvidia-driver-firmware = %{version}-%{release}
Requires:       kmod-nvidia-open = %{version}
Requires:       basalt-nvidia
Requires:       nvidia-modprobe = %{version}
Requires:       nvidia-persistenced = %{version}
Conflicts:      %{other_nvidia}

%description compute
The NVIDIA driver for compute on servers: the open kernel modules, CUDA,
NVML (nvidia-smi), OpenCL and the persistence daemon, without OpenGL,
Vulkan or any display package. GeForce and Titan software is not licensed
for datacenter deployment (NVIDIA Driver License Agreement, section 2.8).

%package libs
Summary:        NVIDIA OpenGL, EGL, GLES, Vulkan, GBM and VDPAU libraries
Requires:       nvidia-driver-cuda-libs%{?_isa} = %{version}-%{release}
Requires:       libglvnd-egl%{?_isa}
Requires:       libglvnd-glx%{?_isa}
Requires:       libglvnd-gles%{?_isa}
Requires:       libglvnd-opengl%{?_isa}
Requires:       vulkan-loader%{?_isa}
Requires:       mesa-libgbm%{?_isa}
Requires:       libvdpau%{?_isa}
# EGL platforms for Wayland, GBM and X11: Fedora's builds of NVIDIA's open
# source libraries, not the copies in the .run.
Requires:       egl-wayland%{?_isa}
Requires:       egl-wayland2%{?_isa}
Requires:       egl-gbm%{?_isa}
Requires:       egl-x11%{?_isa}
Requires:       nvidia-kmod(open) = %{version}
Conflicts:      %{other_nvidia}

%description libs
NVIDIA's OpenGL (GLX and EGL through glvnd), GLES, Vulkan, GBM and VDPAU
drivers, OptiX and the ray tracing core, and the DLSS libraries for Wine.

%package cuda-libs
Summary:        NVIDIA CUDA driver, NVML, OpenCL and video encode and decode libraries
Requires:       ocl-icd%{?_isa}
Requires:       nvidia-kmod(open) = %{version}
Conflicts:      %{other_nvidia}

%description cuda-libs
libcuda (the CUDA driver API), NVML, the PTX JIT compiler and NVVM, the
OpenCL driver, NVENC, NVDEC and optical flow, the GPU compiler library
shared with OpenGL, and the CUDA debugger library.

%package cuda
Summary:        The nvidia-smi tool and the CUDA multi-process service
Requires:       nvidia-driver-cuda-libs%{?_isa} = %{version}-%{release}
Conflicts:      %{other_nvidia}

%description cuda
nvidia-smi, nvidia-debugdump, the CUDA multi-process service (MPS) and
nvidia-bug-report.sh.

%package firmware
Summary:        NVIDIA GSP firmware for the open kernel modules
BuildArch:      noarch
Conflicts:      %{other_nvidia}

%description firmware
Firmware of the GPU System Processor (GSP) that the open kernel modules
load on Turing and newer GPUs.

%package power
Summary:        NVIDIA suspend, resume and hibernate units and Dynamic Boost
Requires:       nvidia-driver-cuda%{?_isa} = %{version}-%{release}
Requires:       systemd
Requires:       kbd
Conflicts:      %{other_nvidia}
%{?systemd_requires}

%description power
NVIDIA's systemd units that save and restore video memory around suspend
and hibernation, and nvidia-powerd (Dynamic Boost on laptops that support
it, off by default).

%prep
%setup -q -c -T
sh %{SOURCE0} --extract-only --target nvrun >/dev/null
sha256sum nvrun/LICENSE

%build
# Nothing to build: NVIDIA's binaries are installed as they are.

%install
v=%{version}
lib=%{_libdir}
: >files-libs.list; : >files-cuda-libs.list; : >files-cuda.list; : >files-firmware.list; : >files-power.list
grep -Ev '^[[:space:]]*(#|$)' %{SOURCE1} | while read -r kind sub a b c; do
  case "$kind" in
    file)
      src="${b//@V@/$v}"; dst="${c//@V@/$v}"; dst="${dst//@LIB@/$lib}"
      install -D -p -m "$a" "nvrun/$src" "%{buildroot}$dst"
      echo "$dst" >>"files-$sub.list" ;;
    link)
      dst="${a//@V@/$v}"; dst="${dst//@LIB@/$lib}"; tgt="${b//@V@/$v}"
      install -d "%{buildroot}$(dirname "$dst")"
      ln -s "$tgt" "%{buildroot}$dst"
      echo "$dst" >>"files-$sub.list" ;;
    *) echo "files.list: unknown record $kind" >&2; exit 1 ;;
  esac
done
# Directories the packages own.
cat >>files-libs.list <<EOF
%dir %{_libdir}/nvidia
%dir %{_libdir}/nvidia/wine
%dir %{_datadir}/nvidia
EOF
cat >>files-cuda-libs.list <<EOF
%dir %{_datadir}/nvidia
%dir %{_datadir}/nvidia/files.d
EOF
cat >>files-firmware.list <<EOF
%dir /usr/lib/firmware/nvidia
%dir /usr/lib/firmware/nvidia/%{version}
EOF
# The Agreement and our notice go into every package (%%license).
sed -e "s/@VERSION@/%{version}/g" -e "s/@RUN_SHA256@/$(sha256sum %{SOURCE0} | cut -d' ' -f1)/" %{SOURCE2} >NOTICE
cp -p nvrun/LICENSE LICENSE
cp -p nvrun/README.txt README.txt
cp -p nvrun/NVIDIA_Changelog NVIDIA_Changelog
install -D -p -m 0644 %{SOURCE3} %{buildroot}%{_presetdir}/70-nvidia-power.preset
echo %{_presetdir}/70-nvidia-power.preset >>files-power.list

%post power
%systemd_post nvidia-suspend.service nvidia-resume.service nvidia-hibernate.service nvidia-suspend-then-hibernate.service nvidia-powerd.service

%preun power
%systemd_preun nvidia-suspend.service nvidia-resume.service nvidia-hibernate.service nvidia-suspend-then-hibernate.service nvidia-powerd.service

%postun power
%systemd_postun nvidia-suspend.service nvidia-resume.service nvidia-hibernate.service nvidia-suspend-then-hibernate.service nvidia-powerd.service

%files
%license LICENSE NOTICE
%doc README.txt NVIDIA_Changelog

%files compute
%license LICENSE NOTICE

%files libs -f files-libs.list
%license LICENSE NOTICE

%files cuda-libs -f files-cuda-libs.list
%license LICENSE NOTICE

%files cuda -f files-cuda.list
%license LICENSE NOTICE

%files firmware -f files-firmware.list
%license LICENSE NOTICE

%files power -f files-power.list
%license LICENSE NOTICE

%changelog
* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 595.104.02-1
- First package: NVIDIA 595.104.02 (production branch R595) userspace from
  the official .run, byte-identical, for the open kernel modules.
