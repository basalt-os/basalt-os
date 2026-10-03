# Lab

A single Linux host (KVM, libvirt, swtpm, OVMF, podman, docker) runs the whole
loop: build, sign, publish to a private registry, install into a VM with
Secure Boot firmware and a TPM, update, roll back. Every value specific to the
host lives in `.env` (see `env.example`); lab state lives under `LAB_DIR`,
outside this repository.

## One-time setup

```sh
make lab-tools      # cosign (pinned, checksum verified) and a tool image with virt-fw-vars
make lab-keys       # dev cosign key pair, lab SSH key, authorized_keys
make lab-registry   # registry container: TLS (lab CA), htpasswd, bound to REGISTRY_BIND:REGISTRY_PORT
```

`LAB_DIR` layout:

| Path | Content | Mode |
|---|---|---|
| `bin/cosign` | cosign binary | 0755 |
| `keys/` | `cosign.key`, `cosign.password`, `cosign.pub`, `vm_ed25519`, `authorized_keys` | dir 0700, secrets 0600 |
| `registry/ca/` | lab CA key and certificate (not mounted into the registry) | 0700 |
| `registry/certs/`, `registry/auth/` | server certificate, htpasswd, password | 0700 |
| `registry/client-certs/ca.crt` | CA for clients (`--cert-dir`) | 0644 |
| `registry/auth.json` | registry credentials for podman, skopeo and cosign | 0600 |
| `recovery/<vm>.txt` | LUKS recovery key of each lab install | dir 0700, file 0600 |
| `sb/` | saved VM variable store, foreign test certificate | 0700 |

The lab CA is trusted only for the registry address, through
`/etc/containers/certs.d/<registry>/ca.crt` in images built for the lab, and
through `--cert-dir` on the build host. Nothing is added to the host's system
trust store.

## Install into a VM

```sh
make release                    # build, sign, publish, verify
scripts/lab/e2e-install.sh      # all of the steps below, timed
```

1. `vm.sh installer-disk`: `bootc install to-disk --via-loopback` writes the
   local image to a raw disk (plain btrfs). This is the installer.
2. `vm.sh create`: a q35 VM with OVMF Secure Boot firmware and Microsoft and
   Red Hat keys enrolled (`secure-boot` and `enrolled-keys` firmware features),
   an emulated TPM 2.0 (swtpm, CRB), the installer disk and a blank target
   disk, on an isolated NAT network.
3. `install-target.sh`: inside the VM, pulls the image from the registry (the
   VM's signature policy is enforced on this pull) and runs `basalt-install` on
   the target disk. The recovery key is saved to `LAB_DIR/recovery/`.
4. `vm.sh detach-installer`, `vm.sh start`: the installed system boots and
   unlocks its disk with the TPM.
5. `vm.sh registry-auth`: lab registry credentials for `bootc upgrade`.

## Tests

```sh
IDLE_SECONDS=120 scripts/lab/measure.sh "label"   # image size, boot, memory, denials, storage
scripts/lab/sb-test.sh                            # Secure Boot changes block unlock; recovery key works
NEW_VERSION=0.0.2 scripts/lab/update-test.sh      # build, publish, upgrade, unsigned refused, rollback
```

## Secure Boot state experiments

The VM must be shut off (`vm.sh stop`). The first change saves the original
variable store.

```sh
scripts/lab/vm.sh sb disable      # SecureBootEnable = false
scripts/lab/vm.sh sb foreign-db   # add a self-made certificate to db
scripts/lab/vm.sh sb restore      # back to the saved store
scripts/lab/console-unlock.py VM                        # report the passphrase prompt
scripts/lab/console-unlock.py VM --key-file KEYFILE     # type the recovery key
```

Either change alters PCR 7, so the TPM refuses to unseal and the boot stops at
the recovery prompt on the serial console. Restoring the store restores
automatic unlock.

## Host notes

- vTPM manufacturing: on the lab host (Fedora 44, libvirt 12, swtpm 0.10)
  libvirt's own `swtpm_setup` run fails with an SELinux denial (`swtpm_t`
  writing its pidfile in virtqemud's private `/tmp`). `vm.sh create` therefore
  defines the VM, creates the TPM state with `swtpm_setup` itself, then starts
  it; libvirt then only runs `swtpm`, which works.
- Builds use `--network=host`: on the lab host, DNS does not resolve inside
  rootful podman's default bridge network (systemd-resolved stub resolver).
- Disk images live in `VM_DIR`, which must be readable by the qemu user (a
  home directory with mode 0700 is not).
