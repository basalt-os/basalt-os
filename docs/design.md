# Basalt OS design

Status: pre-alpha, milestone 1. This document describes how Basalt OS is put
together and why. Measurements and open gaps are in
[milestone-0-report.md](milestone-0-report.md) and
[milestone-1-report.md](milestone-1-report.md); Secure Boot, module
signing and disk unlock in detail in [secure-boot.md](secure-boot.md).

## Goals

- A server operating system that is secure by default: SELinux enforcing,
  an encrypted disk that unlocks only while the boot chain is unchanged, SSH
  with keys only, a host firewall.
- Light: a minimal package set, nothing that a server does not need.
- Updates that can always be undone, without taking away the administrator's
  freedom to install and change things.
- Built on an existing distribution's packages instead of from scratch, so
  security fixes come from a large team and software is not scarce.

## The model: a traditional, package-based Fedora remix

Basalt OS is installed once and then updated with `dnf`, package by package,
like any Fedora system. Whatever the administrator installs or configures
stays across updates and reboots. There is no read-only `/usr`, no image
layering and no image-based update tool.

Image-based systems (bootc, rpm-ostree) give atomic updates for free, but they
change how a server is administered: local package installs become layers,
`/usr` is read-only, and a misunderstanding of the model can make installed
software disappear on update. Basalt OS keeps the familiar model and adds the
safety net separately: btrfs snapshots around every transaction, and a way
back from the boot menu.

Fedora is the base because SELinux enforcing is its tested default (the
targeted policy is developed together with the packages), btrfs is its
default file system, and its packages are current. Basalt OS replaces only
the branding and release packages, which is what the Fedora trademark
guidelines ask of a remix, and adds a handful of its own packages. All other
packages, and all updates to them, come from Fedora's mirrors.

## Packages

| Package | Role |
|---|---|
| `basalt-release` | `/usr/lib/os-release` (`NAME="Basalt OS"`, `ID=basalt`, `ID_LIKE=fedora`, see below), the other release files, `rpm` dist macros (still `.fc44`), the Basalt repository definition and signing key, dnf defaults, systemd presets. Provides `system-release` and `system-release(releasever) = 44`, so `$releasever` and Fedora's repositories keep working; conflicts with `fedora-release*` and `generic-release*`. One build per Fedora release (its `Version` is the Fedora release, like `fedora-release`). |
| `basalt-release-server` | Server defaults: SSH hardening drop-in, firewalld zone `basalt` (SSH only) made the default on first install, LLMNR and multicast DNS off, a warning if SELinux is not enforcing. |
| `basalt-logos` and subpackages | Logos and icons (`LOGO=basalt-logo-icon`), `basalt-logos-httpd`, `plymouth-theme-basalt`, `basalt-grub2-theme`, `basalt-backgrounds`, `basalt-logos-compat`. Provides `system-logos`, conflicts with `fedora-logos` and `generic-logos`. Artwork under CC-BY-SA-4.0. |
| `basalt-snapshots` | snapper template, dnf5 hook, snapshot boot menu, setup and rollback tools (below). |
| `basalt-security` | `basalt-tpm` (TPM2 unlock state, re-enrollment, one-boot suspend before a planned change) and `basalt-secureboot` (Secure Boot state, enrollment of the Basalt kernel module CA as a MOK, module signing certificates loaded at boot). See [secure-boot.md](secure-boot.md). |
| `basalt-assistant` | The system assistant, installed by default: `basalt` (diagnosis, typed proposals, confirmed and audited changes), `basalt-assistantd` (event engine, enabled), `basalt-mcp` (MCP tools), `basalt-notify` (desktop notifications, optional webhook); `basalt-assistant-selinux` confines the daemon and the MCP server. See [assistant.md](assistant.md). |
| `basalt-llm` (optional) | Local language model service for the assistant: llama.cpp's server for the CPU, no network, its own SELinux domain. `MODEL=auto` runs the fine-tuned translator that fits the machine (1.7B with 4 or more cores and enough free memory, else 0.6B), chosen at each start. See [local-model.md](local-model.md). |

The release package keeps the file names other software reads
(`/etc/fedora-release`, `/etc/redhat-release`, `/etc/system-release`) with
Basalt content, as `generic-release` does.

