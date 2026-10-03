# Basalt OS design

Status: pre-alpha, milestone 0. This document describes how Basalt OS is put
together and why. Measurements and open gaps are in
[milestone-0-report.md](milestone-0-report.md).

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
| `basalt-release` | `/usr/lib/os-release` (`NAME="Basalt OS"`, `ID=basalt`, `ID_LIKE=fedora`), the other release files, `rpm` dist macros (still `.fc44`), the Basalt repository definition and signing key, dnf defaults, systemd presets. Provides `system-release` and `system-release(releasever) = 44`, so `$releasever` and Fedora's repositories keep working; conflicts with `fedora-release*` and `generic-release*`. One build per Fedora release (its `Version` is the Fedora release, like `fedora-release`). |
| `basalt-release-server` | Server defaults: SSH hardening drop-in, firewalld zone `basalt` (SSH only) made the default on first install, LLMNR and multicast DNS off, a warning if SELinux is not enforcing. |
| `basalt-logos` and subpackages | Logos and icons (`LOGO=basalt-logo-icon`), `basalt-logos-httpd`, `plymouth-theme-basalt`, `basalt-grub2-theme`, `basalt-backgrounds`, `basalt-logos-compat`. Provides `system-logos`, conflicts with `fedora-logos` and `generic-logos`. Artwork under CC-BY-SA-4.0. |
| `basalt-snapshots` | snapper template, dnf5 hook, snapshot boot menu, setup and rollback tools (below). |

The release package keeps the file names other software reads
(`/etc/fedora-release`, `/etc/redhat-release`, `/etc/system-release`) with
Basalt content, as `generic-release` does.

Presets (`80-basalt.preset`, read before Fedora's `90-default.preset`):
firewalld, sshd, auditd, snapper cleanup and the snapshot boot menu on;
hourly snapper timeline, ModemManager, Bluetooth, Avahi and CUPS off. The
remaining presets are Fedora's, unchanged.

## Repository

Basalt OS serves one small RPM repository with only its own packages
(`/etc/yum.repos.d/basalt.repo`, base URL from the dnf variable
`/etc/dnf/vars/basalt_repo_url`). Packages and repository metadata are
signed with GPG and checked (`gpgcheck=1`, `repo_gpgcheck=1`), so the
repository can be served over any transport or mirror. Source RPMs are
published next to the binaries.

`scripts/build-rpms.sh` builds every package in a clean Fedora container;
`scripts/repo.sh` signs packages (`rpmsign`), builds metadata
(`createrepo_c`) and signs `repomd.xml` (detached, armored). A release key
does not exist yet: it will be created offline and kept out of build hosts.
Lab builds use a development key that never leaves the lab host.

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
  `fedora-logos` excluded so the Basalt ones are chosen;
- partitions the first fixed disk (or `basalt.disk=`): ESP, `/boot` (ext4,
  unencrypted, because GRUB reads it), and one btrfs file system on LUKS2
  (`basalt.encrypt=0` for plain btrfs);
- generates a random LUKS passphrase in `%pre` that exists only in the
  installer's memory; `%post` enrolls a TPM2 key and a recovery key with it
  and then removes it;
- sets a serial-first boot (GRUB menu and kernel console on the serial port
  and the screen, no `rhgb quiet`), the Basalt Plymouth theme for screens,
  and a visible 5 second boot menu;
- runs `basalt-snapshots-setup --no-initial-snapshot` last, which also
  writes the GRUB configuration. No snapshot is taken inside the installer:
  files written there are only labeled for SELinux at the end of the
  install, and first-boot setup (SSH host keys, system users) has not run,
  so such a snapshot would not be a bootable state.
  `basalt-initial-snapshot.service` takes the first snapshot on the first
  boot instead.

Optional site files on the media (`/basalt/site.conf`, `/basalt/site.ks`)
make the install fully unattended (accounts, encryption choice, end action).

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

LUKS2 (aes-xts-plain64, argon2id) holds the btrfs file system. After the
install it has two key slots:

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
the default entry (`grub2-set-default`). The newer kernel stays on `/boot`
although the rolled-back rpm database no longer knows it (see the report's
gaps).

### Booting a snapshot from GRUB

Fedora generates its boot menu from Boot Loader Specification entries
(`/boot/loader/entries`) that GRUB reads directly. `grub-btrfs`, the usual
tool for snapshot boot entries, is not packaged in Fedora and is built around
regenerating `grub.cfg`. Basalt OS uses a small generator instead:

- `/etc/grub.d/42_basalt_snapshots` adds one line to `grub.cfg` (once, at
  setup): source `/boot/grub2/basalt-snapshots.cfg` if it exists.
- `basalt-snapshot-boot update` rewrites that file: a "Basalt OS snapshots
  (read-only)" submenu with the newest pre and single snapshots (8 by
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
after a release upgrade are discussed in the milestone report.

## Security defaults

- SELinux enforcing, targeted policy, unmodified. Every boot in the lab is
  checked for AVC, USER_AVC and SELINUX_ERR records.
- SSH: `PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
  `AuthenticationMethods publickey`, `PermitRootLogin prohibit-password`, no
  X11, agent or TCP tunnel forwarding, `MaxAuthTries 3`.
- firewalld default zone `basalt`, SSH only.
- auditd on, so denials are recorded.
- Secure Boot with the distribution's signed chain (Microsoft-signed shim,
  Fedora-signed GRUB and kernel). Own keys and signed modules are planned.
