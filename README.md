# Basalt OS

> Pre-alpha. Not for production use. Basalt OS is based on Fedora and is not
> affiliated with or endorsed by the Fedora Project or Red Hat. Expect
> breaking changes, rebuilt history and missing pieces.

Basalt OS is a Linux distribution for servers, built as a Fedora remix: a
normal, package-based Fedora install that you update with `dnf`, with its
own identity and secure defaults, and a btrfs snapshot before and after every
package transaction so that any update can be rolled back, from the running
system or from the boot menu. What you install stays installed.

Defaults from the first boot:

| Area | Default |
|---|---|
| Base | Fedora 44 packages from Fedora's own mirrors; only Basalt's few packages come from the Basalt repository |
| Identity | `basalt-release` replaces `fedora-release` (`ID=basalt`, `ID_LIKE=fedora`); `basalt-logos` replaces `fedora-logos` |
| SELinux | enforcing, targeted policy |
| Disk | btrfs with zstd compression on LUKS2; TPM2 unlock bound to the Secure Boot state (PCR 7) and a recovery key; encryption can be turned off at install |
| Layout | the system in one subvolume; `/home`, `/srv`, `/var/log`, `/var/cache`, `/var/tmp`, `/var/spool` and the container, VM and database directories in their own subvolumes, so a rollback never touches data |
| Snapshots | snapper pre/post snapshots around every dnf transaction (libdnf5 actions plugin), retention policy, snapshots bootable from the GRUB menu, `basalt-rollback` |
| SSH | public keys only; root only with a key |
| Firewall | firewalld on, zone `basalt`: only SSH allowed in |
| Other | auditd on; LLMNR and multicast DNS off; serial console first (GRUB and kernel) |

How it works and why: [docs/design.md](docs/design.md). Current state and
measurements: [docs/milestone-0-report.md](docs/milestone-0-report.md).

## Layout

```
packages/basalt-release/     release identity, repository, presets, server defaults (spec + sources)
packages/basalt-logos/       branding: logos, icons, Plymouth and GRUB themes (spec + artwork tree)
packages/basalt-snapshots/   snapper config, dnf5 hook, snapshot boot menu, setup and rollback tools
packages/lab/                test fixtures for the lab (never published)
kickstart/basalt-server.ks   the installer profile
scripts/                     build-rpms.sh, repo.sh (signed repository), iso.sh (installer ISO)
scripts/lab/                 local VM lab: keys, network, repository server, install and tests
docs/                        design, lab, milestone reports
archive/bootc/               an earlier image-based (bootc) prototype, kept for reference
```

## Build

Needs a Linux host with podman (rootful, `sudo podman` by default) and make.
Everything else runs in Fedora containers.

```sh
cp env.example .env      # then edit: paths, lab network, signing key location
make rpms                # build the packages in a Fedora 44 container
make repo                # sign the packages and the repository metadata
make iso-fetch           # download the Fedora netinst ISO, verify its signed checksums
make iso                 # build the Basalt OS installer ISO
```

`make repo` needs a signing key. For development, `make lab-keys` creates one
under `LAB_DIR` (mode 0600, never printed); `BASALT_GPG_PUBKEY` makes
`basalt-release` ship its public half. Without it, `basalt-release` carries a
placeholder and cannot verify the repository. There is no release key yet.

## Install

Boot the ISO (UEFI, Secure Boot can stay on: the Fedora boot chain is
unchanged). The kickstart installs a minimal server:

- encrypted by default; add `basalt.encrypt=0` to the boot entry for plain btrfs;
- `basalt.disk=sdX` to pick the disk (default: the first fixed disk; it is wiped);
- the installer asks for a root password or a user unless the ISO carries a
  site file with accounts (`SITE_DIR`, see `scripts/lab/keys.sh` for an example).

On machines with a TPM 2.0 the disk unlocks at boot without interaction while
the Secure Boot configuration is unchanged. A recovery key is generated, shown
on the console and stored in `/root/basalt-recovery-key.txt`: copy it off the
machine and delete the file. If Secure Boot is turned off, its keys change or
the TPM is cleared, the boot stops at a prompt (also on the serial console)
that accepts the recovery key.

## Update and roll back

```sh
dnf upgrade                       # snapshots before and after, automatically
snapper list                      # the snapshots
basalt-rollback 42                # make snapshot 42 the new root, then reboot
```

If an update leaves the system unbootable or unreachable, pick "Basalt OS
snapshots" in the GRUB menu and boot the snapshot taken before that update.
It starts read-only with your data mounted; `basalt-rollback` keeps it, and
the next reboot is back to normal. Release upgrades use `dnf system-upgrade`
(see the design document).

## Lab

A local lab installs the ISO into a VM with UEFI Secure Boot firmware and an
emulated TPM 2.0 and runs the tests. See [docs/lab.md](docs/lab.md).

```sh
make lab-tools lab-keys rpms-lab repo iso   # once
make lab-install         # unattended install, first boot, recovery key saved
make lab-snapshot-test   # snapshots around dnf, packages persist
make lab-rollback-test   # broken updates, rollback from the system and from GRUB
make lab-sb-test         # Secure Boot changes block the TPM unlock; the recovery key works
make lab-measure
```

## License

Code (specs, scripts, kickstart, configuration): Apache License 2.0, see
[LICENSE](LICENSE). The systemd preset files in `packages/basalt-release`
come from Fedora's `fedora-release` package under the MIT license
(`packages/basalt-release/LICENSE.presets`). Artwork in
`packages/basalt-logos` (logos, icons, themes, wallpapers): CC-BY-SA-4.0,
see `packages/basalt-logos/COPYING-artwork` (draft terms). The installed
system contains Fedora software under its own licenses. "Fedora" is a
trademark of Red Hat; Basalt OS uses no Fedora marks.
