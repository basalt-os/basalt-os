# The NVIDIA driver (basalt-nonfree)

Basalt OS can install the NVIDIA driver for GPUs of the Turing generation
and newer (GeForce GTX 16 and RTX 20 series onward, and the matching
workstation, laptop and datacenter GPUs) from its own repository,
`basalt-nonfree`, which is off by default. The kernel modules are NVIDIA's
open GPU kernel modules, built for every Basalt OS kernel and signed with a
Basalt key, so the driver loads with Secure Boot and kernel lockdown on,
with no compiler on the machine and no key of its own. The rest of the
driver is NVIDIA's own software, unmodified.

NVIDIA, GeForce and CUDA are trademarks of NVIDIA Corporation. The driver
is packaged by OpenBasalt for Basalt OS and is not supported by NVIDIA:
report problems with it to the Basalt OS project.

## What is in the repository

https://obpkg.org/basalt-nonfree/, signed with the OpenBasalt release key
like the other Basalt repositories (`gpgcheck=1`, `repo_gpgcheck=1`).
`basalt-nonfree-release` (in the basalt repository) defines it, off.

| Package | Content | License |
|---|---|---|
| `nvidia-driver` | the desktop driver: pulls in everything below except `nvidia-driver-compute` and `nvidia-persistenced` | NVIDIA Driver License Agreement |
| `nvidia-driver-compute` | the server driver: kernel modules, CUDA, NVML, OpenCL, nvidia-smi, the persistence daemon, no display packages | NVIDIA Driver License Agreement |
| `nvidia-driver-libs` | OpenGL (GLX and EGL through glvnd), GLES, Vulkan, GBM, VDPAU, OptiX, ray tracing, DLSS libraries for Wine | NVIDIA Driver License Agreement |
| `nvidia-driver-cuda-libs` | libcuda, NVML, the PTX compiler and NVVM, OpenCL, NVENC, NVDEC, optical flow | NVIDIA Driver License Agreement |
| `nvidia-driver-cuda` | nvidia-smi, nvidia-debugdump, the CUDA multi-process service, nvidia-bug-report.sh | NVIDIA Driver License Agreement |
| `nvidia-driver-firmware` | the GSP firmware the open kernel modules load | NVIDIA Driver License Agreement |
| `nvidia-driver-power` | NVIDIA's suspend, resume and hibernate units, nvidia-powerd (off) | NVIDIA Driver License Agreement |
| `kmod-nvidia-open-<kernel>` | the open kernel modules for one kernel, signed | MIT and GPL-2.0-only |
| `kmod-nvidia-open` | follows the newest kernel and holds newer kernels until their module exists (see Kernel updates) | Apache-2.0 |
| `nvidia-modprobe`, `nvidia-persistenced` | NVIDIA's open tools, built from their GitHub sources | GPL-2.0-only, MIT |
| `basalt-nvidia` | Basalt's part: nouveau off, the boot-time choice, the check and the fallback, the kernel guard, PRIME offload, the module signing certificate | Apache-2.0 |

One driver version at a time, NVIDIA's current production branch: today
595.104.02 (branch R595). `packages/nvidia/source.conf` pins the version
and the SHA-256 of every source: the `.run` package from
download.nvidia.com, the Agreement inside it, and the GitHub archives of
the open kernel modules, nvidia-modprobe and nvidia-persistenced. A build
refuses anything that does not match.

The EGL platform libraries for Wayland, GBM and X11 come from Fedora's
`egl-wayland`, `egl-wayland2`, `egl-gbm` and `egl-x11`, and the glvnd and
OpenCL loaders from Fedora's `libglvnd` and `ocl-icd`, all built from
source; the copies in the `.run` are not used. Not packaged: the X.Org
driver and GLX server module (Basalt OS runs Wayland sessions; X11 programs
run through Xwayland), nvidia-settings and nvidia-xconfig, the installer,
the DLSS updater, Vulkan SC and the 32-bit libraries.

## Which GPUs

