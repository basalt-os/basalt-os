# Lab

One Linux host (KVM, libvirt, swtpm, OVMF, podman, docker) runs the whole
loop: build the packages, sign the repository, build the ISO, install it
unattended into a VM with Secure Boot firmware and an emulated TPM, and test
updates, snapshots, rollback and the encrypted disk. Every host-specific
value lives in `.env` (see `env.example`); lab state lives under `LAB_DIR`,
outside this repository.

The VMs only ever use an emulated TPM (swtpm). Never point any of these
scripts at the host's own TPM or disks.

## One-time setup

```sh
make lab-tools      # tool image: virt-fw-vars (OVMF variable stores), sbsign, efitools
make lab-keys       # dev signing key, lab SSH key, site files for the lab ISO
make lab-sb-keys    # dev Secure Boot keys: PK, KEK, db, module CA, module signing certificate
make rpms-lab repo  # packages (plus the canary test package), signed repository
make iso-fetch iso  # Fedora netinst ISO (verified) -> Basalt lab ISO
```

`LAB_DIR` layout:

| Path | Content | Mode |
|---|---|---|
| `gpg/` | development signing key, `passphrase`, `keyid`, `RPM-GPG-KEY-basalt-lab` | dir 0700, secrets 0600 |
| `keys/` | `vm_ed25519`, `authorized_keys` | dir 0700 |
| `site/` | `site.conf` (encryption on, power off when done, lab repository URL), `site.ks` (root SSH keys, no password) | 0644 |
| `repo/` | the signed repository; `repo/lab/` holds test fixtures only | |
| `recovery/<vm>.txt` | LUKS recovery key of each lab install | dir 0700, file 0600 |
| `sb/` | saved VM variable stores, a foreign test certificate | 0700 |
| `sb-keys/` | development PK, KEK, db, module CA and module signing keys and certificates, owner GUID | dir 0700, keys 0600 |
| `kmod/<kernel>/` | the test module built for a kernel, unsigned and signed three ways | |
| `tang/` | the lab Tang server's keys | dir 0700, keys 0600 |
| `site-<name>/` | site file variants for extra lab ISOs (`site-variant.sh`) | 0644 |

## Network and repository

`vm.sh net-up` defines an isolated NAT network (`VM_SUBNET`.0/24, fixed
addresses `.10` to `.19` by MAC, added to an existing network too). `repo-serve.sh up` serves `REPO_DIR` with
a busybox httpd container bound to the network's gateway address, so only lab
VMs reach it. HTTP is enough: packages and metadata are signed and checked.

`tang-serve.sh up` runs a Tang server (Fedora container, socat and
`tangd`) on the same gateway address, port `TANG_PORT`, for network
unlock tests; `tang-serve.sh thp` prints the thumbprint clients pin.

## Install

```sh
make lab-install          # scripts/lab/install.sh
```

1. `vm.sh install ISO`: a q35 VM with OVMF Secure Boot firmware (Microsoft
   and Red Hat keys enrolled), swtpm TPM 2.0 (CRB), a blank virtio disk and
   the ISO. The vTPM state is created before the first start (see host
   notes). The kickstart installs and powers the VM off; the ISO is removed.
2. First boot: the disk unlocks through the TPM; the script waits for SSH.
3. The recovery key is copied to `LAB_DIR/recovery/<vm>.txt` and deleted from
   the VM.

A second VM (for example an unencrypted install or a release upgrade test)
uses the same scripts with `VM_NAME=<name> VM_HOST=11`. Other install
choices get their own ISO from a site variant:

```sh
scripts/lab/site-variant.sh tang BASALT_UNLOCK=tang BASALT_TANG_URL=http://<gateway>:7500 \
  BASALT_TANG_THP="$(scripts/lab/tang-serve.sh thp)" BASALT_PROFILE=minimal
SITE_DIR=$LAB_DIR/site-tang SITE_NAME=lab-tang make iso
VM_NAME=<name> VM_HOST=13 scripts/lab/install.sh build/iso/basalt-os-<version>-x86_64-lab-tang.iso
```

