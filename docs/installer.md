# Basalt OS installer

Status: pre-alpha, version 0.2.0. The kickstart installer
(`kickstart/basalt-server.ks` on the Fedora netinst ISO, see
[design.md](design.md#installer)) keeps working and remains the path for
complex storage and fully unattended mass installs.

`basalt-installer` is Basalt OS's own installer: one Go engine that
executes a declarative install plan, with three ways in.

| Frontend | Where | How it reaches the engine |
|---|---|---|
| Text wizard (`basalt-installer tui`) | the screen and the serial console | in process |
| Graphical installer (Quickshell, `basalt-installer-gui`) | the screen, in the cage kiosk compositor | local JSON API (`basalt-installer serve`) on a Unix socket |
| Plan file (`basalt-installer install --plan`) | scripts, CI | in process |
| Boot menu (`basalt.inst.plan=` with `basalt.inst.confirm=`) | unattended installs | the engine service; the text installers follow it |

Every frontend produces the same plan, shows the same preview and needs
the same confirmation; the engine checks everything again.

## Principles

- Preview first. Before anything is written, the engine generates the
  exact list of steps: every command line (an argv, never a shell
  string) and the full content of every file it writes. File system and
  LUKS UUIDs, the FAT volume ID and the machine ID are chosen at that
  moment, so the preview is the installation, not an approximation of it.
  The person reads it, then types the disk's name (`vda`, `nvme0n1`) to
  confirm the wipe. The install request names the preview's token; a plan
  changed after the preview is refused.
- Secrets never reach a log or the preview: passwords travel on a
  command's standard input, the boot passphrase in its environment, and
  both are scrubbed from any output line. The recovery key is captured
  from `systemd-cryptenroll --recovery-key` and goes to the frontend once;
  it is never written to a disk unless the person or the plan asks for a
  copy on a USB stick (see [The recovery key](#the-recovery-key)).
- One installation at a time per machine (a lock), even with the text
  installer on the serial port and the graphical one on the screen.
- The disk is probed again right before the first write; a disk that got
  mounted or opened in the meantime stops the installation.
- The engine runs in its own mount namespace (propagation from the host
  only), so the target's mounts never appear in, or get pinned by, the
  namespaces of running services.
- Rollback where possible: mounts, the open LUKS volume and the temporary
  key are undone in reverse order when a step fails or the person
  cancels. A wiped partition table cannot be restored; the summary says
  so plainly.
- An install log for audit: one JSON record per line, each with the
  SHA-256 of the previous one (`basalt-installer log verify FILE`). A copy
  goes into the installed system at `/var/log/basalt-installer/`, with the
  plan (secrets replaced) and a summary line in the kickstart's format.

## The plan

YAML or JSON, schema `basalt-install-plan/v1`; unknown fields are errors,
so a typo never falls back to a default silently. `basalt-installer plan
template` prints a commented plan; `packages/basalt-installer/examples/`
has one.

| Field | Default | Notes |
|---|---|---|
| `edition` | `server` | `desktop` is accepted and marked experimental: the server system plus `packages.extra` |
| `target.disk` | | the disk Basalt OS goes on |
| `target.wipe` | `false` | `true` erases the whole disk; `false` keeps every partition on it and installs into its largest free region (dual boot, GPT only); a plan for an empty disk must say `true` |
| `target.esp` | none | with `wipe: false`, an existing EFI system partition to share (for example Windows'); by default Basalt OS gets its own in the free space |
| `layout.mode` | `automatic` | `manual`: `esp_mib`, `boot_mib`, `root_gib` (0 = rest), `subvolumes` (root, home and var_log required) |
| `encryption.enabled` | `true` | LUKS2 under btrfs (ADR 0002) |
| `encryption.scope` | `system` | `system` (everything), `home` (a new /home partition of its own only: the system, `/etc`, the logs and `/var` stay unencrypted; the system partition is 64 GiB unless `layout.root_gib` says otherwise) or `none` (servers in a controlled place; same as `enabled: false`) |
| `home.existing` | none | adopt a partition that already holds home directories: `device`, `passphrase` (LUKS; checked before anything is written, never stored), `tpm2` (also enroll this machine's TPM, the passphrase keeps working). It is never formatted |
| `encryption.unlock` | `tpm2` | `tpm2` (sealed to PCR 7), `tang`, `tpm2+tang`, `recovery-only`; `tpm2_pcrs` must be `[7]` |
| `encryption.passphrase` | none | an extra key slot typed at boot (desktops) |
| `encryption.store_recovery_key` | `false` | `true` also leaves the key in `/root/basalt-recovery-key.txt` on the disk it protects (labs; the kickstart's `basalt.recovery-key=store`) |
| `encryption.recovery_key_media` | none | the label of a file system on a USB stick (removable or USB disk, FAT, exFAT, ext4, btrfs or XFS) that receives a copy of the key when the installation has succeeded; that counts as the acknowledgement, so nobody has to be there. The stick must be plugged in when the plan is validated |
| `profile` | `auto` | `minimal` on a virtual machine, `standard` on bare metal (same rule as the kickstart) |
| `hostname`, `timezone`, `locale`, `keymap` | `basalt`, `Etc/UTC`, `en_US.UTF-8`, `us` | |
| `lockdown` | `true` | `lockdown=integrity module.sig_enforce=1` on the kernel command line |
| `accounts.root` | locked | `ssh_keys`, `password` or `password_hash` |
| `accounts.user` | none | `name`, `password` or `password_hash`, `ssh_keys`, `admin` (wheel, default true), `home_dir` (default `/home/<name>`), `uid` and `gid` (the numbers of the files on an existing /home) |
| `ssh.password_auth` | `false` | Basalt OS accepts public keys only; `true` adds a drop-in and a warning |
| `network.mode` | `dhcp` | `static`: `interface`, `address` (CIDR), `gateway`, `dns` |
| `repos.basalt.url` | `media` | the repository on the installer image, or an http(s) URL |
| `repos.basalt.installed_url` | `url` when it is http(s), else `https://obpkg.org/basalt` | what the installed system uses (`/etc/dnf/vars/basalt_repo_url`) |
| `repos.basalt.installed_tools_url` | `https://obpkg.org/basalt-tools` with the default repository, else `<installed_url>/tools` (a lab or mirror) | the basalt-tools repository of the installed system (`/etc/dnf/vars/basalt_tools_url`) |
| `repos.basalt.gpg_key` | the media's key | the repository key used during the installation; the preview says whether it is the OpenBasalt release key (fingerprint `3601734842BD4E482D19DE4AE4EED5ECA395B302`) or another one, such as a lab key. The installed system trusts the key that `basalt-release` ships |
| `repos.fedora.baseurl`, `updates_baseurl` | Fedora's mirrors | for a local mirror |
| `repos.testing` | `false` | `true` turns on `basalt-testing` (pre-release packages): read at install time from `basalt/testing` on the media when the media carries it, else from `<installed_url>/testing` (or https://obpkg.org/basalt-testing with the default repository), and turned on in the installed system with a dnf repository override. The live desktop's plan sets it, because the desktop shell is pre-release |
| `repos.tools` | `true` | the `basalt-tools` repository that `basalt-release` ships stays enabled (metadata only, nothing installed unless chosen, ADR 0005); `false` turns it off with a dnf repository override |
| `repos.third_party.tui_tools` | `true` | the tui-tools repository with its key; the key's fingerprint is pinned in the installer and checked before it is written |
| `assistant` | `true` | `basalt-assistant` installed and its daemon enabled |
| `packages.extra` | none | more packages |
| `finish` | `reboot` | `reboot`, `poweroff`, `none` |

Validation reports errors (blocking) and warnings (shown in the review).
It rejects what this version does not handle and points to the kickstart:
partitions as targets, RAID members and md devices, multipath, iSCSI,
device-mapper devices, disks in use, BIOS boots, disks under 16 GiB, MBR
disks whose partitions should be kept.

## Storage choices

Owner decisions of 2026-10-04 (encryption scope, an existing /home, dual
boot); every frontend offers them and the plan records them.

- Next to other systems. When the chosen disk already has partitions, the
  wizard asks whether to erase it or to use its free space. With the free
  space, the preview's partition step names every partition that is kept
  and every new one with its exact sectors; the new partitions take the
  lowest free numbers inside the largest free region, nothing else on the
  disk is written, and a failure deletes only the new partitions. Basalt
  OS gets its own EFI system partition unless the plan shares an existing
  one (`target.esp`; then Fedora's boot files go into `EFI/fedora` on it).
  The firmware boot menu lists "Basalt OS" next to the other systems.
  Making room (shrinking Windows) is done beforehand, in Windows.
- What is encrypted. The whole system (default), only a new /home
  partition, or nothing. With "/home only" the wizard says what stays
  readable: the system, its settings, the logs and `/var` with containers,
  virtual machines and databases; the recovery key and the unlock method
  apply to the /home volume, which opens after the system has started.
  Memory is never swapped to a disk (zram only).
- An existing /home. The wizard lists the LUKS volumes and Linux file
  systems of the machine (not on a disk that is erased), asks for the
  passphrase once and opens the volume read-only to list who owns its
  directories; the new user gets the same name, uid and gid and, by
  default, a directory of its own (`/home/<name>-basalt`, editable), so the
  other system's settings stay apart. The installation checks the
  passphrase before it writes anything, mounts the partition on /home
  without formatting it, writes crypttab (the passphrase is asked for at
  boot, or also the TPM when chosen; the passphrase slot stays) and fstab,
  creates only the new user's directory and labels only that directory
  (and the /home mount point) for SELinux: the other files keep their
  contents, owners and labels.

## What an installation does

The default plan generates about 100 steps; the golden preview in
`packages/basalt-installer/internal/steps/testdata/preview-default.txt`
lists them all. In order:

1. Disk: `sgdisk --zap-all`, `wipefs`, one `sgdisk` call for the GPT
   (EFI system partition 600 MiB, `/boot` 1 GiB, the rest for the system),
   `mkfs.vfat`, `mkfs.ext4`.
2. Encryption: a 64 character temporary key in memory, `cryptsetup
   luksFormat --type luks2`, `cryptsetup open`.
3. File systems: `mkfs.btrfs`, the subvolumes of the Basalt layout (the
   same as the kickstart's), every one mounted with `compress=zstd:1` so
   the system is written compressed.
4. Configuration read by package scripts: fstab, crypttab (with
   `tpm2-device=auto` when a TPM key is enrolled), `/etc/kernel/cmdline`
   (otherwise kernel-install would copy the live system's command line),
   `/etc/default/grub` (serial-first menu), machine ID, host name, locale,
   keymap, time zone, the Basalt repository URL.
5. Packages: `/dev`, `/proc`, `/sys` and a `/run` for the target, then one
   `dnf --installroot` transaction from the configured repositories:
   `@core`, the kernel, Fedora's signed shim and GRUB, the Basalt
   packages and the assistant, `dnf5-plugins` (so `dnf config-manager`
   works, as obpkg.org documents), without `fedora-release` and
   `fedora-logos`, and without firmware in the minimal profile. Packages
   and the Basalt repository metadata are signature checked.
6. System: SELinux enforcing, accounts and SSH keys, the SSH and network
   drop-ins, the `basalt-tools` URL (and an override when the plan turns
   it off) and the tui-tools repository, the Basalt
   boot splash, `multi-user.target` as the default target (what Anaconda
   sets for a server; systemd's own default is graphical), the same
   services the kickstart enables.
7. Boot loader: the ESP stub that points Fedora's signed GRUB at `/boot`,
   `grub2-mkconfig`, a firmware boot entry (the removable-media fallback
   path also holds Fedora's shim).
8. Unlock: `systemd-cryptenroll --tpm2-pcrs=7` (or Clevis for Tang), the
   optional passphrase, the recovery key, a check that slot 0 is the
   temporary key (token plugins off), then that slot is wiped and the key
   file deleted.
9. `dracut --regenerate-all`, `basalt-snapshots-setup
   --no-initial-snapshot` (no snapshot inside an installer; the first one
   is taken on the first boot), the install record, a full SELinux
   relabel with the target's own policy (`setfiles` in the target, given
   every subvolume, `/.snapshots` and `/boot` by name, because it does not
   cross into another file system on its own; append-only and immutable
   files lose that attribute for the relabel and get it back), then
   everything is unmounted and closed.

## The recovery key

It is shown once, by the frontend, grouped for reading, and the person
types its first group to confirm they stored it; only then can the
installer reboot. Before that, the person can write a copy to a USB stick:
ctrl+o in the text installer, "Save a copy to a USB stick" in the
graphical one. The file (`basalt-recovery-key-<host>-<time>.txt`, the key on
a line of its own and what it is for) goes to a writable file system on a
removable or USB disk, never to the installer media; a copy is not an
acknowledgement, the first group is still typed.

Nothing writes it to a disk otherwise. The exceptions are explicit: the
plan names a USB stick (`encryption.recovery_key_media`, written when the
installation has succeeded, and then no acknowledgement is asked), the plan
asks for the old behavior (`store_recovery_key`, the key next to what it
protects), or a non-interactive install names a file
(`--recovery-key-out`, which takes precedence). The engine forgets the key
after the acknowledgement.

The kickstart installer follows the same rule (`basalt.recovery-key=` in
`kickstart/basalt-server.ks`): by default the key is shown on the console
and the installation waits until its first group is typed there, and
`save` writes a copy to a USB stick first; `media:LABEL` writes it to a
USB stick without asking (checked before the disk is touched); `store`
(labs, CI) leaves it in `/root`.

## The live installer image

`packages/basalt-installer/live/` builds it with mkosi; `make live-iso`.

- mkosi installs a Fedora 44 system with `basalt-release`,
  `basalt-logos`, the installer, its GUI and the storage, TPM and Clevis
  tools into a directory, from Fedora's repositories and the signed
  Basalt repository.
- The tree becomes `LiveOS/squashfs.img`, an EROFS image whose SELinux
  labels come from the image's own policy (`mkfs.erofs --file-contexts`;
  the build host's labels play no part, and the build reads a few labels
  back from the image to check). A generic dracut initramfs with
  `dmsquash-live` finds the medium by its label, copies the image to
  memory (`rd.live.ram=1`) and boots it under a tmpfs overlay, the way
  Fedora's live media boot: the installer runs from memory and the medium
  can be removed.
- The ISO's EFI system partition image holds Fedora's signed shim (as the
  removable-media loader), GRUB and MokManager, taken unmodified from the
  tree; the kernel is Fedora's signed one. It boots with Secure Boot on
  and enrolls nothing. UEFI only, hybrid (optical or USB).
- The Basalt repository travels on the media (`/basalt/repo`); Fedora
  packages come from the network, as with the netinst.
- The serial console always gets the text installer. The screen gets the
  graphical installer when a display device exists, otherwise the text
  one; the boot menu can force either (`basalt.inst.ui=tui|gui`). The
  graphical installer runs as an unprivileged session user that may only
  talk to the engine's socket (checked with `SO_PEERCRED`); the engine
  runs as root. There is no root shell: root's password is locked and
  nothing logs it in. For debugging only, `basalt.inst.debug-shell` on the
  boot line logs root in on tty2 and on the second serial port (off by
  default, never on a release image's boot menu).
- The live system runs SELinux enforcing (targeted policy, `enforcing=1`
  on the command line: a live system that cannot load its policy does not
  boot). The engine, `/usr/bin/basalt-installer`, runs in `install_t`,
  the domain Fedora's policy gives Anaconda, bootc and rpm-ostree: it may
  write labels that only the installed system's policy knows (the
  assistant's types, for example), which a domain without `mac_admin`
  could not. There is no Basalt policy module for the installer (owner
  decision, 2026-10-04): the binary's `install_exec_t` label comes from a
  live-only `file_contexts.local` entry, and Fedora's policy starts it in
  `install_t` from a service (`init_t`) or a root login (`unconfined_t`).
  From any other domain (the systemd debug shell runs in `initrc_t`) the
  engine refuses to start and says to use `systemd-run --pty --wait`. The
  graphical session (cage and Quickshell) runs as an ordinary unconfined
  service of the unprivileged session user. The installed
  system is labeled with its own policy by `setfiles` and is enforcing
  from its first boot. Why not the earlier cpio-as-initramfs design: the
  kernel's initial root file system (`rootfs`) gets one fixed label for
  every file from the policy (`genfscon rootfs / root_t`), so a system
  running from it can only run with SELinux off.

Why mkosi and not lorax: lorax builds Anaconda's environment (or a
livemedia-creator live image through Anaconda itself), which is exactly
the stack this installer replaces; mkosi builds a plain system from
packages, declaratively, in minutes, and its directory output is all the
ISO needs. mkosi has no ISO output, so the script assembles the ISO with
xorriso around Fedora's signed boot files. Why not mkosi's own boot
loader and UKI: they would need our own Secure Boot signature (shim
review, ADR 0008); Fedora's signed chain needs none.

Hardware and the boot menu:

- Network firmware is on the image, so the network cards and Wi-Fi chips
  whose drivers load firmware work during the installation:
  `linux-firmware` (Broadcom NetXtreme, Chelsio, Intel ice, Marvell,
  Mellanox and other wired cards) and the vendor packages
  `realtek-firmware`, `iwlwifi-mvm-firmware`, `iwlwifi-mld-firmware`,
  `iwlwifi-dvm-firmware`, `atheros-firmware`, `brcmfmac-firmware`,
  `mt7xxx-firmware`, `nxpwireless-firmware` and `qed-firmware`, with
  NetworkManager's Wi-Fi support. `n` on the text installer's welcome
  screen opens `nmtui` to join a Wi-Fi network or set an address.
- The GRUB menu reads keys from the firmware console only: UEFI firmware
  that copies its console to a serial port (OVMF, BMC serial over LAN)
  passes the port's keys there, and GRUB reading the port as well garbled
  typed boot line edits. With the graphical theme on a screen, the screen
  shows it and the serial port the text menu; without it the text menu
  goes to the firmware console alone, because GRUB writing to the port as
  well showed every line twice on redirecting firmware. Firmware that does
  not redirect then shows the menu on the screen only; the default entry
  boots after the timeout and the installer runs on the serial console
  either way.

Kernel command line options of the live image: `basalt.inst.repo=URL`,
`basalt.inst.installed-repo=URL`, `basalt.inst.hostname=`,
`basalt.inst.unlock=`, `basalt.inst.finish=`, `basalt.inst.plan=PATH|URL`
(start from a plan file), `basalt.inst.confirm=DISK` (see below),
`basalt.inst.ui=auto|tui|gui`, `basalt.inst.debug-shell`. A plan at
`basalt/plans/default.yaml` on the media (`LIVE_PLANS` when building) is
the starting point when the boot line names none.

### A plan from the boot menu, and unattended installs

`basalt.inst.plan=` alone fills in the wizard: every screen shows the
plan's values (the welcome screen names the plan, or says why it could not
be read), and the person still reviews the steps and types the disk name.

An installation without questions needs the boot line to name the disk it
erases as well, the way a person types it in the wizard:

```
basalt.inst.plan=http://192.0.2.1/plans/web01.yaml basalt.inst.confirm=vda
```

Then the engine service (`basalt-installer serve`) loads the plan,
checks that `basalt.inst.confirm` names the plan's `target.disk`, makes the
preview and installs; the screen and the serial console show the text
installer following it (`basalt-installer tui --attach`). When the
confirmation names another disk, or the plan cannot be read or is not
valid, nothing is written: the wizard opens with the plan and says why.
Without `basalt.inst.plan`, the confirmation applies to the plan on the
media. The recovery key is still shown and must be acknowledged on one of
the consoles, unless the plan writes it to a USB stick
(`encryption.recovery_key_media`); then the plan's end action runs by
itself.

## The live desktop image

`LIVE_PROFILE=desktop` (`make live-desktop-iso`) builds a live system for
trying the desktop edition, from the same tooling: the installer image plus
the files in `packages/basalt-installer/live/desktop/`.

- Packages: `basalt-desktop` (the Basalt shell on SwayFX, the greetd login
  screen, portals, PipeWire, fonts, themes, foot, Files, Text Editor and
  Firefox) and the assistant, from the media repository, which must hold
  them (the shell is built in its own repository); Fedora packages come
  from Fedora's repositories with their updates, so the image ships the
  current ones (selinux-policy among them).
- A live user, `basalt` (password `basalt`, in `wheel`), starts the Basalt
  session at boot (greetd's initial session); logging out leads to the
  login screen. The graphical installer is an app of the session
  ("Install Basalt OS"): the engine's API service allows that user. The
  serial console still gets the text installer, tty2 a login prompt (no
  root shell).
- It runs from the medium (no `rd.live.ram`, a desktop image is too large
  to copy to memory on modest machines), boots `quiet`, and keeps SELinux
  enforcing and Fedora's signed boot chain like the installer image.
- The image stores every file as root (`mkfs.erofs --all-root`, so the
  build sandbox's ID mapping never reaches it); the build lists the
  packaged files that belong to other users (`/usr/lib/basalt-live/owners.list`)
  and `basalt-live-owners.service` gives those and the live home their
  owners back before anyone logs in. Only those few files are copied up
  into the overlay, which lives in memory.
- A plan at `basalt/plans/default.yaml` (from `live/desktop/plans/`, or
  `LIVE_PLANS`) with `edition: desktop`, `repos.testing: true` and
  `packages.extra: [basalt-desktop, ...]` makes the installer install the
  desktop edition.

## Building

```sh
make rpm-installer     # basalt-installer, basalt-installer-gui, source RPM
make installer-test    # go vet + go test in a container
make repo              # sign and publish (with the other packages)
make live-iso          # build/live/basalt-os-<version>-server-x86_64.iso
make live-desktop-iso  # build/live/basalt-os-<version>-desktop-x86_64.iso
```

Images are named as in [versioning.md](versioning.md):
`basalt-os-<version>-<edition>-<arch>.iso`, the version taken from
`VERSION`, `BASALT_STAGE` (default `dev`) and `BASALT_BUILD` (default
today, UTC), for example `basalt-os-44.0-dev.20261005-server-x86_64.iso`;
`LIVE_NAME` adds a suffix for test variants (`-lab`). Each build rewrites
`build/live/SHA256SUMS` over the images of that version.

`LIVE_SOURCE=obpkg` builds from the published repositories instead of
`REPO_DIR`: `scripts/release/mirror-published.sh` mirrors
https://obpkg.org/basalt (and, for the desktop, basalt-testing)
byte-identical after checking the metadata signature and every checksum,
mkosi installs from that mirror with `gpgcheck` and `repo_gpgcheck`, and
the media carries it as `basalt/repo` and `basalt/testing`. Such an image
holds only packages signed with the OpenBasalt release key:

```sh
LIVE_SOURCE=obpkg make live-iso
LIVE_SOURCE=obpkg make live-desktop-iso
```

The live menu needs `basalt-grub2-theme` 0.2.0 or later in the
repository: 0.1.0 looked for the theme next to GRUB on the EFI image and
loaded font files, which Fedora's signed GRUB refuses under Secure Boot,
so the screen showed the plain text menu. The build stops when the image
has no theme or an older one.

Go toolchain: the text installer is built on
[tui-kit](https://github.com/tui-tools/tui-kit) as a module dependency, so
it shares the preview, confirm and run look of every tui-tools program.
tui-kit needs Go 1.27; Fedora 44 ships 1.26. `go.mod` names the toolchain
(`toolchain go1.27.1`) and `build.sh` puts that official release, checked
against a pinned SHA-256, first in the container's PATH. `go mod vendor`
(verified against `go.sum`) makes the vendor tarball that is the RPM's
second source; rpmbuild runs offline (`GOPROXY=off`, `GOTOOLCHAIN=local`).
Nothing is vendored into the repository.

## Testing

- Unit tests: plan validation (every rule), step generation (order,
  exact commands, the golden preview, secrets never in the preview, the
  pinned tui-tools key), the engine (rollback in reverse, cancellation,
  optional steps, the lock, the log chain, secrets scrubbed), the API
  (peer checks aside: stale tokens, the typed confirmation, the recovery
  key acknowledgement before the reboot, history without the key).
- Lab (`make lab-installer-iso lab-installer-tui lab-installer-gui`): a
  QEMU/KVM VM with Secure Boot (Microsoft and Red Hat keys) and swtpm boots
  the live ISO; the text installer is driven over the serial console like
  a person (every screen, the review, the typed disk name, the recovery
  key read off the screen and acknowledged); the graphical installer is
  clicked through with QMP and photographed page by page. Then the
  kickstart boot test's checks (`scripts/ci/vm-test.sh`,
  `INSTALL_MODE=external`) run on the installed disk: unattended TPM
  unlock, os-release, the profile, Secure Boot, SELinux enforcing with 0
  denials, LUKS slots and PCR 7, snapper around dnf, the rollback dry
  run, the assistant, sealed audit rotation, a reboot. Before the end
  action, the drivers read the live system's own SELinux state through
  the lab ISO's debug shell (`basalt.inst.debug-shell`,
  `tests/liveshell.py`): it must be enforcing with 0 AVC denials for the
  whole installation, or the test fails.

## Translations

User-facing text goes through a translation catalog; English is the
reference language. The text installer looks strings up with `i18n.T`
(`internal/i18n`: GNU gettext `.mo` catalogs, domain `basalt-installer`,
in `/usr/share/locale/<lang>/LC_MESSAGES/`, language from `LANGUAGE`,
`LC_ALL`, `LC_MESSAGES` or `LANG`); the graphical installer marks strings
with `qsTr`. Each message is a whole sentence, never pieces joined
together. Logs, the audit log and the plan format stay in English.

`i18n.T` and `i18n.N` (plural forms) take literal English text;
`po/basalt-installer.pot` is extracted from those calls by a test
(`go test ./internal/i18n -update` rewrites it), and another test checks
that every `po/<lang>.po` translates every message with the same
placeholders. The package compiles the catalogs with `msgfmt`. Brazilian
Portuguese is the first translation. New and changed texts go through the
catalog (0.1.3: the welcome text; 0.2.0: the recovery key copy, the
unattended mode, the summary of `install --plan` and the texts that were
reworded to fit 80 columns). Still to do: the remaining strings of both
front ends, a `QTranslator` loaded by the graphical installer, and the
engine's step titles and the plan's validation messages that both front
ends show.

## Complex storage

RAID, multipath, iSCSI, a system over several disks, MBR disks or LVM:
use the kickstart installer and write the storage section yourself
(see `kickstart/basalt-server.ks` and Anaconda's kickstart reference). The
Basalt parts (encryption enrollment, snapshots setup, services) are in its
`%post`.

## Not yet

- Offline installs (Fedora packages on the media).
- Joining a Wi-Fi network from the graphical installer (the text
  installer opens `nmtui`).
- Resizing another system's partition; using a chosen region other than
  the largest free one; BIOS boot.
- A desktop package set for `edition: desktop`.
- The tui-tools repository file moves into `basalt-third-party` once it
  exists (`basalt-tools` is already in `basalt-release`); the installer
  then only enables or disables it.
- Translations and keyboard layouts in the frontends.