`basalt drivers` and Settings, Additional drivers, read every display
controller from sysfs (PCI class 03) and look NVIDIA's device ids up in the
list of supported GPUs that ships with the driver (`supported-gpus.json`,
kept as `nvidia-gpus.json` in basalt-assistant, with its license):

| The GPU is | What happens |
|---|---|
| supported by this driver with the open kernel modules (Turing and newer) | the NVIDIA driver is recommended |
| one of the 580 legacy branch (Maxwell, Pascal, Volta, for example a GeForce GTX 1070) | nothing is installed: the open modules do not support these GPUs and Basalt OS does not package NVIDIA's closed kernel module; nouveau drives them |
| older (470 branch and before) | nouveau drives it |
| not in the list | nothing is recommended (the GPU may be newer than the driver) |

On a machine with a 580 branch GPU, RPM Fusion's `akmod-nvidia-580xx`
builds the closed module on the machine; with Secure Boot on it needs your
own key, enrolled once (secure-boot.md, "Your own modules"). Do not mix it
with basalt-nonfree.

## Installing

From the desktop: Settings, Additional drivers. The page lists the GPUs,
says what installing changes, shows the NVIDIA Driver License Agreement,
which must be accepted before the Install button works, and then shows the
system assistant's proposal with the exact commands to confirm. From a
terminal:

```sh
basalt drivers                          # the GPUs, the driver that fits, what changes
basalt drivers license nvidia           # the Agreement
sudo basalt drivers install nvidia --apply
sudo reboot
```

The install is the assistant's `driver.install` action (assistant.md). It
takes a snapshot, then runs exactly:

```
dnf -y install basalt-nonfree-release
dnf config-manager setopt basalt-nonfree.enabled=1
dnf -y install --skip-unavailable nvidia-driver kmod-nvidia-open-<running kernel>
basalt-nvidia arm
```

(`nvidia-driver-compute` on a system without a graphical target.) The
proposal carries the SHA-256 of the Agreement that was shown; the assistant
refuses any other value. The packages also take their own snapshots before
and after the transaction, like every dnf transaction.

Requirement: with Secure Boot on, the Basalt kernel module CA must be
enrolled as a MOK (secure-boot.md); that is the one confirmation at the
firmware console Basalt OS needs per machine, for every Basalt-signed
module. `basalt drivers` refuses the install until it is enrolled or its
enrollment is pending:

```sh
sudo basalt-secureboot enroll-mok       # then confirm at the next start (MokManager)
```

## The first start, the check and the fallback

`basalt-nvidia` (package `basalt-nvidia`) turns nouveau off
(`/usr/lib/modprobe.d/basalt-nvidia.conf`: `blacklist nouveau`,
`options nouveau modeset=0`) and keeps nouveau and the NVIDIA modules out
of the initramfs (`/usr/lib/dracut/dracut.conf.d/90-basalt-nvidia.conf`):
the NVIDIA modules are signed with a certificate the kernel learns only
from the root file system (`basalt-module-keys.service`), and the choice of
driver is made there, before udev:

1. `basalt-nvidia-boot.service` runs after the module certificates are
   loaded and before udev. It uses nouveau for this boot (a file
   `/run/modprobe.d/basalt-nvidia.conf` that replaces the packaged one)
   when the running kernel has no NVIDIA module, when the module does not
   match the installed driver version, or when the module does not load
   (a refused signature, no supported GPU). Otherwise the NVIDIA module is
   loaded there. On the first start after an install (a trial) it records
   the boot before loading the module.
2. `basalt-nvidia-check.service` runs after the login screen: the module
   is loaded, `nvidia-smi` answers, and on a desktop where the NVIDIA GPU
   drives the display, the login screen is up and `nvidia_drm` runs with
   kernel mode setting. A trial that passes makes the driver active.
3. A trial that fails, or that never reaches its check (a hang, a forced
   power off: the next boot sees the unconfirmed trial), falls back: the
   file `/etc/modprobe.d/basalt-nvidia.conf` replaces the packaged one, so
   nouveau is used from then on, and the reason is recorded. A driver that
   was already active and fails a check later is reported, not switched off.

