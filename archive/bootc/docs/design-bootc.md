# Basalt OS design

This page summarizes the decisions that shape Basalt OS. It describes intent;
what is already implemented is tracked in the milestone reports.

## Goals

A Linux distribution that is secure by default, light, and able to explain and
repair itself. Servers come first. A desktop edition may follow later on the
same foundations.

## Base: Fedora, as bootable container images

Basalt OS is built from Fedora's packages through the Fedora bootc base image.
Reasons:

- SELinux enforcing is Fedora's default and tested configuration, and the
  targeted policy is developed together with the packages, so new services
  arrive labeled and confined.
- btrfs is Fedora's default file system and is in the mainline kernel.
- bootc delivers the whole operating system as an OCI image, updated
  atomically, with rollback to the previous image.
- Security fixes come from a large, active project.

Fedora's release cycle (about 13 months of support per release) is accepted
because every update is a complete image that is built and tested before it
is published. Basalt OS uses its own name and logo and no Fedora marks; it says
"based on Fedora" and nothing more.

## Security defaults

- SELinux enforcing, always. Denials are treated as bugs: an image that
  produces SELinux denials on boot or on update does not ship.
- Secure Boot. Today the image boots through Fedora's signed boot chain. The
  plan is to support booting with keys owned by the machine's operator, and
  signed kernel modules.
- Minimal attack surface: a small server package set, SSH with public keys
  only, a host firewall that allows only SSH by default.

## Storage and encryption

- Root file system: btrfs with zstd compression.
- Full disk encryption with LUKS2, on by default, with a clear option to turn
  it off at install time.
- A recovery key is always generated at install time and handed to the
  operator.
- Unlock without a person at the console, for servers:
  - TPM2, sealed to the Secure Boot state (PCR 7), so a changed boot chain
    does not unlock the disk;
  - or network-bound unlock with Clevis and Tang, for sites that run a Tang
    server.
- Snapshots of data before risky operations are an orchestration feature on
  top of btrfs; the operating system itself rolls back through images, not
  through file system snapshots.

## Updates and rollback

Updates are whole images pulled from a registry. `bootc upgrade` stages the new
image, a reboot activates it, and `bootc rollback` returns to the previous one.
A failed update never leaves a system half updated.

## Signed artifacts

Everything users download is signed, so the place it is downloaded from does
not need to be trusted:

- images signed with cosign (Sigstore), with a signature policy enforced on
  update where the key can be expressed in the system's policy;
- packages of our own, when they exist, signed with GPG, with repository
  metadata signatures checked;
- installation media published with signed checksums.

Images are distributed through public container registries. Basalt OS serves
only what is its own; everything else comes from Fedora's infrastructure.

## Tools: preview, confirm, run

System changes go through tools that show the exact command before running it
and ask for confirmation ([tui-tools](https://tui.tools)). The same pattern is
the interface for the assistant below.

## Planned: a local assistant

A small model that runs on the machine itself, offline and on CPU, specialized
in the operating system: its services, logs, SELinux, storage and network. It
never acts directly: it diagnoses through read-only tools and proposes changes
that a person previews and confirms, with an audit log. The assistant runs in
its own SELinux domain. Users may plug in larger local or remote models by
their own choice; remote use is opt-in and shows what leaves the machine.

## Not in scope for now

- ZFS (licensing and out-of-tree module signing).
- A block or snapshot layer of our own.
- Own mirrors: public registries and object storage with a CDN come first.
