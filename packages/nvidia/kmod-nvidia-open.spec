# kmod-nvidia-open: follows the newest Basalt OS kernel that has a signed
# NVIDIA module in basalt-nonfree, and holds kernel updates until the next
# one has its module.
#
#   rpmbuild --define "kver 7.2.8-200.fc44.x86_64" -ba kmod-nvidia-open.spec
#
# kver is the newest kernel of the Fedora release's stable repositories
# (fedora, updates) with a kmod-nvidia-open-<kver> in this build. The
# package requires that module and conflicts with any newer kernel-core:
# dnf upgrade then leaves a newer kernel out (it reports the conflict)
# until a new kmod-nvidia-open with its module is published, and both
# arrive in the same transaction. basalt-nvidia's kernel-install plugin
# covers a kernel installed some other way (it never becomes the default
# boot entry without its module). See docs/nvidia.md.
#
# Release: the kernel's version and release, so a newer kernel always
# means a newer package ("7.2.8.200" for 7.2.8-200.fc44.x86_64), then a
# counter for changes of this spec.

# "unset" only lets tools such as rpmlint read the spec; %%prep refuses it.
%{!?kver:%global kver unset-0.fc0.x86_64}
%global kver_v %(echo %{kver} | sed 's/-.*//')
%global kver_r %(echo %{kver} | sed 's/^[^-]*-//; s/\\.fc[0-9]*\\..*$//')
%global kver_evr %(echo %{kver} | sed 's/\\.x86_64$//')

Name:           kmod-nvidia-open
Version:        595.104.02
Release:        %{kver_v}.%{kver_r}.1%{?dist}
Summary:        NVIDIA open GPU kernel modules for the newest Basalt OS kernel
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
ExclusiveArch:  x86_64

Requires:       kmod-nvidia-open-%{kver} = %{version}
# The kernel update guard: no newer kernel without its module.
Conflicts:      kernel-core > %{kver_evr}

%description
Keeps the NVIDIA open GPU kernel modules installed for the newest Basalt
OS kernel:
%{kver}.
A newer kernel is held back by dnf until its signed module is published
in basalt-nonfree; then both are installed together.

%prep
[ "%{kver}" != unset-0.fc0.x86_64 ] || { echo "define kver (rpmbuild --define 'kver RELEASE')" >&2; exit 1; }

%files

%changelog
* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 595.104.02-1
- First package: follows the newest kernel with a signed module and holds
  newer kernels until their module is published.
