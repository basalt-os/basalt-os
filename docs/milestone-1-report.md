# Milestone 1 report

Date: 2026-10-03. Status: done, with the gaps listed at the end.

Milestone 1 asked for:

1. Secure Boot with Basalt's own keys, proven in the lab, in two modes: MOK
   mode on top of Fedora's shim (with signed kernel modules and lockdown)
   and custom db mode (own PK, KEK and db in the firmware), with the effect
   on the TPM measured and a re-enrollment flow.
2. A decision on the TPM policy beyond PCR 7, and a `basalt-tpm` helper.
3. Optional network unlock with Clevis and Tang.
4. Milestone 0 gaps: kernels left on `/boot` by a rollback, a minimal
   package profile for virtual machines, booting the pre-upgrade snapshot
   after a release upgrade, reliable GRUB automation in the tests.

All of it works. How and why: [secure-boot.md](secure-boot.md),
[design.md](design.md); the release key procedure:
[key-ceremony.md](key-ceremony.md). The version (`0.0.1`), the installer
approach (`mkksiso` on the stock Fedora ISO) and the absence of release keys
are unchanged and remain open decisions.

## Environment

Same lab host and firmware as [milestone 0](milestone-0-report.md): KVM with
OVMF Secure Boot firmware (Microsoft and Red Hat keys enrolled) and an
emulated TPM 2.0 (swtpm) per VM, never the host's TPM. Fedora 44, systemd
259, shim 16.1, kernels 6.19.10 (install) and 7.2.8 (updates). Five VMs, 4
vCPUs and 6 to 8 GiB each:

| VM | Install | Used for |
|---|---|---|
| 1 | standard profile, `tpm2` unlock | all milestone 0 tests again, MOK mode, orphaned kernels |
| 2 | milestone 0's unencrypted VM, upgraded to Fedora 45 | pre-upgrade snapshot boot, rollback to 44, forward to 45 |
| 3 | standard profile, `tpm2` unlock | custom db mode |
| 4 | minimal profile, `tang` unlock | Tang, minimal profile measurements |
| 5 | minimal profile, `tpm2+tang` unlock | TPM2 and Tang together |

## 1. Secure Boot with Basalt's own keys

### MOK mode (`scripts/lab/mok-test.sh`)

The Basalt kernel module CA is enrolled with `basalt-secureboot enroll-mok`
(`mokutil --import`); at the next boot MokManager is confirmed over the
serial console with the one-time password (`mok-console.py`). A test module
(`packages/lab/kmodtest`) is built against the running kernel and signed
four ways (`kmod-build.sh`).

| Module | Secure Boot on, before enrollment | Secure Boot on, CA enrolled | Secure Boot off (command line `lockdown=integrity module.sig_enforce=1`) |
|---|---|---|---|
| unsigned | refused | refused | refused |
| unknown key | refused | refused | refused |
| signed by the module CA key | refused | loads | refused |
| signed by a signing certificate issued by the CA | refused | loads | refused |

Refusals are "Key was rejected by service" ("Loading of unsigned module is
rejected", "Loading of module with unavailable key is rejected" in the
kernel log). After enrollment the CA is in the `.machine` keyring and
`basalt-module-keys.service` adds the signing certificate to
`.secondary_trusted_keys` at boot. With Secure Boot off the kernel does not
load MOKs at all, so only Fedora's own modules load: the command line keeps
enforcement on, and Basalt-signed modules need Secure Boot.

Kernel lockdown is `integrity` in every case: from Secure Boot when it is
on, from the command line when it is off.

Effect on the TPM (PCR 7 sealed disk, unattended unlock):

| PCR | Before | After enrollment |
|---|---|---|
| 4 | fee588bc0f27 | fee588bc0f27 (changes only on the boot that runs MokManager) |
| 7 | 1ea05e42f705 | 1ea05e42f705 |
| 14 | 17cdefd9548f | c0b62d7152a8 |

0 TPM failures, 0 denials. MOK enrollment needs no TPM re-enrollment.

Finding: Fedora kernels only accept a CA as a MOK
(`INTEGRITY_CA_MACHINE_KEYRING_MAX`); a plain self-signed signing
certificate enrolled as a MOK would land in the `.platform` keyring and be
useless for modules. Hence the CA plus signing certificate design.