`os-release` follows the common practice of Fedora remixes: `VERSION_ID`
is the Fedora release, so tools that key on `ID_LIKE` and `VERSION_ID`
(Ansible, cloud-init, third-party repository setup scripts) behave as on
that Fedora release, and the Basalt version is carried separately:

```
NAME="Basalt OS"
VERSION="44 (Basalt 0.0.1)"
ID=basalt
ID_LIKE=fedora
VERSION_ID=44
BUILD_ID=20261003.0123456789ab
BASALT_VERSION=0.0.1
BASALT_CODENAME=pre-alpha
PRETTY_NAME="Basalt OS 44 (Basalt 0.0.1, pre-alpha)"
```

`BUILD_ID` is the build date and the commit of the tree that built the
package (`BASALT_BUILD_ID`, set by `scripts/lib.sh`). Boot menu entries
are titled from `NAME` and `VERSION` ("Basalt OS (kernel) 44 (Basalt
0.0.1)"); `/etc/system-release` reads "Basalt OS release 44 (Basalt
0.0.1)". After `dnf system-upgrade` to the next Fedora release,
`VERSION_ID` follows it, while `BASALT_VERSION` changes only with a
Basalt release.

Presets (`80-basalt.preset`, read before Fedora's `90-default.preset`):
firewalld, sshd, auditd, snapper cleanup, the snapshot boot menu, the
module signing certificate loader, the assistant's daemon, its
notifications and its audit rotation timer on;
hourly snapper timeline, ModemManager, Bluetooth, Avahi and CUPS off. The
remaining presets are Fedora's, unchanged.

## Repository

Basalt OS serves one small RPM repository with only its own packages
(`/etc/yum.repos.d/basalt.repo`, base URL from the dnf variable
`/etc/dnf/vars/basalt_repo_url`). Packages and repository metadata are
signed with GPG and checked (`gpgcheck=1`, `repo_gpgcheck=1`) against the
OpenBasalt release key that `basalt-release` ships, so the repository can
be served over any transport or mirror. Source RPMs are published next to
the binaries.

The repositories live on one domain, https://obpkg.org, with one path per
repository:

| Path | Content | dnf variable (default) | Enabled |
|---|---|---|---|
| `/basalt/<releasever>/<arch>/` | Basalt OS packages (`[basalt]`) | `basalt_repo_url` (`https://obpkg.org/basalt`) | yes |
| `/basalt-tools/<releasever>/<arch>/` | OpenBasalt tools, metadata only unless chosen (`[basalt-tools]`, ADR 0005) | `basalt_tools_url` (`https://obpkg.org/basalt-tools`) | yes (installer option `repos.tools`) |
| `/basalt-testing/<releasever>/<arch>/` | packages on their way to `/basalt/` (`[basalt-testing]`) | `basalt_testing_url` (`https://obpkg.org/basalt-testing`) | no |
| `/apt/` | Samba Conductor for Debian and Ubuntu | | |
| `/keys/` | public keys (`openbasalt-release-key.asc`) | | |
| `/iso/` | installer images and their checksums | | |

`basalt-release` ships the three dnf variables. Builds for a lab or a
mirror override them with `BASALT_DEFAULT_REPO_URL`,
`BASALT_DEFAULT_TOOLS_URL` and `BASALT_DEFAULT_TESTING_URL`
(`scripts/build-rpms.sh`); an installation points one machine elsewhere
with `repos.basalt.installed_url` and `installed_tools_url` (installer
plan) or `BASALT_REPO_URL` (kickstart), which write the same variables;
on an installed system, edit the files in `/etc/dnf/vars/`. The
repositories are published, signed, by the release step that holds the
release key; no build host signs them.

`scripts/build-rpms.sh` builds every package in a clean Fedora container;
`scripts/repo.sh` signs packages (`rpmsign`), builds metadata
(`createrepo_c`) and signs `repomd.xml` (detached, armored). The OpenBasalt
release key was created offline ([key-ceremony.md](key-ceremony.md)) and is
kept out of build hosts. Lab builds use a development key that never leaves
the lab host. Publishing to obpkg.org (build, sign, upload) is described
in [publishing.md](publishing.md).

## Installer

The installer is the stock Fedora netinst ISO with the Basalt kickstart and
the Basalt repository embedded, made with `mkksiso` (lorax).

Why `mkksiso` and not a custom ISO from lorax or livemedia-creator: the boot
chain on the ISO stays Fedora's signed shim, GRUB and kernel, so it boots with
Secure Boot on and nothing needs signing; the installer itself (Anaconda)
stays the tested upstream one; the build takes minutes, needs no compose
infrastructure and follows Fedora point releases by swapping one input file.
The costs: the installer's own screens and boot menu say "Fedora" in places
(the boot menu entries are renamed), and packages come from the network at
install time (the Basalt packages travel on the ISO). A branded Anaconda
(product image) or an offline ISO are later options.

The kickstart (`kickstart/basalt-server.ks`):

- installs `@core` plus the Basalt packages, with `fedora-release` and
  `fedora-logos` excluded so the Basalt ones are chosen, and the system
  assistant (`basalt-assistant`, `basalt-assistant-selinux`) with
  `basalt-assistantd`, `basalt-notify` and the audit rotation timer
  enabled. The optional local model service (`basalt-llm`) is not
  installed. The minimal profile leaves out hardware firmware, CPU
  microcode and fwupd (and what they pull in: udisks2, polkit, Bluetooth,
  mdadm and others); `kernel-core` only recommends `linux-firmware`.
  `basalt.profile=auto` (default) picks it on virtual machines and cloud
  instances (`systemd-detect-virt --vm`, or the CPU's hypervisor flag) and
  the standard profile on bare metal; `minimal` or `standard` forces one,
  for example `standard` for a VM with a passed-through GPU or NIC that
  needs firmware. The choice and its reason are in
  `/root/basalt-install-pre.log`. Leaving out all weak
  dependencies was measured too and rejected: it also drops `logrotate`,
  `crypto-policies-scripts` and `systemd-pam`;
- partitions the first fixed disk (or `basalt.disk=`): ESP, `/boot` (ext4,
  unencrypted, because GRUB reads it), and one btrfs file system on LUKS2
  (`basalt.encrypt=0` for plain btrfs);
- generates a random LUKS passphrase in `%pre` that exists only in the
  installer's memory; `%post` enrolls a TPM2 key and a recovery key with it
  and then removes it;
- sets a serial-first boot (GRUB menu and kernel console on the serial port
  and the screen, no `rhgb quiet`), the Basalt Plymouth theme for screens,
  and a visible 5 second boot menu;
- adds `lockdown=integrity module.sig_enforce=1` to the kernel command line
  (`basalt.lockdown=0` to leave them out): Fedora kernels already lock down
  under Secure Boot; these keep unsigned modules out with Secure Boot off;
- sets up the disk unlock chosen with `basalt.unlock=`: `tpm2` (default),
  `tang` or `tpm2+tang` (Clevis; `basalt.tang=URL`, `basalt.tang-thp=`
  thumbprint; adds `rd.neednet=1`);
- runs `basalt-snapshots-setup --no-initial-snapshot` last, which also
  writes the GRUB configuration. No snapshot is taken inside the installer:
  files written there are only labeled for SELinux at the end of the
  install, and first-boot setup (SSH host keys, system users) has not run,
  so such a snapshot would not be a bootable state.
  `basalt-initial-snapshot.service` takes the first snapshot on the first
  boot instead.

Optional site files on the media (`/basalt/site.conf`, `/basalt/site.ks`)
make the install fully unattended (accounts, encryption, unlock method,
profile, end action).

## Storage

```
ESP (vfat, /boot/efi)   /boot (ext4)   LUKS2 -> btrfs, label "basalt", compress=zstd:1
                                                ├── root                 /
                                                │   └── .snapshots       /.snapshots (snapper)
                                                ├── home                 /home
                                                ├── srv                  /srv
                                                ├── var_log              /var/log
                                                ├── var_cache            /var/cache
                                                ├── var_tmp              /var/tmp
                                                ├── var_spool            /var/spool
                                                ├── var_lib_containers   /var/lib/containers
                                                ├── var_lib_libvirt      /var/lib/libvirt
                                                ├── var_lib_pgsql        /var/lib/pgsql
                                                └── var_lib_mysql        /var/lib/mysql
```

What is in the root subvolume is the system: it is snapshotted and rolled
back as a whole. That includes the rpm database (`/usr/lib/sysimage/rpm`),
`/etc`, `/opt` (packages install there) and the rest of `/var/lib`, which
holds state that must match the installed packages (the SELinux policy
store in `/var/lib/selinux`, alternatives, systemd state). Data that must
survive a rollback lives in its own subvolume: user homes, served data,
logs (so the logs that explain a rollback survive it), caches and spools,
container storage, virtual machines and the two common databases. Snapshots
of the root do not include these subvolumes.

`compress=zstd:1` is Anaconda's default for btrfs on Fedora; it is active
while the installer writes the system, so the installed files are
compressed (an earlier image-based prototype mounted it only after the fact
and wrote everything uncompressed).

## Encryption and unlock

LUKS2 (aes-xts-plain64, argon2id) holds the btrfs file system. With the
default unlock method (`tpm2`) it has two key slots after the install:

- a TPM2 key bound to PCR 7 (`systemd-cryptenroll --tpm2-pcrs=7`, given
  explicitly: without `--tpm2-pcrs` current systemd binds no PCR at all). PCR
  7 measures the Secure Boot configuration (enabled or not, PK, KEK, db, dbx)
  and which certificates verified the boot loaders. The disk unlocks without
  interaction as long as that is unchanged;
- a recovery key (`systemd-cryptenroll --recovery-key`), shown on the console
  at install time and left in `/root/basalt-recovery-key.txt` with a login
  notice to move it off the machine.

`/etc/crypttab` gets `tpm2-device=auto` and no `headless=true`, so when the
TPM refuses (Secure Boot disabled, keys changed, TPM cleared, disk moved to
another machine) the boot stops at a passphrase prompt on the console and the
serial port, which accepts the recovery key. On machines without a TPM the
recovery key is the only key and is asked for at every boot.

Booting a snapshot from the GRUB menu uses the same signed kernel and boot
loader, so PCR 7 is the same and the disk still unlocks by itself.

With `tang` the TPM2 slot is replaced by a Clevis binding to a Tang server;
with `tpm2+tang` by a Clevis Shamir binding that needs both the TPM (PCR 7)
and the Tang server. The installer's temporary passphrase is removed by its
slot number (tested slot by slot with token plugins off), because a Clevis
slot also counts as a "password" slot for `systemd-cryptenroll`.

Why PCR 7 alone, what it does not cover, and how to change the Secure Boot
state without losing unattended boots (`basalt-tpm suspend`, `reenroll`):
[secure-boot.md](secure-boot.md#disk-unlock-and-the-tpm).

## Snapshots

`basalt-snapshots` sets up snapper for the root and hooks it into dnf:

- `/etc/dnf/libdnf5-plugins/actions.d/basalt-snapper.actions` runs
  `basalt-snapshot-dnf` before and after every transaction through the
  libdnf5 actions plugin (Fedora 44 uses dnf5, so the older
  `python3-dnf-plugin-snapper`, a dnf4 plugin, does not apply). The pre
  snapshot number is passed to the post step through an actions-plugin
  variable; both are described with the dnf command line. The hook is
  disabled for `--installroot` transactions (`enabled=host-only`) and never
  fails a transaction (`raise_error=0`; the script itself always exits 0).
  The same hook runs inside offline transactions (`dnf offline`,
  `dnf system-upgrade`).
- Retention (`basalt-root` template): number cleanup keeps 20 snapshots (5
  important), none younger than 30 minutes; snapshots may use at most 30 % of
  the file system and cleanup keeps 20 % free; empty pre/post pairs are
  dropped; no hourly timeline snapshots of the root. `snapper-cleanup.timer`
  runs daily.

### Rollback

`snapper rollback` works by changing the btrfs default subvolume, so the root
must be mounted through the default subvolume rather than by name.
`basalt-snapshots-setup` therefore makes the `root` subvolume the default,
removes `rootflags=subvol=root` from the kernel command line (boot entries,
`/etc/kernel/cmdline`, `/etc/default/grub`) and `subvol=root` from the `/`
line in `/etc/fstab`, and gives `/.snapshots` its own fstab line
(`subvol=root/.snapshots`) so it is mounted again when the root becomes a
snapshot.

`basalt-rollback N` runs `snapper --ambit classic rollback N`: snapper keeps a
read-only snapshot of the current root, creates a writable copy of snapshot
N and makes it the default subvolume. The next boot runs the rolled-back
system; the data subvolumes are untouched. The ambit is given explicitly
because a read-only root (a snapshot booted from GRUB) would otherwise be
detected as a transactional system.

Kernels live on `/boot`, outside the snapshots. When the target snapshot
predates a kernel update, the newest boot entry has no modules in it, so
`basalt-rollback` makes the newest kernel whose modules exist in the target
the default entry (`grub2-set-default`).

The newer kernel stays on `/boot` although the rolled-back rpm database no
longer knows it, so dnf will never remove it. `basalt-rollback --kernels`
lists every kernel on `/boot` as running, installed (owned by a package),
kept (no package, but a snapshot holds its modules: rolling forward to that
snapshot needs it) or orphaned (neither). Orphaned kernels are reported
after every dnf transaction and removed on request with
`basalt-rollback --clean-kernels` (preview and confirmation,
`kernel-install remove`); the running kernel, kernels the package database
owns and kernels held by a snapshot are never removed, and nothing is
removed while a rollback is pending or the system runs from a read-only
snapshot. Kernels become orphaned when the retention policy deletes the
last snapshot that held them.

A snapshot booted from the menu runs its own, possibly older, copy of
`basalt-rollback`. When the installed system (the default subvolume) has a
newer `basalt-snapshots`, the snapshot's copy hands over to that one: the
milestone 1 tests met a pre-upgrade snapshot whose copy predated the
kernel selection above.

### Booting a snapshot from GRUB

Fedora generates its boot menu from Boot Loader Specification entries
(`/boot/loader/entries`) that GRUB reads directly. `grub-btrfs`, the usual
tool for snapshot boot entries, is not packaged in Fedora and is built around
regenerating `grub.cfg`. Basalt OS uses a small generator instead:

- `/etc/grub.d/42_basalt_snapshots` adds one line to `grub.cfg` (once, at
  setup): source `/boot/grub2/basalt-snapshots.cfg` if it exists.
- `basalt-snapshot-boot update` rewrites that file: a "Basalt OS snapshots
  (read-only, key s)" submenu (the `s` key opens it from the main menu) with the newest pre and single snapshots (8 by
  default). It runs after every pre and post snapshot, after snapper cleanup
  and at boot, so the menu never lists a deleted snapshot. No
  `grub2-mkconfig` run is needed for updates.
- Each entry boots a kernel from `/boot` with
  `rootflags=subvol=root/.snapshots/N/snapshot`. Kernels live on `/boot`,
  outside the snapshots, so the generator picks the newest kernel that is
  still on `/boot` and whose modules exist inside the snapshot; a snapshot
  without such a kernel gets no entry.
- The snapshot is a read-only subvolume, so the system boots read-only, with
  the data subvolumes mounted read-write from its `/etc/fstab`. Services that
  write to the root fail or degrade; SSH, logs and the data are available.
  `basalt-rollback` (with no number) keeps the booted snapshot. The entries
  add `basalt.snapshot=N` to the kernel command line, which is how the tools
  know the system was started from this menu.

## Release upgrades

Fedora releases about every six months. A Basalt OS system moves to the next
Fedora release with `dnf system-upgrade`, which downloads the new release's
packages and installs them in an offline transaction at reboot; the dnf hook
takes a pre snapshot before that transaction, and the snapshot boot menu
offers the way back. The Basalt repository publishes `basalt-release` (and
the other Basalt packages) for each Fedora release under
`$releasever/$basearch`; the project tests each upgrade path in VMs before
announcing it. The rpm database, the kernel set on `/boot` and the snapshots
after a release upgrade are discussed in the milestone reports; milestone 1
booted the pre-upgrade snapshot after a 44 to 45 upgrade, rolled back to
44 and forward to 45 again.

## Security defaults

- SELinux enforcing, targeted policy, unmodified. Every boot in the lab is
  checked for AVC, USER_AVC and SELINUX_ERR records.
- SSH: `PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
  `AuthenticationMethods publickey`, `PermitRootLogin prohibit-password`, no
  X11, agent or TCP tunnel forwarding, `MaxAuthTries 3`.
- firewalld default zone `basalt`, SSH only.
- auditd on, so denials are recorded.
- Secure Boot with the distribution's signed chain (Microsoft-signed shim,
  Fedora-signed GRUB and kernel); Basalt's own keys in two modes: the
  Basalt kernel module CA as a MOK (signed Basalt modules load under
  lockdown), and an advanced custom db mode where the firmware trusts only
  Basalt's PK, KEK and db. See [secure-boot.md](secure-boot.md).
- Kernel lockdown (`integrity`) and module signature enforcement on the
  command line, also when Secure Boot is off.
