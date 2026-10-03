# Milestone 0 report

Date: 2026-10-03. Status: done, with the gaps listed at the end.

Milestone 0 asked for a first installable Basalt OS server on Fedora 44,
traditional and package-based (see [design.md](design.md)), and proof on
real tooling that its security and update model works:

1. Basalt packages built as RPMs from specs in this repository, published in
   a signed RPM repository.
2. An installer with the Basalt storage layout, LUKS2 on by default, TPM2
   unlock bound to PCR 7 and a recovery key.
3. In a VM with UEFI Secure Boot and a TPM: SELinux enforcing with no
   denials, unattended unlock, a changed Secure Boot state blocking it and the
   recovery key working, snapshots around dnf transactions, packages that
   persist across updates and reboots, and rollback of a broken update both
   from the running system and from the boot menu without touching data.
4. Measurements.

All of it works. An earlier prototype of this milestone was image-based
(bootc); it is kept under `archive/bootc/` and its report is
`archive/bootc/docs/milestone-0-bootc-report.md`. Its SSH, firewall,
branding, encryption and lab findings were carried over.

## Environment

| Item | Value |
|---|---|
| Lab host | Fedora 44, 28 CPU threads, 62 GiB RAM, KVM, libvirt 12.0, QEMU 10.2, swtpm 0.10.2, edk2-ovmf 20260812, podman 5.8, docker |
| Build | `registry.fedoraproject.org/fedora:44` and `:45` containers (rpmbuild, rpmsign, createrepo_c, lorax 44 `mkksiso`) |
| Installer input | `Fedora-Everything-netinst-x86_64-44-1.7.iso` (CHECKSUM signature and SHA-256 verified) |
| Installed base | Fedora 44: kernel 6.19.10 at install, 7.2.8 after updates; systemd 259; dnf5 5.4; snapper 0.13.0; selinux-policy-targeted 44 |
| VM | q35, 4 vCPU, 8 GiB RAM, 30 GiB virtio disk, OVMF with Secure Boot on and the Microsoft and Red Hat keys enrolled, swtpm TPM 2.0 (CRB), isolated NAT network |

## 1. Packages and signed repository

```sh
make rpms-lab repo repo-verify      # FEDORA_RELEASE=45 for the next release
```

| Package | Version | Content |
|---|---|---|
| `basalt-release` | 44-2 | os-release (`Basalt OS 0.0.1 (pre-alpha)`, `ID=basalt`, `ID_LIKE=fedora`), release files, dist macros, `basalt.repo` with `gpgcheck` and `repo_gpgcheck`, key, `basalt_repo_url` dnf variable, dnf defaults, presets |
| `basalt-release-server` | 44-2 | SSH keys-only drop-in, firewalld zone `basalt` (SSH only) as default, LLMNR and mDNS off |
| `basalt-logos` and 5 subpackages | 0.1.0-2 | branding kit; GRUB theme integrated through `/etc/grub.d/06_basalt_theme` for grub2-mkconfig |
| `basalt-snapshots` | 0.1.0-8 | snapper template, dnf5 actions hook, snapshot boot menu, setup, rollback, first-boot snapshot unit |
| `basalt-canary` (lab only) | 1.0, 2.0, 3.0 | healthy, broken update, breaks sshd at next boot; published to a separate lab repository |

- All packages build in a clean Fedora container in under a minute.
  `basalt-release` provides `system-release(releasever) = 44`, so `$releasever`
  and the Fedora repositories keep working (checked: `dnf repolist` shows
  Fedora 44 and Basalt OS 44).
- Signing: a development RSA 4096 key on the lab host only (mode 0600, its
  passphrase in a 0600 file, never printed). `rpmsign` signs every package,
  `createrepo_c` builds the metadata and `repomd.xml` gets a detached armored
  signature. `make repo-verify` checks both with only the exported public key:
  9 packages, 0 failures, "Good signature" on the metadata. Source RPMs are
  published in `<release>/source/`.
- On the installed system dnf verifies the repository metadata and the
  packages against the key shipped in `basalt-release` (the first use imports
  it after showing its fingerprint).

## 2. Installer