### Custom db mode (`scripts/lab/sb-custom-test.sh`)

| Step | Result |
|---|---|
| shim signed with the lab db key (`sbsign`) | the Microsoft signature stays, Basalt's is added; `sbverify --cert db.pem` OK |
| `basalt-tpm suspend`, then PK, KEK, db replaced (Microsoft and Red Hat certificates out, dbx kept) | |
| first boot in custom db mode | unattended in 32 s through the suspended key; `basalt-tpm-resume` sealed a new key to the new PCR 7 (7e3bf19757a4, was 1ea05e42f705) and removed the unbound one; 0 denials |
| next boot | unattended, 0 TPM failures |
| stock Fedora installer ISO (shim signed only by Microsoft) | refused by the firmware: "Access Denied -- rejected probably by Secure Boot"; it fell back to the disk |
| stock keys restored without suspending | TPM refuses, recovery key typed on the serial console, `basalt-tpm reenroll --recovery-key-file` seals again, next boot unattended |
| custom keys again, same recovery flow | same; PCR 7 identical to the first custom boot |

`basalt-secureboot status` in custom db mode lists the Basalt PK, KEK and
db; Fedora's kernel and GRUB updates keep working (they are verified by
shim's built-in Fedora certificate), only shim needs Basalt's signature.

## 2. TPM policy

PCRs recorded on the standard VM (first 12 hex digits):

| Event | PCR 4 | PCR 7 | PCR 8 | PCR 9 | PCR 14 | Disk unlock |
|---|---|---|---|---|---|---|
| first boot, kernel 6.19.10 | caa1c266ad45 | 1ea05e42f705 | 88d65f28026e | e8e90c389973 | 17cdefd9548f | unattended |
| after the update to 7.2.8 | fee588bc0f27 | same | 1ade0856dc13 | 2890e2e359e5 | same | unattended |
| after rollback A (same kernel) | fee588bc0f27 | same | bbcf7bf8cb51 | 80bdddd33793 | same | unattended |
| snapshot booted from the GRUB menu | fee588bc0f27 | same | 21fe7a114539 | 011c76f95b99 | same | unattended |
| rollback C to the first-boot state (kernel 6.19.10) | 58abc8f6a243 | same | 591b2a0cae89 | d76a4f3ca952 | same | unattended |
| MOK enrolled | fee588bc0f27 | same | | | c0b62d7152a8 | unattended |
| custom db keys | | 7e3bf19757a4 | | | | re-sealed |

PCR 4, 8 and 9 change with kernel updates, rollbacks and snapshot boots;
PCR 7 changes only with the Secure Boot configuration. `systemd-pcrlock
log` on this boot chain finds matching components only for PCR 3 and 6:
the shim, GRUB and kernel images in PCR 4 and the 72 GRUB command events of
PCR 8 are not recognized, and GRUB's configuration changes after every dnf
transaction. Signed PCR 11 policies need unified kernel images, which
conflict with the snapshot boot menu (see secure-boot.md).

Decision: PCR 7 stays the TPM policy. A binding that includes PCR 4, 8 or
9 would ask for the recovery key after updates, rollbacks and snapshot
boots, the moments an unattended server must come back alone, and it
cannot be re-sealed ahead of time without a prediction. What PCR 7 does not
cover (any boot chain verified by the same certificates measures the same
value) is narrowed by custom db mode and by `tpm2+tang`.