## Tests

```sh
make lab-snapshot-test    # dnf install/upgrade: pre/post snapshots, packages persist across reboot
make lab-rollback-test    # canary updates: rollback from the system, then from the GRUB menu
make lab-sb-test          # Secure Boot disabled / foreign db cert: TPM refuses, recovery key works
make lab-measure          # ISO size, installed size, boot time, idle memory, denials, snapshot space
make lab-mok-test         # MOK mode: module CA enrolled through MokManager, module signature matrix
make lab-sb-custom-test   # custom db mode: own PK/KEK/db, re-signed shim, TPM suspend and reenroll
make lab-tang-test        # Tang and TPM2+Tang unlock: server down, back, recovery key, PCR 7 change
make lab-kernels-test     # orphaned kernels after a rollback: detection and cleanup
scripts/lab/upgrade-test.sh 45 && scripts/lab/upgrade-rollback-test.sh   # release upgrade, then back and forth
```

`mok-test.sh --sb-off` also boots once with Secure Boot disabled to check
the command line enforcement. Each test logs PCR values where they matter
and counts SELinux denials after every boot.

The rollback test drives the GRUB menu over the serial console
(`grub-console.py`): it opens the "Basalt OS snapshots" submenu with its
hotkey `s`, moves with Ctrl-N and boots with Ctrl-F, all single bytes (an
arrow key's escape sequence split by the serial line reads as a lone ESC
and opens GRUB's prompt; that made milestone 0's runs flaky). It then
requires GRUB's "Booting snapshot N" line for the expected N, so a wrong
selection fails instead of booting another snapshot. `--top N` boots a
top-level entry, for example an older kernel. `console-unlock.py` types the
recovery key at the LUKS prompt, `mok-console.py` confirms a MOK request
in MokManager with the one-time password, `serial-watch.py` waits for a
line. None of them prints key material. Firmware messages printed before a
console attaches are read from the serial log libvirt keeps.

## Host notes

- vTPM manufacturing: on Fedora 44 hosts libvirt's own `swtpm_setup` run
  fails with an SELinux denial (`swtpm_t` writing its pidfile in virtqemud's
  private `/tmp`). `vm.sh` therefore defines the VM, creates the TPM state
  with `swtpm_setup` itself, then starts it; libvirt then only runs `swtpm`.
- Docker on the same host sets the iptables `FORWARD` policy to `DROP`,
  which also drops the libvirt network's NAT traffic (VMs reach the gateway
  but not the internet). `vm.sh net-up` inserts two rules in Docker's
  `DOCKER-USER` chain that accept forwarding for the lab bridge only; they
  are lost when Docker restarts, so run `make lab-net` again after that.
- Builds use the host network: on some hosts DNS does not resolve inside
  rootful podman's default bridge network.
- The repository container binds to the lab bridge address, which exists
  only while the libvirt network is up; after a host reboot run
  `make lab-repo` again if it did not start.
- Disk images live in `VM_DIR`, which must be readable by the qemu user (a
  home directory with mode 0700 is not).

## Removing the lab

| Resource | Remove with |
|---|---|
| VM (libvirt definition, NVRAM, vTPM state, disk) | `VM_NAME=<name> scripts/lab/vm.sh destroy` |
| Repository server | `make lab-repo-down` |
| Network | `virsh net-destroy $VM_NETWORK && virsh net-undefine $VM_NETWORK` |
| Keys, repository, site files, recovery keys, logs | `rm -rf "$LAB_DIR"` |
| Tang server | `scripts/lab/tang-serve.sh down` |
| Tool images | `sudo podman rmi localhost/basalt-lab-tools localhost/basalt-lab-tang localhost/basalt-iso-tools:44` |
| Serial logs | `sudo rm /var/log/libvirt/qemu/$VM_NAME-serial.log` |
