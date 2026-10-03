# Milestone 0 report

Date: 2026-10-03. Status: done, with the gaps listed at the end.

Direction change after this milestone: Basalt OS will be a traditional
Fedora-based distribution (dnf, packages persist on the installed system),
with btrfs snapshots before every update (snapper and a dnf plugin) and
rollback by booting a snapshot from GRUB. Disk encryption stays: LUKS2 with
TPM2. This milestone was built on bootc, so this report is now mainly a
findings report: the next section lists what carries over to the traditional
model and what was specific to bootc. The detailed results follow.

## Findings: what carries over, what does not

Reusable in the traditional model, validated here:

| Topic | Result | Reuse |
|---|---|---|
| SELinux enforcing, targeted | 0 AVC, USER_AVC or SELINUX_ERR records after first boot, recovery-key boots, update and rollback | same policy; keep the zero-denials gate in CI |
| Denial found | `bootupd_t` reading `/proc/swaps` and `/proc` via `lsblk` (7 records per first boot) | bootupd is a bootc component; the CIL module pattern (`selinux/*.cil` + `semodule -i`) carries over for any future denial |
| SSH hardening | `rootfs/etc/ssh/sshd_config.d/10-basalt-hardening.conf`, effective values asserted with `sshd -T` | as is, shipped by a basalt-release (or similar) package |
| Firewall | firewalld, zone `public` with only `ssh` (`rootfs/etc/firewalld/zones/public.xml`) | as is |
| Branding | `generic-release` swaps out `fedora-release*`; Basalt `os-release`; logo; initramfs regenerated so the initrd shows Basalt | as is; on a package-based system this becomes a `basalt-release` RPM that conflicts with `fedora-release*` instead of overwriting files |
| LUKS2 + TPM2 bound to PCR 7 | `systemd-cryptenroll --tpm2-device=auto --tpm2-pcrs=7`; unattended unlock; Secure Boot disabled or a foreign `db` certificate blocks the unlock; restoring the state restores it | identical with Anaconda/kickstart (`autopart --encrypted`) followed by enrollment; note systemd 259 binds no PCRs unless `--tpm2-pcrs` is given |
| Recovery key | `systemd-cryptenroll --recovery-key`, typed at `Please enter recovery key for disk root` on the serial console | as is |
| Prompt on TPM refusal | requires `luks.options`/crypttab without `headless=true` | as is |
| Lab: OVMF Secure Boot + swtpm | libvirt VM, `virt-fw-vars` to flip Secure Boot or add a `db` certificate, scripted console unlock (`scripts/lab/vm.sh sb ...`, `sb-test.sh`, `console-unlock.py`) | as is for any install method |
| Lab host workarounds | vTPM state created before first start (Fedora 44 swtpm SELinux denial); builds on the host network (DNS) | as is |
| tui-tools from the signed RPM repository, key fingerprint pinned | installs and runs | as is (`dnf install`, updates now come from the repository) |
| Measurements | idle memory 445 to 460 MiB in a 4 GiB VM; boot 11.4 to 12.1 s (17.4 s first boot); top RSS firewalld 53 MiB | a baseline to compare the traditional install against |
| btrfs `compress=zstd:1` | mount option active, but data written at install time stayed uncompressed | the installer must mount with compression from the start (Anaconda btrfs mount options or a kickstart `%pre`) |

bootc-specific, not reusable as is:

- The OCI image pipeline: `Containerfile`, `bootc container lint`, push and
  sign by digest, the `:stable` channel tag, the lab OCI registry, the
  keyless ghcr.io workflow. Signing moves to RPM GPG signatures and
  `repo_gpgcheck` for Basalt's own repository; the cosign findings only matter
  if images are kept for containers.
- `bootc upgrade` / `bootc rollback`, the signature-enforced image reference
  (`ostree-image-signed`, `policy.json` default `reject`), the
  update-download size analysis (one 159 MiB layer per update).