`basalt-tpm` (status, pcrs, reenroll, suspend, resume) was used throughout
the tests above. Finding: `systemd-cryptenroll` treats an enrollment as a
no-op when a TPM2 key with the same PCR set exists ("This PCR set is
already enrolled, executing no operation"), even if its sealed values are
stale; `basalt-tpm` removes the stale key first.

## 3. Network unlock with Tang (`scripts/lab/tang-test.sh`)

Tang server: a Fedora container on the lab network
(`scripts/lab/tang-serve.sh`). Installs with `basalt.unlock=tang` and
`tpm2+tang`, thumbprint pinned.

| Case | `tang` | `tpm2+tang` |
|---|---|---|
| Tang up | unattended, 33 s to SSH | unattended, 31 s |
| Tang down | prompt, not reachable | prompt, not reachable |
| Tang back while waiting at the prompt | unlocked by itself 12 s later | 9 s later |
| Tang down, recovery key on the serial console | unlocks | unlocks |
| Secure Boot disabled, Tang up | | prompt, not reachable (the TPM half refuses); recovery key unlocks; firmware restored: unattended again |

0 denials and 0 failed units after every boot. Key slots: one Clevis slot
and the recovery key; crypttab without `tpm2-device`; `rd.neednet=1` on the
command line.

Finding: `cryptsetup open --test-passphrase --key-file` tries token
plugins first. The installer used it to find its temporary passphrase's
slot and, inside the installer, the TPM2 token answered first, so the TPM2
slot was removed instead (the first two `tpm2` installs of this milestone
needed the recovery key on first boot and were reinstalled). The kickstart
now tests each slot on its own with token plugins off.

## 4. Milestone 0 gaps

### Orphaned kernels (`scripts/lab/kernels-test.sh`)

| Step | Result |
|---|---|
| rollback to the first-boot snapshot (6.19.10) while 7.2.8 runs, reboot | 7.2.8 stays on `/boot`, listed as "kept: no package; modules only in snapshots 5 to 20"; `--clean-kernels` removes nothing |
| the snapshots holding 7.2.8 deleted | 7.2.8 listed as "orphaned"; the next `dnf install` prints "orphaned kernels on /boot: 7.2.8-200.fc44.x86_64; remove them with: basalt-rollback --clean-kernels" |
| `basalt-rollback --clean-kernels --yes` | kernel image, initramfs, boot entry and the dangling `symvers` link removed; default entry 6.19.10 |
| reboot | 6.19.10, 0 denials, 0 failed units, no orphans |

### Release upgrade: back to the old release (`scripts/lab/upgrade-rollback-test.sh`)

On the VM upgraded from Fedora 44 to 45 in milestone 0:

| Step | Result |
|---|---|
| the pre-upgrade snapshot (the offline upgrade transaction's pre snapshot) booted from the GRUB menu | Fedora 44 userspace (`rpm -E %fedora` 44, `basalt-release-44`), kernel 6.19.10 (still on `/boot`), root read-only, SSH up, 0 denials, 0 failed units |
| `basalt-rollback` (keep it), reboot | bug found: booted the Fedora 45 kernel with the 44 root, no modules, emergency mode |
| cause | the snapshot carried `basalt-snapshots` 0.1.0-4, older than the kernel selection that milestone 0 added later; a booted snapshot runs its own copy of the tools |
| fix | `basalt-rollback` started from a snapshot now hands over to the installed system's copy when it is newer (0.2.0-2) |
| after selecting the 44 kernel | Fedora 44 read-write, `dnf repolist` shows the 44 repositories, 0 denials, 0 failed units |
| `basalt-rollback` to the kept 45 state (new tool) | default entry set to 7.2.8-300.fc45; reboot: Fedora 45, `htop` still installed, 0 denials, 0 failed units |

### Minimal profile (`basalt.profile=minimal`)

| | Standard | Minimal (with Clevis) |
|---|---|---|
| Packages after install | 473 | 397 |
| Installed size | 1129 MiB | 774 MiB |
| Root file system on disk (compressed) | 807 MiB (70 %) | 481 MiB (61 %) |
| Idle memory | 505 MiB | 479 MiB |
| Boot (first) | 18.2 s | 14.4 s |
| SELinux denials, failed units | 0, 0 | 0, 0 |

The minimal profile excludes hardware firmware, CPU microcode, fwupd and
flashrom, which also drops udisks2, polkit, Bluetooth, avahi, mdadm and
file system tools for foreign formats that came in as their weak
dependencies. A container dry run of excluding all weak dependencies
(337 packages, 723 MiB) was rejected: it also drops `logrotate`,
`crypto-policies-scripts`, `systemd-pam` and `sudo`'s Python plugin.

### GRUB automation

`grub-console.py` now uses only single-byte keys: the snapshot submenu's
hotkey `s` (new in the generated menu), Ctrl-N to move, Ctrl-F to boot. It
fails unless GRUB prints "Booting snapshot N" for the expected N, and it
falls back to Ctrl-E (last entry) on menus without the hotkey. Every GRUB
selection in this milestone worked at the first attempt: the hotkey on the
new menu, the fallback on the older menu of the upgraded VM, and
`--top N` for a top-level kernel entry.

## 5. Milestone 0 tests again

On VM 1 with the milestone 1 packages, one unattended sequence: measure,
snapshot test, rollback test (cases A, B, C), Secure Boot test, MOK test
with the Secure Boot off case, measure. All passed; 0 SELinux denials
(AVC, USER_AVC, SELINUX_ERR) after every boot and update; 0 failed units.

| Measure | Milestone 1 (standard VM) |
|---|---|
| Unattended install | 575 s (523 s minimal) |
| First boot to SSH, TPM unlock | 32 to 33 s |
| Boot time later | 12.3 s (3.7 kernel, 3.8 initrd, 4.7 userspace) |
| Idle memory | 497 MiB |
| Packages after the tests | 490 |
| Snapshots kept after the tests | 19, 709 MiB on disk (live root 1025 MiB) |
| `basalt-security` | 23 KiB package |

## Gaps and follow-ups

1. Release keys do not exist; `basalt-security` and `basalt-release` ship
   placeholders. The key ceremony is written, not performed.
2. A Basalt-signed shim package does not exist. In custom db mode a shim
   update from Fedora replaces the Basalt-signed file with a
   Microsoft-only one that the firmware refuses: until the package exists,
   custom db machines must hold shim updates (`excludepkgs=shim-*`) and
   re-sign by hand. This is why custom db mode is the advanced option.
3. Basalt OS ships no out-of-tree kernel module yet, so nothing is signed
   with the module key in production; the pipeline is the lab one.
   Modules needed in the initramfs would also need the signing certificate
   there.
4. PCR 7 only, as decided above. Revisit with signed unified kernel images.
   A shim update that raises the SBAT level, or a dbx update, changes PCR 7:
   run `basalt-tpm suspend` before those (not automated).
5. MOK enrollment needs a person at the console once per machine (by
   design). Fleet installs in custom db mode can avoid it only with a shim
   carrying Basalt's certificate built in, which means shim review.
6. On the first dnf use after the Basalt key is (re)introduced, for example
   after rolling back to the first-boot snapshot, dnf5 prints "repomd.xml
   GPG signature verification error: Signing key not found" before it
   imports the key from `basalt-release` and verifies. Importing the key at
   install time would avoid the message.
7. Tang without a pinned thumbprint trusts the first advertisement; the
   installer logs a warning. Tang server high availability (two servers,
   Shamir threshold 1 of 2) is not set up.
8. A rolled-back system may still list a newer kernel in the GRUB main menu
   (it is kept while snapshots hold it); choosing it by hand boots a root
   without its modules. The default entry is right; the menu does not hide
   it.

## Reproduce

```sh
cp env.example .env && $EDITOR .env
make lab-tools lab-keys lab-sb-keys rpms-lab repo iso-fetch iso lab-tang
make lab-install lab-measure lab-snapshot-test lab-rollback-test lab-sb-test
scripts/lab/mok-test.sh --sb-off
scripts/lab/kernels-test.sh
VM_NAME=<vm3> VM_HOST=12 make lab-install && VM_NAME=<vm3> VM_HOST=12 make lab-sb-custom-test
scripts/lab/site-variant.sh tang BASALT_UNLOCK=tang BASALT_TANG_URL=http://<gateway>:7500 BASALT_TANG_THP="$(scripts/lab/tang-serve.sh thp)" BASALT_PROFILE=minimal
SITE_DIR=$LAB_DIR/site-tang SITE_NAME=lab-tang make iso
VM_NAME=<vm4> VM_HOST=13 scripts/lab/install.sh build/iso/basalt-os-0.0.1-x86_64-lab-tang.iso && VM_NAME=<vm4> VM_HOST=13 make lab-tang-test
VM_NAME=<vm2> VM_HOST=11 scripts/lab/upgrade-test.sh 45 && VM_NAME=<vm2> VM_HOST=11 scripts/lab/upgrade-rollback-test.sh
```
