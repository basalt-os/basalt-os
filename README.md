# Basalt OS

[![CI](https://github.com/basalt-os/basalt-os/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/basalt-os/basalt-os/actions/workflows/ci.yml)

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
| Identity | `basalt-release` replaces `fedora-release` (`ID=basalt`, `ID_LIKE=fedora`, `VERSION_ID` = the Fedora release so tools that key on it behave as on Fedora, `VERSION="44 (Basalt 0.0.1)"`, `BASALT_VERSION`, `BUILD_ID`); `basalt-logos` replaces `fedora-logos` |
| SELinux | enforcing, targeted policy |
| Disk | btrfs with zstd compression on LUKS2; TPM2 unlock bound to the Secure Boot state (PCR 7), or network unlock through Tang, or both, and always a recovery key; encryption can be turned off at install |
| Boot | Secure Boot with Fedora's signed chain; Basalt's kernel module CA can be enrolled as a MOK, or the firmware can trust only Basalt's own keys; kernel lockdown and module signature enforcement on the command line (modules built with DKMS or akmods are signed with your own MOK, see [docs/secure-boot.md](docs/secure-boot.md)) |
| Layout | the system in one subvolume; `/home`, `/srv`, `/var/log`, `/var/cache`, `/var/tmp`, `/var/spool` and the container, VM and database directories in their own subvolumes, so a rollback never touches data |
| Snapshots | snapper pre/post snapshots around every dnf transaction (libdnf5 actions plugin), retention policy, snapshots bootable from the GRUB menu, `basalt-rollback` |
| SSH | public keys only; root only with a key |
| Firewall | firewalld on, zone `basalt`: only SSH allowed in |
| Assistant | `basalt-assistant` installed, its confined daemon on: it diagnoses events and proposes fixes, never applies them; findings in the journal, desktop notifications where a graphical session exists, an optional signed webhook; hash-chained audit log with sealed rotation. The local language model is optional and not installed |
| Egress and audit | `basalt-resolver` on: every confined agent session is default-deny in the kernel, with DNS-aware allowlists; `basalt-ledger` on: one append-only, hash-chained audit trail (agents, network decisions, SELinux denials, escalations, rollbacks, logins), plain-English views, exports signed with a TPM-held key, sealed files kept for a year |
| Packages | minimal profile (no hardware firmware or microcode) on virtual machines, standard on bare metal, picked by the installer |
| Other | auditd on; LLMNR and multicast DNS off; serial console first (GRUB and kernel) |

How it works and why: [docs/design.md](docs/design.md),
[docs/secure-boot.md](docs/secure-boot.md) and, for the system assistant
(`basalt status`, `basalt why`, `basalt fix selinux`, proposals that are
applied only after confirmation), [docs/assistant.md](docs/assistant.md); its optional
local language model: [docs/local-model.md](docs/local-model.md). Running AI
coding agents confined by SELinux (container or native, per-session egress
allowlist and audit): [docs/agents.md](docs/agents.md). Current state and measurements:
[docs/milestone-1-report.md](docs/milestone-1-report.md) (and
[milestone 0](docs/milestone-0-report.md)).

## Layout

```
packages/basalt-release/     release identity, repository, presets, server defaults (spec + sources)
packages/basalt-logos/       branding: logos, icons, Plymouth and GRUB themes (spec + artwork tree)
packages/basalt-snapshots/   snapper config, dnf5 hook, snapshot boot menu, setup and rollback tools
packages/basalt-security/    basalt-tpm and basalt-secureboot: TPM2 unlock, MOK and module signing
packages/basalt-assistant/   the system assistant: basalt CLI, basalt-assistantd, basalt-mcp, basalt-notify, SELinux module (Go)
packages/basalt-agent/       run AI coding agents (Claude Code, Codex, Gemini, Aider) confined by SELinux: container and native modes, per-session egress allowlist and audit (Go)
packages/basalt-resolver/    per-session default-deny egress: DNS-aware nftables sets by cgroup, own resolver per session (Go)
packages/basalt-ledger/      the audit ledger: append-only hash chain, collectors, plain-English views, TPM-signed exports (Go)
packages/basalt-llm/         optional local model service: llama.cpp server for the CPU, no network, own SELinux domain
packages/basalt-shell/       desktop shell build, from github.com/basalt-os/basalt-shell at a pinned commit (basalt-testing)
eval/                        shared evaluation suite: labeled decision cases, translator test set, generators
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
`basalt-release` ship its public half. Without it, `basalt-release` ships the
OpenBasalt release key (fingerprint `3601734842BD4E482D19DE4AE4EED5ECA395B302`,
public key at https://obpkg.org/keys/openbasalt-release-key.asc), which only
verifies repositories signed on the release signer.

CI (GitHub Actions, `.github/workflows/ci.yml`) runs the same steps on every
push and pull request: `make ci-lint` (ShellCheck, rpmlint, ksvalidator,
actionlint), `make ci-build` (packages, repository, ISO, checksums) and
`make ci-boot-test` (an unattended install in QEMU/KVM with Secure Boot and an
emulated TPM, then smoke checks; skipped when the runner has no KVM). CI holds
no keys: the packages, repository and ISO it publishes as artifacts are
unsigned test builds, and the boot test signs its own repository with a
throwaway key that exists for one run. Release signing happens outside CI,
see [docs/key-ceremony.md](docs/key-ceremony.md).

## Install

Boot the ISO (UEFI, Secure Boot can stay on: the Fedora boot chain is
unchanged). The kickstart installs a minimal server:

- encrypted by default; add `basalt.encrypt=0` to the boot entry for plain btrfs;
- `basalt.unlock=tang basalt.tang=URL basalt.tang-thp=THUMBPRINT` (or
  `tpm2+tang`) for network unlock through a Tang server;
- `basalt.profile=auto` (default) installs the minimal profile (no hardware
  firmware, CPU microcode or fwupd) on a virtual machine or cloud instance
  and the standard one on bare metal; `basalt.profile=standard` or
  `minimal` forces one (a VM with passthrough hardware that needs firmware
  wants `standard`);
- `basalt.disk=sdX` to pick the disk (default: the first fixed disk; it is wiped);
- the installer asks for a root password or a user unless the ISO carries a
  site file with accounts (`SITE_DIR`, see `scripts/lab/keys.sh` for an example).

On machines with a TPM 2.0 the disk unlocks at boot without interaction while
the Secure Boot configuration is unchanged. A recovery key is generated, shown
on the console and stored in `/root/basalt-recovery-key.txt`: copy it off the
machine and delete the file. If Secure Boot is turned off, its keys change or
the TPM is cleared, the boot stops at a prompt (also on the serial console)
that accepts the recovery key; `basalt-tpm reenroll` then seals the TPM key
to the new state. Before a planned change, `basalt-tpm suspend` lets the
next boot unlock unattended and re-seal by itself.

## Update and roll back

```sh
dnf upgrade                       # snapshots before and after, automatically
snapper list                      # the snapshots
basalt-rollback 42                # make snapshot 42 the new root, then reboot
basalt-rollback --kernels         # kernels on /boot; --clean-kernels removes orphaned ones
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
make lab-assistant-test  # system assistant: events, proposals, confirmed applies, rollback
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