- `basalt-install` as written: it wraps `bootc install to-disk --block-setup
  tpm2-luks` and edits ostree boot entries. Its TPM2/recovery logic (sections 3
  and the table above) is what to keep.
- ostree/composefs layout, `bootc install` config (`/usr/lib/bootc/...`),
  kernel arguments through `kargs.d`, bootupd and its SELinux denial.
- Not validated here and new for the traditional model: snapper with the dnf
  plugin, GRUB snapshot boot entries (grub-btrfs or equivalent) and how a
  snapshot boot interacts with the PCR 7 binding (it should not change PCR 7,
  since the signed shim, GRUB and kernel stay the same).

## Lab resources and how to remove them

All on the lab host, named by the `.env` values below; left running.

| Resource | Remove with |
|---|---|
| VM `VM_NAME` (libvirt, NVRAM, swtpm state) | `scripts/lab/vm.sh destroy` (or `virsh destroy` + `virsh undefine --nvram --tpm`) |
| Disk images in `VM_DIR` (installer.raw, target.qcow2) | `sudo rm -rf "$VM_DIR"` |
| libvirt network `VM_NETWORK` (bridge `VM_BRIDGE`) | `virsh net-destroy $VM_NETWORK && virsh net-undefine $VM_NETWORK` |
| Registry container `REGISTRY_CONTAINER` | `scripts/lab/registry.sh down` (data kept) |
| Registry data, lab CA, credentials, dev cosign key, recovery key, logs, cosign binary | `rm -rf "$LAB_DIR"` |
| Container images: `localhost/basalt-os:*`, `REGISTRY/IMAGE_REPO:*`, `localhost/basalt-lab-tools`, `quay.io/fedora/fedora-bootc:44` | `sudo podman rmi ...` |
| Serial log | `sudo rm /var/log/libvirt/qemu/$VM_NAME-serial.log` |

Milestone 0 asked for a first Basalt OS server image and proof that its
security and update model works end to end on real tooling:

1. an image derived from the Fedora bootc base, SELinux enforcing, hardened
   SSH, firewall on, tui-tools, Basalt OS branding;
2. a build, sign and publish pipeline with signature verification on the
   client;
3. an install with btrfs on LUKS2, TPM2 unlock bound to the Secure Boot state,
   and a recovery key, with proof that a changed Secure Boot state blocks the
   automatic unlock and that the recovery key works;
4. an update to a new version and a rollback, with no SELinux denials;
5. measurements.

All five were met. Commands below use the make targets and scripts in this
repository; site-specific values (registry address, paths) are shown as
`REGISTRY` and `LAB_DIR`.

## Environment

| Item | Value |
|---|---|
| Build and lab host | Fedora 44, 28 CPU threads, 62 GiB RAM, KVM, libvirt 12.0, QEMU 10.2, swtpm 0.10.2, edk2-ovmf 20260812, podman 5.8 |
| Base image | `quay.io/fedora/fedora-bootc:44` (the `latest` tag pointed to 44 on 2026-10-02; 45 is pre-release) |
| Image contents | kernel 7.2.8-200.fc44, systemd 259.9, bootc 1.16.13, selinux-policy-targeted 44.10, 580 packages |
| Registry | distribution (`registry:2`) in a container, TLS from a lab CA, htpasswd, bound to one LAN address |
| Signing | cosign v2.6.5, development key pair on the build host (0600) |
| VM | q35, 2 vCPUs, 4 GiB RAM, 20 GiB virtio disk, OVMF Secure Boot firmware with Microsoft and Red Hat keys enrolled, swtpm TPM 2.0 (CRB), isolated NAT network |

## 1. The image

`Containerfile` starts from the Fedora bootc base and runs `image/build.sh`:

- Branding: `generic-release` replaces `fedora-release`, `fedora-release-common`
  and `fedora-release-identity-basic` (the base has no `fedora-logos`).
  `/usr/lib/os-release` is Basalt OS (`NAME="Basalt OS"`, `ID=basalt`,
  `ID_LIKE=fedora`, `VERSION_ID=0.0.1`, `LOGO=basalt-logo`); the logo is
  installed under `/usr/share/pixmaps` and the hicolor icon theme. The
  initramfs is rebuilt so the initrd also identifies as Basalt OS.