`make iso` embeds the kickstart and the signed repository into the stock
Fedora netinst ISO with `mkksiso` (why: [design.md](design.md#installer)).
Boot menu entries read "Install Basalt OS 0.0.1 (Fedora 44 base)"; the default
entry starts after 5 s; kernel arguments add the serial console and text mode.
The lab ISO also carries a site file (root SSH keys, power off at the end,
recovery key not shown on the console).

| Install (lab VM, unattended) | Result |
|---|---|
| VM creation to power off (packages from Fedora mirrors) | 513 to 534 s |
| First boot to SSH, disk unlocked by the TPM | 33 to 34 s |
| Layout | GPT: ESP 600 MiB, `/boot` ext4 1 GiB, LUKS2 (aes-xts-plain64) with btrfs: `root`, `home`, `srv`, `var_log`, `var_cache`, `var_tmp`, `var_spool`, `var_lib_containers`, `var_lib_libvirt`, `var_lib_pgsql`, `var_lib_mysql`, `root/.snapshots` |
| LUKS key slots | `1 tpm2` (`tpm2-hash-pcrs: 7`, bank sha256), `2 recovery`; the installer's random passphrase slot is wiped |
| crypttab | `none discard,x-initrd.attach,tpm2-device=auto` (no `headless`) |
| Compression | `compress=zstd:1` active during the install: the root is 70 % of its uncompressed size right after install (805 MiB on disk for 1.1 GiB) |
| Identity | `basalt-release` installed, no `fedora-release*` or `fedora-logos`; boot entries titled "Basalt OS (kernel) 0.0.1 (pre-alpha)" |
| Default subvolume | `root` (id 266); no `rootflags=subvol=` on the kernel command line |
| First boot | 0 SELinux denials, 0 failed units; SecureBoot enabled; firewalld zone `basalt` with only `ssh`; `sshd -T` keys only; LLMNR and mDNS off; Plymouth theme `basalt` |

`basalt.encrypt=0` (or `BASALT_ENCRYPT=0` in the site file) was tested on a
second VM: plain btrfs with the same subvolumes, no crypttab, 0 denials, 0
failed units.

The recovery key (71 characters) is written to `/root/basalt-recovery-key.txt`
with a login notice; the lab moves it to the lab host (0600) and deletes it
from the VM.

## 3. Tests

The final run is one unattended sequence on a freshly installed VM, with the
packages as committed: install, measure, snapshot test, rollback test
(cases A, B and C), Secure Boot test, measure. It passed. Two earlier full
runs found the issues listed under findings 7 and 8 and a test script bug. SELinux denials (AVC, USER_AVC, SELINUX_ERR)
were counted after every boot and every update: 0 everywhere, except the boot
noted in the findings below, which the fix removed.

### Snapshots around dnf, persistence (`make lab-snapshot-test`)

| Step | Result |
|---|---|
| `dnf install htop` | pre snapshot and post snapshot, both described `dnf -y install htop`; the pre snapshot is first in the GRUB snapshot menu |
| `dnf upgrade --refresh` (570 MiB download, 247 packages, kernel 6.19.10 to 7.2.8) | pre/post pair around the whole transaction; `htop` still installed; 0 denials |
| Reboot | new kernel, TPM unlock, `htop` installed and working, 0 denials, 0 failed units |
| Snapshot cost | `snapper create` 83 to 150 ms; each transaction adds two snapshots and two boot menu updates, well under a second in total (the install times measured, 10 to 76 s, were dominated by the mirror) |

### Rollback (`make lab-rollback-test`)

Data markers are written to `/home`, `/var/lib/containers`, `/var/lib/pgsql`
and `/var/log` before and after each broken update, and checked after each
rollback.

| Case | Result |
|---|---|
| A. canary 1 to 2 (broken: the command fails) | pre snapshot recorded; `basalt-rollback <pre>` then reboot: canary 1.0 back and healthy, markers written before and after the broken update all present, TPM unlock, 0 denials, 0 failed units |
| B. canary 1 to 3 (breaks sshd at the next boot) | reboot: SSH stays down. Power cycle; over the serial console the GRUB "Basalt OS snapshots (read-only)" submenu is opened and the update's pre snapshot booted. It comes up read-only (writing to `/usr` fails), disk unlocked by the TPM, SSH up, canary 1.0 healthy, all markers present, 0 denials, 0 failed units. `basalt-rollback` keeps it; the normal reboot is healthy with no broken sshd drop-in |
| C. across a kernel update | rollback to the first-boot snapshot (kernel 6.19.10 only) while 7.2.8 runs: `basalt-rollback` makes 6.19.10 the default boot entry; the system boots read-write on 6.19.10 with SSH, markers present, 0 denials. Rolling forward to the copy snapper kept returns to 7.2.8 and canary 1.0 |

snapper's output during a rollback, for the record: "Creating read-only
snapshot of current system. Creating read-write snapshot of snapshot N.
Setting default subvolume to snapshot M." Each rollback adds two snapshots
(the kept state and the new root); the retention policy cleans them up.

### Encryption and Secure Boot (`make lab-sb-test`)

| Case | Result |
|---|---|
| Normal boot | unattended unlock, SSH in 29 s from power on, 0 TPM failures |
| `SecureBootEnable` set to false in the VM's variable store | TPM refuses (PCR 7 changed); boot stops at the passphrase prompt, also on the serial console, with Plymouth installed; not reachable over the network; the recovery key typed on the serial console unlocks it; `mokutil`: SecureBoot disabled |
| Original store restored | unattended unlock again |
| A self-made certificate added to `db` (Secure Boot still on) | same refusal and prompt; recovery key unlocks |
| Original store restored | unattended unlock again |
| Boot of a snapshot from the GRUB menu | unattended unlock (same signed kernel, PCR 7 unchanged) |

### Release upgrade, Fedora 44 to 45 (`scripts/lab/upgrade-test.sh`)

Fedora 45 was available as a pre-release (branched, 2026-10). On the
unencrypted VM, with `basalt-release` and the other Basalt packages built for
45 in the repository:

| Step | Result |
|---|---|
| `dnf system-upgrade download --releasever=45` | 632 MiB, download and test transaction in 74 s; the Fedora 45 key is already shipped by `fedora-repos` 44 |
| `dnf offline reboot` | offline upgrade and reboot to SSH in 303 s |
| After | `Basalt OS 0.0.1 (pre-alpha)`, `rpm -E %fedora` and `$releasever` = 45, `basalt-release-45`, kernel 7.2.8-300.fc45, `htop` (installed before) upgraded and present, `fedora-release` not installed, 0 denials, 0 failed units |
| Snapshots | the offline transaction (`dnf5 offline _execute`) is bracketed by a pre and a post snapshot; the pre snapshot is in the boot menu |

Not tested: booting that pre-upgrade snapshot (it needs a Fedora 44 kernel,
which is still on `/boot` after the upgrade, so the menu offers it).

## 4. Measurements

| Measure | Value |
|---|---|
| Basalt OS ISO (lab) | 1164 MiB (Fedora netinst 1160 MiB plus 3.6 MiB of Basalt packages and the kickstart) |
| Installed packages | 470 after install (1127 MiB installed size), 487 after updates |
| Disk used after install | root file system 910 MiB (805 MiB of it the root subvolume, compressed from 1.1 GiB); `/boot` 305 MiB |
| Boot time (firmware excluded) | 16.1 to 17.4 s on first boot (kernel 3.9 to 4.1, initrd 5.2 to 5.6 including the TPM unlock, userspace 6.5 to 8.1); 12.3 to 12.4 s on later boots (3.8 + 4.0 + 4.6) |
| Power on to SSH | 22 to 34 s with TPM unlock |
| Idle memory, 2 min after boot (8 GiB VM) | 497 to 507 MiB used |
| Largest resident processes | firewalld 46 MiB, systemd-journald 28 MiB, systemd 21 MiB, NetworkManager 21 MiB |
| Listening | sshd on 22; systemd-resolved stub on 127.0.0.53/54; chronyd on localhost |
| Snapshot creation | 83 to 150 ms |
| Space kept by the first snapshot | 8 MiB right after the first boot |
| Space kept by snapshots | 675 to 707 MiB after one full update, the rollback tests and 15 to 19 snapshots (live root 1024 MiB); mostly the pre-update kernel, firmware and libraries |
| Unencrypted VM after the Fedora 45 upgrade | boot 10.5 s, 485 packages, 2.7 GiB used |

## Findings

1. dnf5 integration. Fedora 44 uses dnf5; `python3-dnf-plugin-snapper` (still
   packaged) is a dnf4 plugin and does nothing for dnf5. The libdnf5 actions
   plugin works well for this: one `.actions` file, the pre number carried to
   the post step in an actions variable, `enabled=host-only` to skip
   installer transactions, `raise_error=0` so a snapshot problem never blocks
   an update. It also runs inside offline transactions (`system-upgrade`).
2. snapperd caches snapshot lists. Snapshots created with `--no-dbus` while
   snapperd runs do not appear in `snapper list` until it restarts. The hook
   uses snapperd when D-Bus is available and falls back to `--no-dbus`
   otherwise.
3. snapperd memory. With background comparison on, comparing the pre and post
   snapshots of a full update kept snapperd busy for over a minute with a
   memory peak of about 850 MiB. The Basalt template turns background
   comparison off (`snapper status` still compares on demand).
4. Rollback needs the default subvolume. Fedora mounts the root by name
   (`rootflags=subvol=root`), which makes `snapper rollback` ineffective, and
   `grub2-mkconfig` (`10_linux`) adds that option back to every boot entry.
   `SUSE_BTRFS_SNAPSHOT_BOOTING="true"` in `/etc/default/grub` stops it; it
   then appends a `${extra_cmdline}` placeholder that Fedora's GRUB passes to
   the kernel as text, so the Basalt GRUB hook removes it from the entries.
5. grub-btrfs is not packaged in Fedora 44 and is built around regenerating
   grub.cfg; a small generator that writes one sourced file is enough with
   Fedora's BLS layout, needs no grub2-mkconfig per update and lists only
   snapshots whose kernel is still on `/boot`.
6. Booting a read-only snapshot then `snapper rollback` works on Fedora's BLS
   layout, with `--ambit classic` given explicitly (a read-only root would
   otherwise be detected as a transactional system). The read-only boot had 0
   failed units and 0 denials in these tests.
7. Never snapshot inside the installer. The first version took the initial
   snapshot in the kickstart's `%post`; files there are relabeled only at the
   end of the install and first-boot setup (SSH host keys, system users) had
   not run. Rolling back to it booted with `/etc/fstab` labeled `root_t`:
   `systemd-remount-fs` could not read it, the root stayed read-only, SSH
   host keys could not be created, and the boot logged AVC denials. The first
   snapshot is now taken on the first boot by `basalt-initial-snapshot.service`.
8. Kernels live on `/boot`, outside the snapshots. A rollback across a kernel
   update must boot a kernel whose modules exist in the rolled-back root;
   `basalt-rollback` now sets that entry as the default.
9. TPM enrollment from the installer works: PCR 7 measured while the netinst
   ISO boots (Microsoft-signed shim, Fedora GRUB and kernel) matches the
   installed system's, so the first boot unlocks unattended.
10. Lab host: Docker's iptables `FORWARD DROP` policy also drops libvirt NAT
    traffic (VMs reach the gateway but not the internet); the lab inserts two
    `DOCKER-USER` rules for its bridge only. The vTPM manufacturing workaround
    from the earlier prototype is still needed on Fedora 44 hosts.

## Gaps and follow-ups

Milestone 1 ([milestone-1-report.md](milestone-1-report.md)) addressed
gaps 1 (orphaned kernels), 2 (decision: PCR 7 stays; own Secure Boot keys),
5 (minimal profile), 7 (pre-upgrade snapshot booted), 8 (Tang) and 9 (GRUB
automation).

1. Kernels and rollback. After a rollback across a kernel update, the newer
   kernel stays on `/boot` with its boot entry although the rolled-back rpm
   database does not know it; the next kernel update will not remove it.
   Options: a kernel-install hook that keeps kernels in sync with the root, or
   `/boot` content in the snapshot (needs GRUB to read the encrypted root).
2. PCR 7 only. A different boot loader or kernel signed by the same keys still
   unlocks. Signed PCR policies (UKI, `systemd-pcrlock`) and re-sealing on
   kernel updates are next, together with own Secure Boot keys.
3. Installer branding: Anaconda's own screens say Fedora; installs need the
   network for Fedora packages. A product image or an offline ISO are options.
4. The lab ISO's accounts come from a site file; the public ISO asks for a
   root password or user in text mode. A first-boot SSH key prompt or a cloud
   style method is not designed yet.
5. Package set: `@core` pulls hardware firmware a VM or many servers do not
   need (the largest installed package is `nvidia-gpu-firmware`, 101 MiB) and
   firewalld brings Python. A smaller default set is a separate decision.
6. Release key: none exists. The placeholder in `basalt-release` makes a
   non-lab build unable to verify the repository on purpose.
7. Release upgrade: the pre-upgrade snapshot was not booted after the
   upgrade; the rpm database, `/boot` and the snapshot boot menu across a
   release change need a test of their own before upgrades are announced.
8. Tang (network-bound unlock) and passphrase-only installs on machines
   without a TPM (today the recovery key is then the only key) are not done.
9. Lab GRUB automation over the serial line is occasionally flaky (an escape
   sequence split in two reads as ESC and opens GRUB's prompt); the script now
   retries.
10. The read-only snapshot boot is meant for recovery only; services that need
    to write to the root fail there by design.

## Reproduce

```sh
cp env.example .env && $EDITOR .env
make lab-tools lab-keys rpms-lab repo iso-fetch iso
make lab-install lab-measure lab-snapshot-test lab-rollback-test lab-sb-test
FEDORA_RELEASE=45 make rpms repo
VM_NAME=<name> VM_HOST=11 SITE_DIR=<site with BASALT_ENCRYPT=0> make lab-install   # second VM
VM_NAME=<name> VM_HOST=11 scripts/lab/upgrade-test.sh 45
```
