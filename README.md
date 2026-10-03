# Basalt OS

> Pre-alpha. Not for production use. Basalt OS is based on Fedora and is not
> affiliated with or endorsed by the Fedora Project or Red Hat. Expect breaking
> changes, rebuilt history and missing pieces.

Basalt OS is a Linux distribution for servers built as a bootable container
image on top of the Fedora bootc base image. The system is delivered and
updated as a whole image, signed, with one-command rollback. Security defaults
are on from the first boot: SELinux enforcing, encrypted disk unlocked by the
TPM only while the boot chain is unchanged, SSH with keys only, a host
firewall.

The design behind these choices is summarized in [docs/design.md](docs/design.md).
Milestone 0 (this repository's first working state) is reported in
[docs/milestone-0-report.md](docs/milestone-0-report.md).

## What the image contains

| Area | Default |
|---|---|
| Base | `quay.io/fedora/fedora-bootc:44` (Fedora packages, Fedora kernel and signed boot chain) |
| Identity | `/usr/lib/os-release`: `NAME="Basalt OS"`, `ID=basalt`, `ID_LIKE=fedora`; `generic-release` replaces the Fedora branding packages |
| SELinux | enforcing, targeted policy, checked at build time |
| Storage | btrfs root with zstd compression; `basalt-install` puts it on LUKS2 with TPM2 unlock bound to PCR 7 and a recovery key |
| SSH | public key only; root only with a key; no X11 or agent forwarding (`/etc/ssh/sshd_config.d/10-basalt-hardening.conf`) |
| Firewall | firewalld on, default zone `public`, only SSH allowed in |
| Audit | auditd on, so SELinux denials are recorded and searchable with `ausearch` |
| Tools | [tui-tools](https://tui.tools) for systemd, logs, firewall, disks, network, SSH, users and more, from their signed repository (key fingerprint pinned at build time) |
| Updates | `bootc upgrade` from the image's registry, signature required by `/etc/containers/policy.json` when a signing key is configured; `bootc rollback` |
| Locale | `en_US.UTF-8`, boots to `multi-user.target`, kernel console on tty0 and ttyS0 |

## Requirements

On the build host: podman (rootful, through `sudo podman` by default), skopeo,
openssl, make. The lab additionally needs libvirt with KVM, swtpm, the OVMF
(edk2) firmware with Secure Boot variable stores, and docker or podman to run
a registry. Builds have been run on Fedora 44.

## Configure

```sh
cp env.example .env     # then edit: registry, lab paths, VM settings
```

`.env` is ignored by git; it is where site-specific values live (registry
address, paths, user names). Nothing in this repository depends on a
particular host. The image version comes from the `VERSION` file and can be
overridden per run: `make build BASALT_VERSION=0.0.2`.

## Build, sign, publish

```sh
make build      # podman build into local storage
make publish    # push, sign the pushed digest with cosign, move the channel tag
make verify     # cosign verify, then a pull with the image's own signature policy
make release    # all three
```

`scripts/publish.sh` pushes to any OCI registry the engine can log in to. It
signs by digest with a cosign key pair (`COSIGN_KEY`, `COSIGN_PASSWORD_FILE`)
and then points `CHANNEL_TAG` (default `stable`) at that digest, so a tag
never moves to an unsigned image. When `SIGNING_PUBKEY` is set, the build
bakes that public key into the image together with a containers policy that
requires it for this image's repository, so installed systems refuse unsigned
updates. Details and the key handling rules are in [docs/signing.md](docs/signing.md).

Public builds run in GitHub Actions ([.github/workflows/image.yml](.github/workflows/image.yml)):
they push to `ghcr.io/basalt-os/basalt-os` and sign keyless with Sigstore, so
no signing key exists to manage. Verify such an image with:

```sh
cosign verify ghcr.io/basalt-os/basalt-os:stable \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/basalt-os/basalt-os/\.github/workflows/image\.yml@'
```

## Install

`basalt-install` ships in the image. Run it from the image, on the machine
being installed, because the disk is sealed to that machine's TPM:

```sh
sudo podman run --rm --privileged --pid=host \
  --security-opt label=type:unconfined_t \
  -v /var/lib/containers:/var/lib/containers -v /dev:/dev \
  REGISTRY/basalt-os:stable \
  basalt-install /dev/DISK -- \
    --target-imgref REGISTRY/basalt-os:stable \
    --enforce-container-sigpolicy \
    --root-ssh-authorized-keys /path/inside/container/authorized_keys \
  > recovery-key.txt
```

The disk gets an EFI system partition, a `/boot` partition and a LUKS2
partition holding the btrfs root (zstd). The LUKS2 volume has two key slots: a
TPM2 key bound to PCR 7 (the Secure Boot state), which unlocks it with no
interaction while the firmware's Secure Boot configuration is unchanged, and a
recovery key, which is the only output of the command. Store it safely; if
Secure Boot is turned off, keys change or the TPM is cleared, the boot stops at
a prompt that accepts it. `--no-encryption` installs plain btrfs.

Plain `bootc install to-disk` and bootc-image-builder also work with this
image and default to btrfs without encryption.

## Update and roll back

```sh
bootc upgrade          # fetch and stage the new image (signature checked)
systemctl reboot       # boot into it
bootc rollback         # make the previous image the default again
systemctl reboot
bootc status           # what is booted, staged and available for rollback
```

## Lab

The lab runs on one Linux host: a private registry (TLS from a lab CA,
htpasswd), a development signing key, and a VM with Secure Boot firmware and an
emulated TPM 2.0. See [docs/lab.md](docs/lab.md).

```sh
make lab-tools lab-keys lab-registry   # once
make release                           # build, sign, publish
scripts/lab/e2e-install.sh             # VM: installer disk, basalt-install, first boot
make lab-measure                       # size, boot time, memory, SELinux denials
scripts/lab/sb-test.sh                 # Secure Boot state vs TPM2 unlock, recovery key
NEW_VERSION=0.0.2 scripts/lab/update-test.sh   # upgrade, unsigned refused, rollback
```

## Layout

```
Containerfile           the image
image/build.sh          steps run inside the build (branding, packages, services, checks)
packages/               package lists
rootfs/                 files copied into the image
scripts/                build, publish (push + sign), verify, site overlay
scripts/lab/            lab registry, keys, VM, install, measurements
docs/                   design, signing, lab, milestone reports
```

## License

Apache License 2.0, see [LICENSE](LICENSE). The image includes software from
Fedora and other projects under their own licenses.