The person is told why: a notification at the start of the desktop
session opens Settings, Additional drivers, which shows the reason and
offers the rollback to the snapshot taken before the install (the
assistant's `snapshot.rollback`, confirmed the same way). From a terminal:

```sh
basalt drivers                          # state, reason, snapshot
basalt-nvidia status                    # the same in detail; --json for scripts
sudo basalt drivers rollback --apply    # back to the snapshot taken before the install
sudo basalt-nvidia retry                # or: try the NVIDIA driver again at the next start
```

Every step is recorded in the audit ledger (ledger.md): `driver.install`,
`driver.check`, `driver.fallback`, `driver.retry`, `driver.boot` (a boot
that used nouveau), `driver.kernel_hold` and `driver.kernel_release`, and
the assistant's own records of the proposal and the apply. Records made
early in boot, before the ledger service runs, wait in
`/var/lib/basalt-nvidia/ledger-queue` and keep their time; the check after
boot sends them.

State: `/var/lib/basalt-nvidia/state` (part of the root, so a rollback
takes it back too) and `/run/basalt-nvidia/boot` (this boot's choice).

## Kernel updates

Kernels come from Fedora's repositories; the NVIDIA modules from
basalt-nonfree, built for every kernel of the release's `fedora`, `updates`
and `updates-testing` repositories. A kernel must never become the
default boot entry without its signed module. Two layers make sure of it:

1. Dependencies. `kmod-nvidia-open` requires the module of the newest
   stable kernel and conflicts with any newer `kernel-core`. `dnf upgrade`
   therefore leaves a newer kernel out (it reports the conflict) until a
   new `kmod-nvidia-open` that requires that kernel's module is published;
   then the kernel and its module are installed in the same transaction.
   Building modules for `updates-testing` kernels too keeps the wait short.
2. The boot entry. `99-zz-basalt-nvidia.install`, a kernel-install plugin
   that runs after the one that makes a new kernel the default, keeps the
   default on the newest installed kernel that has a matching module (a
   kernel installed despite the conflict, with `--allowerasing` or from
   another source) and records the hold; when the module arrives, the
   module package makes that kernel the default again. Older kernels and
   snapshots chosen from the boot menu still start: without their module
   the boot uses nouveau.

`basalt drivers` and the desktop show a held kernel. The cost of the hold:
a kernel update, security fixes included, waits for its module.

## Laptops with two GPUs

On a laptop with an integrated GPU (Intel or AMD) and an NVIDIA GPU, the
integrated GPU keeps the display and the NVIDIA GPU renders on demand
(PRIME render offload); CUDA uses the NVIDIA GPU directly. Run a program on
it with:

```sh
basalt-nvidia-run glxinfo -B
basalt-nvidia-run vkcube
```

which sets `__NV_PRIME_RENDER_OFFLOAD=1`, `__VK_LAYER_NV_optimus=NVIDIA_only`,
`__GLX_VENDOR_LIBRARY_NAME=nvidia` and the NVIDIA EGL vendor file for that
program only. On Ampere and newer laptop GPUs the NVIDIA GPU powers down
when idle (NVIDIA's runtime power management, on by default there).

## Desktop sessions

sway and SwayFX refuse to start while NVIDIA's kernel module is loaded
unless they get `--unsupported-gpu`; `basalt-session` adds it whenever the
module is loaded, on a laptop with two GPUs too. They work with the NVIDIA
driver 560 and newer (GBM, explicit sync); the driver's own application
profile already limits its pool of freed video memory for wlroots
compositors. niri needs no flag; `basalt-nvidia` ships the same memory
profile for it (`/etc/nvidia/nvidia-application-profiles-rc.d/50-basalt-niri.json`).
`nvidia_drm` runs with kernel mode setting and its framebuffer console
(`modeset=1 fbdev=1`), and video memory is kept across suspend and
hibernation (NVIDIA's units in `nvidia-driver-power`).

## Servers

On a system that starts without a desktop, the recommendation is
`nvidia-driver-compute`: the kernel modules, CUDA, NVML, OpenCL,
nvidia-smi and the persistence daemon (enabled), and no OpenGL, Vulkan or
display package. NVIDIA's Agreement (section 2.8) does not license GeForce
and Titan software for datacenter deployment; that applies to whoever runs
it, and `basalt drivers` and the desktop repeat it when they find a GeForce
or Titan GPU.

## Packaging rules

NVIDIA's Agreement allows distributing the driver for operating systems
with an open source kernel when the binaries are not modified (except for
uncompressing) and the Agreement goes to every recipient. The packaging
follows that:

- Every NVIDIA file is installed byte-identical to the file in the `.run`:
  the spec turns off stripping, debuginfo, build-id links and every brp
  step; nothing is compressed, patched or relinked.
  `packages/nvidia/files.list` says where each file goes (NVIDIA's own
  installer paths) and `packages/nvidia/check-identical.sh` proves it on
  the built packages: the SHA-256 of every installed NVIDIA file equals the
  file in the `.run`, and the packages hold nothing else but the Agreement,
  our notice and our power preset.
- The Agreement (`LICENSE`) and a notice with the source URL, version and
  checksum are in every NVIDIA package (`/usr/share/licenses/<package>/`);
  the build refuses a `.run` whose Agreement differs from the pinned one,
  and the desktop and `basalt drivers license nvidia` show the same text
  before the install.
- The open kernel modules are built from the GitHub source of the same
  version. Each `kmod-nvidia-open-<kernel>` source package is the
  corresponding source: the upstream archive, the spec and the module
  signatures; `COPYING` (MIT and GPLv2) is in each binary package.
- nvidia-modprobe and nvidia-persistenced are built from their GitHub
  sources, not taken from the `.run`.
- The packages conflict with the other NVIDIA packagings: RPM Fusion's
  (`akmod-nvidia`, `kmod-nvidia`, `xorg-x11-drv-nvidia*`) and NVIDIA's CUDA
  repository's (`nvidia-kmod-common`, the DKMS modules). Use one source of
  the driver at a time.

## Signing the kernel modules

The modules are signed with the basalt-nonfree module signing key, whose
certificate the OpenBasalt kernel module CA issues (key-ceremony.md). It is
a key of its own, separate from the one for Basalt's other modules, so it
can be replaced or revoked alone. `basalt-nvidia` ships the certificate as
`/usr/lib/basalt/module-keys/basalt-nonfree-module-signing.der`;
`basalt-module-keys.service` adds it to the kernel's secondary keyring at
boot, which accepts it because the enrolled CA issued it.

The build host never holds the key, and the signer never compiles:

```sh
scripts/release/build-nonfree.sh modules /tmp/nvidia-modules            # build host: unsigned modules
scripts/release/sign-modules.sh --op /tmp/nvidia-modules /tmp/nvidia-sigs   # release signer: signatures only
scripts/release/build-nonfree.sh packages /tmp/nvidia-sigs /tmp/nonfree-in  # build host: every package
```

The first step builds the modules for every kernel, without network, in a
build image made for this driver version (the toolchain and every kernel's
headers). The signer checks them, signs each one in a container without
network, verifies the signatures with the certificate alone and returns
only the signatures. The third step builds the same source again in the
same image, checks that each module is byte-identical to the one signed
(a fixed build date, user and host make the build reproducible) and
appends the signature; a module that differs stops the build. The
signature is what the kernel's `sign-file` appends: a detached CMS
signature with SHA-512, no certificates and no signed attributes, the
`module_signature` trailer and `~Module signature appended~`. Rebuilding a
source package gives the same signed modules.

The rest of the publish is the usual one (publishing.md) with
`OB_REPO=basalt-nonfree`.

## Removing the driver

```sh
sudo dnf remove nvidia-driver basalt-nvidia 'kmod-nvidia-open*'
sudo dnf config-manager setopt basalt-nonfree.enabled=0
sudo reboot
```

Removing `basalt-nvidia` removes the fallback file it wrote and rebuilds
the initramfs; nouveau is used again at the next start. Or roll back to the
snapshot taken before the install (`sudo basalt drivers rollback --apply`).
