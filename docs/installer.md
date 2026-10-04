# Basalt OS installer

Status: pre-alpha, version 0.1.0. The kickstart installer
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
  from `systemd-cryptenroll --recovery-key` and goes to the frontend once.
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
| `target.disk`, `target.wipe` | | the whole disk; `wipe: true` is required |
| `layout.mode` | `automatic` | `manual`: `esp_mib`, `boot_mib`, `root_gib` (0 = rest), `subvolumes` (root, home and var_log required) |
| `encryption.enabled` | `true` | LUKS2 under btrfs (ADR 0002) |
| `encryption.unlock` | `tpm2` | `tpm2` (sealed to PCR 7), `tang`, `tpm2+tang`, `recovery-only`; `tpm2_pcrs` must be `[7]` |
| `encryption.passphrase` | none | an extra key slot typed at boot (desktops) |
| `encryption.store_recovery_key` | `false` | `true` also leaves the key in `/root/basalt-recovery-key.txt`, as the kickstart does |
| `profile` | `auto` | `minimal` on a virtual machine, `standard` on bare metal (same rule as the kickstart) |
| `hostname`, `timezone`, `locale`, `keymap` | `basalt`, `Etc/UTC`, `en_US.UTF-8`, `us` | |
| `lockdown` | `true` | `lockdown=integrity module.sig_enforce=1` on the kernel command line |
| `accounts.root` | locked | `ssh_keys`, `password` or `password_hash` |
| `accounts.user` | none | `name`, `password` or `password_hash`, `ssh_keys`, `admin` (wheel, default true) |
| `ssh.password_auth` | `false` | Basalt OS accepts public keys only; `true` adds a drop-in and a warning |
| `network.mode` | `dhcp` | `static`: `interface`, `address` (CIDR), `gateway`, `dns` |
| `repos.basalt.url` | `media` | the repository on the installer image, or an http(s) URL; `installed_url` is what the installed system uses |
| `repos.fedora.baseurl`, `updates_baseurl` | Fedora's mirrors | for a local mirror |
| `repos.tools` | `true` | `basalt-tools` configured, metadata only (ADR 0005) |
| `repos.third_party.tui_tools` | `true` | the tui-tools repository with its key; the key's fingerprint is pinned in the installer and checked before it is written |
| `assistant` | `true` | `basalt-assistant` installed and its daemon enabled |
| `packages.extra` | none | more packages |
| `finish` | `reboot` | `reboot`, `poweroff`, `none` |

Validation reports errors (blocking) and warnings (shown in the review).
It rejects what this version does not handle and points to the kickstart:
partitions, RAID members and md devices, multipath, iSCSI, device-mapper
devices, disks in use, BIOS boots, disks under 16 GiB.

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
   packages and the assistant, without `fedora-release` and
   `fedora-logos`, and without firmware in the minimal profile. Packages
   and the Basalt repository metadata are signature checked.
6. System: SELinux enforcing, accounts and SSH keys, the SSH and network
   drop-ins, the `basalt-tools` and tui-tools repositories, the Basalt
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
   cross into another file system that the kernel does not mark as
   labeled, and the live system runs without SELinux; append-only and
   immutable files lose that attribute for the relabel and get it back),
   then everything is unmounted and closed.

## The recovery key

It is shown once, by the frontend, grouped for reading, and the person
types its first group to confirm they stored it; only then can the
installer reboot. Nothing writes it to disk unless the plan asks
(`store_recovery_key`) or a non-interactive install names a file
(`--recovery-key-out`). The engine forgets it after the acknowledgement.

## The live installer image

`packages/basalt-installer/live/` builds it with mkosi; `make live-iso`.

- mkosi installs a Fedora 44 system with `basalt-release`,
  `basalt-logos`, the installer, its GUI and the storage, TPM and Clevis
  tools into a directory, from Fedora's repositories and the signed
  Basalt repository.
- The tree becomes one zstd compressed cpio archive that the kernel
  unpacks as its initramfs, with systemd as its first process: the
  installer runs entirely from memory, needs no live root file system and
  no initramfs logic of its own, and the medium can be removed.
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
  runs as root. tty2 has a root shell, like Anaconda's.
- The live system boots with `selinux=0`: it runs from an unlabeled
  memory file system that holds no data and offers no network service,
  and the installed system is labeled with its own policy by `setfiles`.
  The installed system is enforcing from its first boot.

Why mkosi and not lorax: lorax builds Anaconda's environment (or a
livemedia-creator live image through Anaconda itself), which is exactly
the stack this installer replaces; mkosi builds a plain system from
packages, declaratively, in minutes, and its directory output is all the
ISO needs. mkosi has no ISO output, so the script assembles the ISO with
xorriso around Fedora's signed boot files. Why not mkosi's own boot
loader and UKI: they would need our own Secure Boot signature (shim
review, ADR 0008); Fedora's signed chain needs none.

Kernel command line options of the live image: `basalt.inst.repo=URL`,
`basalt.inst.installed-repo=URL`, `basalt.inst.hostname=`,
`basalt.inst.unlock=`, `basalt.inst.finish=`, `basalt.inst.plan=PATH|URL`
(start from a plan file), `basalt.inst.ui=auto|tui|gui`. A plan at
`basalt/plans/default.yaml` on the media (`LIVE_PLANS` when building) is
the starting point; the person still reviews it and types the disk name.

## Building

```sh
make rpm-installer     # basalt-installer, basalt-installer-gui, source RPM
make installer-test    # go vet + go test in a container
make repo              # sign and publish (with the other packages)
make live-iso          # build/live/basalt-os-<version>-x86_64-live.iso
```

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
  run, the assistant, sealed audit rotation, a reboot.

## Complex storage

RAID, multipath, iSCSI, several disks, existing partitions to keep or
LVM: use the kickstart installer and write the storage section yourself
(see `kickstart/basalt-server.ks` and Anaconda's kickstart reference). The
Basalt parts (encryption enrollment, snapshots setup, services) are in its
`%post`.

## Not yet

- Offline installs (Fedora packages on the media).
- Hardware firmware in the live image (some network cards need it).
- Several disks, existing partitions, dual boot; BIOS boot.
- A desktop package set for `edition: desktop`.
- The `basalt-tools` and tui-tools repository files move into
  `basalt-release` and `basalt-third-party` once those exist; the
  installer then only enables or disables them.
- Translations and keyboard layouts in the frontends.