- Packages added: firewalld, audit, policycoreutils-python-utils,
  setools-console, glibc-langpack-en, compsize, and twelve tui-tools from
  their signed repository. The repository key is shipped in the image and its
  fingerprint is checked at build time before it is imported.
- SSH: `PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
  `AuthenticationMethods publickey`, `PermitRootLogin prohibit-password`,
  no X11, agent or TCP tunnel forwarding, `MaxAuthTries 3`. The effective
  configuration is asserted with `sshd -T` during the build.
- firewalld enabled, default zone `public` with only `ssh`.
- auditd enabled, `multi-user.target` default, `en_US.UTF-8`, kernel console
  on tty0 and ttyS0.
- bootc install defaults: btrfs root, `rootflags=compress=zstd:1`, block
  setups `direct` (default for plain bootc) and `tpm2-luks`.
- `basalt-install` (section 3).
- One local SELinux module (`selinux/basalt_bootupd.cil`, section 4).
- Build-time assertions: SELinux enforcing and targeted, no Fedora branding
  packages, SSH and firewall settings, tools present; then `bootc container
  lint` (14 checks passed, 0 warnings).

```sh
make build            # 142 to 213 s on the lab host, depending on mirror speed
```

## 2. Sign, publish, verify

```sh
make lab-tools lab-keys lab-registry   # cosign, dev key pair, registry
make publish                           # push, sign the digest, move :stable
make verify                            # cosign verify + containers policy check
```

- `publish.sh` pushes `REGISTRY/IMAGE_REPO:<version>`, signs the
  pushed digest with cosign (`--tlog-upload=false`, private registry), then
  copies that digest to `:stable` with `skopeo copy --preserve-digests`. Push,
  sign and tag took 6 s on the LAN.
- The cosign passphrase reaches cosign through its environment only; the
  private key never leaves `LAB_DIR/keys` (mode 0600) and was never printed.
- `verify.sh` checks the signature with `cosign verify --key` and then copies
  the image with the exact `policy.json` and `registries.d` that the image
  ships. Result: signed tag accepted; an unsigned image pushed to the same
  repository (`:unsigned-test`) rejected by both checks
  (`A signature was required, but no signature exists`).
- Client side, inside the VM: `podman pull` of the signed image succeeds and
  of `:unsigned-test` fails with the same message, because the image's own
  `/etc/containers/policy.json` requires the key for its repository.
- The installed system tracks the image with signature enforcement
  (`bootc status`: `signature: containerPolicy`), and `bootc upgrade` refuses
  an unsigned image (section 4).

Finding: bootc refuses to enforce signatures when the policy's default is
`insecureAcceptAnything` (the base image's policy): `containers-policy.json
specifies a default of insecureAcceptAnything; refusing usage`. The image
policy therefore has a default of `reject`, its own repository signed by the
key, a short list of public registries allowed unsigned (docker.io, quay.io,
ghcr.io, Fedora and Red Hat registries) and local transports unchanged.

Public builds: `.github/workflows/image.yml` (not run yet) builds on GitHub
runners, pushes to `ghcr.io/basalt-os/basalt-os` and signs keyless. See
[signing.md](signing.md) for why keyless signatures are not yet enforceable
through `policy.json`.

## 3. Install: btrfs on LUKS2, TPM2 on PCR 7, recovery key

The install runs inside the target VM, because the key must be sealed to that
VM's TPM:

```sh
scripts/lab/e2e-install.sh
```

| Step | Time |
|---|---|
| installer disk (`bootc install to-disk --via-loopback`, plain btrfs) | 96 s |
| define VM, create vTPM state, boot installer to SSH | 39 s |
| `podman pull` of the image inside the VM (1156 MiB, signature checked) | 67 s |
| `basalt-install` on the target disk | 104 s |
| first boot of the installed system to SSH | 26 s |

`basalt-install` runs `bootc install to-disk --block-setup tpm2-luks` and then
fixes three things that bootc 1.16 does differently from what Basalt OS needs:

| bootc `tpm2-luks` alone | after `basalt-install` |
|---|---|
| TPM2 token with no PCR binding (`tpm2-hash-pcrs:` empty): any boot chain unlocks | TPM2 re-enrolled with `--tpm2-pcrs=7`, bank sha256 |
| no recovery key | recovery key slot added (written to stdout once, nowhere else) |
| `luks.options=tpm2-device=auto,headless=true`: no prompt if the TPM refuses | `headless=true` removed from the boot entry, so a prompt appears |

Resulting layout on the 20 GiB disk: ESP (vfat), `/boot` (btrfs), LUKS2
(aes-xts-plain64) holding the btrfs root mounted with `compress=zstd:1`. Key
slots: `0 recovery`, `2 tpm2` (PCR 7). The lab saves the recovery key to
`LAB_DIR/recovery/<vm>.txt` (0600) and only reports its size.

Secure Boot test (`scripts/lab/sb-test.sh`):

| Case | Result |
|---|---|
| Normal boot, Secure Boot enabled | unattended unlock, SSH in 25 s, 0 TPM failures |
| `SecureBootEnable` set to false in the VM's variable store | `TPM policy does not match current system state`, boot stops at `Please enter recovery key for disk root`, not reachable over the network; recovery key typed on the serial console unlocks it; `mokutil`: SecureBoot disabled |
| Original store restored | unattended unlock again |
| A self-made certificate added to `db`, Secure Boot still enabled | same refusal and prompt; recovery key unlocks |
| Original store restored | unattended unlock again |

The variable store is edited with `virt-fw-vars` (virt-firmware) while the VM is
off; the TPM state is untouched, so the only difference between runs is the
firmware's Secure Boot configuration measured into PCR 7.

## 4. Update and rollback

0.0.2 is the same tree built with `BASALT_VERSION=0.0.2`; the visible change is
the version in `os-release`, the login message and `/usr/share/basalt/version`.

```sh
NEW_VERSION=0.0.2 scripts/lab/update-test.sh
```

| Step | Result |
|---|---|
| build, sign, publish 0.0.2 to `:stable` | ok, signature verified |
| `bootc upgrade` in the VM | staged in 11 s (see note), signature checked |
| reboot | 33 s to SSH; `VERSION_ID=0.0.2`; motd `Basalt OS 0.0.2 (pre-alpha, based on Fedora Linux 44)`; 0 denials |
| `:stable` pointed at an unsigned image, `bootc upgrade` | refused: `A signature was required, but no signature exists`; tag restored |
| `bootc rollback`, reboot | 31 s to SSH; `VERSION_ID=0.0.1`; 0 denials; 0.0.2 kept as the rollback image |

Note on update time: 0.0.2 differs from 0.0.1 in one layer of 159 MiB (the
whole customization runs in one build step); that layer was already in the
VM from an earlier run of the same test, so 11 s is staging time, not
download time. Over the lab LAN the download adds a few seconds.

SELinux: `ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot` returned
nothing after the first boot, after the boots with the recovery key, after the
upgrade and after the rollback. An earlier build logged seven AVC records on
first boot, all from `bootupd_t` (a permissive domain in Fedora 44) reading
`/proc/swaps` and `/proc` through `lsblk` in `bootloader-update.service`. The
local module `basalt_bootupd` allows exactly those reads; with it the count is
zero.

## 5. Measurements

| Measure | Value |
|---|---|
| Base image, uncompressed | 1856 MiB |
| Basalt OS image, uncompressed | 2106 MiB (+250 MiB) |
| Basalt OS image, compressed in the registry | 1156 MiB, 69 layers |
| Update download 0.0.1 to 0.0.2 | 1 layer, 159 MiB |
| Boot time (firmware excluded) | first boot 17.4 s (kernel 2.4, initrd 4.6, userspace 10.4); later boots 11.4 to 12.1 s (kernel 2.4, initrd 4.0 to 4.4, userspace 4.6 to 5.3) |
| Boot to SSH, unattended unlock | 25 to 26 s from `virsh start` |
| Idle memory, 2 min after boot (4 GiB VM) | 445 to 460 MiB used, about 3370 MiB available |
| Largest resident processes | firewalld 53 MiB, journald 29 MiB, NetworkManager 21 MiB |
| Root file system used | 2.2 GiB (one deployment), 2.4 GiB (two) |
| Listening sockets | sshd 22; systemd-resolved LLMNR 5355 (blocked by the firewall) and local stub 53 |
| SELinux denials | 0 after boot, after update, after rollback |
| Failed units | 0 |

Where the 250 MiB go: about 90 MiB of packages (tui-tools 65 MiB, the rest
26 MiB), a second copy of the 102 MiB initramfs (the rebuilt one is a new layer
over the base's), and a rewritten SELinux policy store (`semodule`).

## Gaps and follow-ups

1. btrfs compression is not effective for the installed system yet: `compsize`
   reports 98 to 99 % (no gain). `bootc install` writes the deployment before
   the `compress=zstd:1` mount option is in effect; only later writes are
   compressed. Options: install with `bootc install to-filesystem` onto a
   file system prepared and mounted by `basalt-install` (which also allows
   btrfs subvolumes), or recompress after install.
2. `bootc install --block-setup tpm2-luks` (bootc 1.16) does not bind PCRs,
   creates no recovery key and sets `headless=true`. `basalt-install` works
   around all three after the fact (editing the boot entry in place). Worth
   reporting upstream; `to-filesystem` would remove the workaround.
3. Secure Boot uses the distribution chain (Microsoft-signed shim, Fedora's
   grub and kernel). Own keys and signed modules are a later milestone. The
   ESP path is `EFI/fedora` and the installer's firmware boot entry is labeled
   "Fedora" (from shim's `BOOTX64.CSV`); the installed entry is labeled
   "Basalt OS 0.0.1 (pre-alpha)".
4. PCR 7 only: a changed boot loader or kernel signed by the same keys still
   unlocks. Adding PCR 4/11 or signed PCR policies (systemd-pcrlock or UKI with
   `--tpm2-public-key`) is the next step; it needs a plan for re-sealing on
   kernel updates.
5. Keyless signatures (CI) cannot be enforced through `policy.json` for a
   GitHub workflow identity; the public image currently ships no signature
   policy (see [signing.md](signing.md)).
6. Image size: the rebuilt initramfs and policy store duplicate data in new
   layers, and the customization is one 159 MiB layer, so every update
   downloads at least that much. Rechunking the image would split it by
   package and shrink updates.
7. Tang (network-bound unlock) and a passphrase-only install path for machines
   without a TPM are not implemented; `basalt-install` refuses to encrypt
   without a TPM and offers `--no-encryption`.
8. Hardening still to do: disable LLMNR in systemd-resolved, review the
   default package set, kernel lockdown and `module.sig_enforce`.
9. The lab registry has no read-only account: the installed system's pull
   credential can also push. Acceptable for a lab only.
10. Lab host findings: libvirt's own vTPM manufacturing fails on Fedora 44
    (`swtpm_t` denied on its pidfile in virtqemud's `/tmp`), worked around by
    creating the TPM state before the first start; DNS does not resolve inside
    rootful podman's default network on that host, so builds use the host
    network.

## Reproduce

```sh
cp env.example .env && $EDITOR .env
make lab-tools lab-keys lab-registry
make release
scripts/lab/e2e-install.sh
IDLE_SECONDS=120 scripts/lab/measure.sh "first boot"
scripts/lab/sb-test.sh
NEW_VERSION=0.0.2 scripts/lab/update-test.sh
```
