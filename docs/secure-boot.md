# Secure Boot, signed modules and disk unlock

Status: milestone 1. How Basalt OS uses its own Secure Boot keys, signs
kernel modules, and decides when the encrypted disk unlocks by itself.
Measurements are in [milestone-1-report.md](milestone-1-report.md); the
offline key procedure is in [key-ceremony.md](key-ceremony.md).

## The boot chain

A Basalt OS install boots the way Fedora does:

```
UEFI firmware (db) -> shim (signed by Microsoft) -> GRUB (signed by Fedora) -> kernel (signed by Fedora)
                                   |                                               |
                                   +-- MokList (machine owner keys)                +-- modules (Fedora key built in,
                                                                                       Basalt module CA through MOK)
```

The firmware checks shim against its signature database (`db`). shim
checks GRUB and the kernel against the Fedora certificate built into it,
against `db`, and against the machine owner key list (MOK). The kernel
checks every module it loads. Basalt OS changes none of the Fedora
binaries; it adds keys at two points of this chain, in two modes.

| Mode | What Basalt adds | What it buys | What it costs |
|---|---|---|---|
| MOK mode (default path) | the Basalt kernel module CA, enrolled as a MOK | Basalt-built kernel modules load with Secure Boot and lockdown on | one confirmation at the console per machine (MokManager) |
| Custom db mode (advanced) | Basalt's own PK, KEK and db in the firmware, shim signed with the Basalt db key | the firmware runs only binaries Basalt signed; other vendors' boot loaders, including other Microsoft-signed shims, are refused | firmware access once per machine; every shim update must be signed by Basalt |

The two combine: custom db mode keeps shim, so the MOK list and the module
CA work the same way.

## Kernel lockdown and module signatures

Fedora kernels lock down (`integrity`) when Secure Boot is on, and then
refuse modules without a valid signature. Basalt OS also puts
`lockdown=integrity module.sig_enforce=1` on the kernel command line
(installer option `basalt.lockdown=0` leaves them out; not recommended,
see "Your own modules" below for the supported way), so that a machine
booted with Secure Boot off still refuses unsigned modules, `/dev/mem`
writes, unsigned kexec images and hibernation to an unverified image.
`integrity`, not `confidentiality`: the latter also blocks reading kernel
memory, which takes `perf`, kprobes and much of BPF with it.

Measured on the lab (milestone 1), with the test module of
`packages/lab/kmodtest` signed four ways:

| Module | Secure Boot on, no MOK | Secure Boot on, Basalt CA enrolled | Secure Boot off, command line enforcement |
|---|---|---|---|
| unsigned | refused | refused | refused |
| signed with an unknown key | refused | refused | refused |
| signed with the Basalt module CA key | refused | loads | see the report |
| signed with a Basalt signing certificate issued by the CA | refused | loads (certificate loaded at boot) | see the report |

## MOK mode: the Basalt kernel module CA

### Why a CA, and not a plain signing key

Fedora kernels are built with `CONFIG_INTEGRITY_CA_MACHINE_KEYRING` and
`CONFIG_INTEGRITY_CA_MACHINE_KEYRING_MAX`: a MOK only reaches the kernel's
`.machine` keyring, the one trusted for modules, when it is a CA
certificate (basic constraints CA, key usage `keyCertSign`, and no
`digitalSignature`). A plain self-signed signing certificate enrolled as a
MOK ends up in the `.platform` keyring, which the kernel never consults for
modules.

So the certificate Basalt asks machine owners to enroll is a CA:

- the Basalt kernel module CA (RSA 4096, key usage `keyCertSign, cRLSign`),
  enrolled once per machine. Its private key stays offline.
- module signing certificates issued by it (`digitalSignature`, code
  signing), which sign the `.ko` files Basalt builds. They ship in the
  `basalt-security` package (`/usr/lib/basalt/module-keys/`) and
  `basalt-module-keys.service` adds them to the kernel's
  `.secondary_trusted_keys` keyring at boot, before modules are loaded from
  the root file system. The kernel accepts them there only because the CA
  in `.machine` issued them.

A signing certificate can be replaced by a package update without another
trip to the console: only the CA needs the owner's confirmation. Modules
needed inside the initramfs would need the signing certificate loaded there
too; Basalt OS ships no such module yet.

### Enrolling the CA

```sh
basalt-secureboot status        # Secure Boot, keys, MOKs, lockdown, keyrings
basalt-secureboot enroll-mok    # mokutil --import, asks for a one-time password
reboot
```

At the next boot shim starts MokManager on the console (screen or serial
line): press a key within 10 seconds, choose "Enroll MOK", "Continue",
"Yes", type the one-time password, then "Reboot". Without that
confirmation nothing changes; that physical presence step is the point of
MOK. The lab drives it over the serial console
(`scripts/lab/mok-console.py`).

`basalt-secureboot unenroll-mok` queues the removal the same way.

### Effect on the TPM

Enrolling a MOK changes PCR 14 (shim measures its MOK lists there) and,
during the one boot that runs MokManager, PCR 4 (one more boot
application). PCR 7 does not change, so a disk sealed to PCR 7 keeps
unlocking without interaction. Measured in the lab.

### What Basalt signs

Nothing yet: Basalt OS ships no out-of-tree kernel modules today. The
pipeline exists and is tested in the lab (`scripts/lab/kmod-build.sh`
builds a module against the target kernel's `kernel-devel` and signs it
with `scripts/sign-file` from that kernel). Modules a user builds locally
(DKMS, akmods) are signed with that user's own MOK, next section.

## Your own modules: DKMS and akmods

Out-of-tree drivers built on the machine (NVIDIA through akmods, ZFS,
VirtualBox and other DKMS modules) are refused while lockdown and module
signature enforcement are on, with or without Secure Boot. Keep both on:
sign the modules with a key of your own and enroll its certificate as a
MOK, once per machine. Turning lockdown off (`basalt.lockdown=0`, or
removing the arguments with `grubby`) opens the kernel to any unsigned
module and is not the supported path.

The key must be shaped like a CA. Fedora kernels put a MOK into the
`.machine` keyring, the one trusted for modules, only when it is a CA
certificate (see "Why a CA" above); the self-signed code signing
certificate that akmods and DKMS generate by default lands in `.platform`
and its modules are still refused. Create one key and use it for both
signing and enrollment (the kernel does not check key usage when it
verifies a module signature; the lab measured that modules signed directly
with a CA key load):

```sh
sudo dnf install openssl mokutil keyutils
sudo install -d -m 0700 /etc/pki/basalt-mok
cd /etc/pki/basalt-mok
sudo openssl req -new -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes \
  -subj "/CN=$(hostname) module signing CA/" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -addext "subjectKeyIdentifier=hash" \
  -keyout mok.key -outform DER -out mok.der
sudo chmod 0600 mok.key
```

Point the build tool at it:

- akmods (Fedora's `akmods` package, used by RPM Fusion kmods) signs with
  `/etc/pki/akmods/private/private_key.priv` and
  `/etc/pki/akmods/certs/public_key.der`:

  ```sh
  sudo install -m 0640 -o root -g akmods /etc/pki/basalt-mok/mok.key /etc/pki/akmods/private/private_key.priv
  sudo install -m 0644 /etc/pki/basalt-mok/mok.der /etc/pki/akmods/certs/public_key.der
  sudo akmods --force --rebuild
  ```

- DKMS signs with the files named in `/etc/dkms/framework.conf`:

  ```sh
  echo 'mok_signing_key=/etc/pki/basalt-mok/mok.key' | sudo tee -a /etc/dkms/framework.conf
  echo 'mok_certificate=/etc/pki/basalt-mok/mok.der' | sudo tee -a /etc/dkms/framework.conf
  sudo dkms autoinstall
  ```

Enroll the certificate and confirm it at the console, as for the Basalt CA:

```sh
sudo mokutil --import /etc/pki/basalt-mok/mok.der   # asks for a one-time password
sudo reboot                                          # MokManager: Enroll MOK, Continue, Yes, password, Reboot
mokutil --list-enrolled | grep -A1 Subject           # after the reboot
sudo keyctl list %:.machine                          # the certificate is in .machine
cat /sys/kernel/security/lockdown                    # still [integrity]
```

Then `modprobe` the module; `basalt-secureboot status` shows the lockdown
state and the keyrings. The TPM effect is the one described above: PCR 14
changes, PCR 7 does not, so a disk sealed to PCR 7 keeps unlocking
unattended. Keep `mok.key` on the machine only as long as modules are
built there (akmods and DKMS rebuild on every kernel update, so usually for
good), readable by root only, and inside the encrypted root.

## Custom db mode: the firmware trusts only Basalt

In custom db mode the firmware's Secure Boot keys are replaced:

- PK (platform key) and KEK (key exchange key): Basalt's, so only Basalt
  can update `db` and `dbx` from the running system;
- db: only the Basalt db certificate (optionally also Microsoft's UEFI CA,
  see below);
- dbx (revocations): kept as shipped, with the firmware vendor's and
  Microsoft's revocations.

shim, as Fedora ships it, is signed only by Microsoft. Basalt adds its own
signature with the db key (`sbsign` appends; the Microsoft signatures stay),
so the same file boots under stock keys and under custom keys. GRUB and the
kernel keep their Fedora signatures: shim checks them with its built-in
Fedora certificate. Kernel updates from Fedora therefore keep working
unchanged in custom db mode; only shim updates need Basalt's signature.

What it changes:

- The firmware refuses every boot loader Basalt did not sign: other
  distributions' shims, Windows' boot manager, old vulnerable boot loaders
  that Microsoft once signed. In the lab the stock Fedora installer ISO
  (shim signed by Microsoft only) is refused with "Access Denied".
- PCR 7 changes (it measures PK, KEK, db and the certificate that verified
  each boot loader), so the TPM2 disk key must be sealed again (below).
- Option ROMs: plug-in cards (network, RAID, GPU) carry firmware signed by
  Microsoft's UEFI CA. A db without that CA may stop them from
  initializing. On such machines add Microsoft's UEFI CA to db and accept
  that it then also trusts other Microsoft-signed boot loaders. Virtual
  machines and hardware without add-in cards do not need it.

Applying it needs the firmware in setup mode once (the firmware setup
screen: "clear Secure Boot keys" or "reset to setup mode"), then the signed
key update files from the key ceremony are written from the running system
or from the firmware's own key management screen. The lab writes them
straight into the VM's variable store (`scripts/lab/vm.sh sb custom`).

### Order of operations

1. Install shim with the Basalt signature (a Basalt package in the future,
   see the report); check it: `sbverify --list /boot/efi/EFI/fedora/shimx64.efi`.
2. `basalt-tpm suspend` (below), so the next boot does not need the
   recovery key.
3. Enroll PK, KEK and db.
4. Boot. The TPM2 key is sealed again to the new PCR 7 on that boot.

Going back: restore the firmware's default keys (setup screen); the
re-signed shim still carries Microsoft's signature, so it keeps booting.

### Not chosen: unified kernel images signed by Basalt

The alternative chain, firmware -> a unified kernel image (kernel,
initramfs and command line in one signed EFI binary), would bind the
command line and the initramfs to the signature and enable signed PCR 11
policies (next section). It is not used because:

- a UKI's command line is fixed when Secure Boot is on, and the snapshot
  boot menu works by passing `rootflags=subvol=<snapshot>`. Snapshot boot
  entries would need one signed image or add-on per snapshot, that is a
  signing key on every machine;
- every kernel and initramfs update would need a Basalt signature, so
  Basalt would effectively ship its own kernel packages.

It stays an option for a later image with a different recovery story.

## Disk unlock and the TPM

### Unlock methods (installer option `basalt.unlock=`)

| Method | Unlocks without interaction when | Use |
|---|---|---|
| `tpm2` (default) | this machine's TPM and an unchanged Secure Boot state (PCR 7) | servers and desktops with a TPM |
| `tang` | a Tang server of the site answers (Clevis in the initramfs) | servers without a TPM, or where the disk must not open off the site network |
| `tpm2+tang` | both: this TPM with PCR 7 unchanged and the Tang server (Clevis, Shamir threshold 2) | servers that must open only on this machine and only on this network |

Every method also enrolls a recovery key, typed at the console prompt when
the automatic unlock fails. A stolen `tpm2+tang` disk does not open, and
neither does the whole machine carried off the network; a machine
booted with a different Secure Boot state does not open even on the site
network.

Tang needs the network in the initramfs: the installer adds
`rd.neednet=1` to the kernel command line and installs `clevis`,
`clevis-luks` and `clevis-dracut`. Pin the Tang server's key thumbprint
(`basalt.tang-thp=`, from `tang-show-keys` on the server); without it the
installer trusts the first advertisement and logs it. When Tang is not
reachable the boot waits at the prompt and Clevis keeps trying: in the lab
the boot continued by itself 9 to 12 seconds after Tang came back.

### Why PCR 7, and what it does not cover

The TPM2 key is sealed to PCR 7 only. PCR 7 holds the Secure Boot state:
enabled or not, PK, KEK, db, dbx, shim's SBAT level, and the certificates
that verified each boot loader. What it does not hold is which signed boot
loader or kernel ran: any boot chain verified by the same certificates
produces the same PCR 7. With stock keys that includes every Fedora-signed
kernel and Fedora's own installer media. Custom db mode narrows that to
what Basalt signed; Tang (`tpm2+tang`) adds a second factor the machine
alone cannot provide.

Other PCRs were measured in the lab on the Basalt boot chain (shim, GRUB,
BLS kernel and initramfs):

| PCR | Holds | Changes on |
|---|---|---|
| 4 | boot loader and kernel images | every kernel update; booting an older kernel (snapshot boot, rollback across a kernel update); the boot that runs MokManager |
| 7 | Secure Boot state and verifying certificates | Secure Boot keys, dbx, SBAT level, Secure Boot on/off; not kernel updates, snapshot boots or rollbacks |
| 8 | GRUB commands and the kernel command line | every boot menu change; snapshot boot (different command line) |
| 9 | files GRUB reads: kernel, initramfs, configuration | every kernel or initramfs update; the snapshot menu file, rewritten after every dnf transaction |
| 11 | unified kernel image sections and boot phases | unused here (no UKI) |
| 14 | shim's MOK lists | MOK enrollment |

Binding 4, 8 or 9 means the disk stops unlocking by itself after routine
events: every update, every rollback, every boot of a snapshot from the
menu, which is exactly when an unattended server most needs to come back
on its own. Predicting the next values before an update is what
`systemd-pcrlock` is for, but on this chain it recognizes only the
trivial PCRs (3 and 6 in the lab): the shim, GRUB and kernel images in PCR 4
and the 72 GRUB command events in PCR 8 have no matching components, and
GRUB's configuration changes after every transaction. Signed PCR 11
policies need unified kernel images (see above). Re-sealing after a kernel
update or rollback cannot be done without a prediction, because the new
values only exist after the reboot.

Decision for now: PCR 7 stays, with custom db mode and Tang as the ways to
narrow it. Revisit when the boot chain moves to signed unified kernel
images.

### Changing the Secure Boot state: basalt-tpm

`basalt-tpm` (package `basalt-security`) shows and repairs the TPM2 unlock.
Every change shows the exact commands and asks for confirmation.

```sh
basalt-tpm status            # key slots, PCRs per TPM2 key, whether each unseals now, Clevis bindings
basalt-tpm pcrs              # current PCR values
basalt-tpm suspend           # before a planned change: one boot without PCR binding
basalt-tpm reenroll          # after a change: seal again (TPM if it still unseals, else the recovery key)
```

- Planned change (new Secure Boot keys, a firmware update, a dbx update,
  a shim update that raises the SBAT level): `basalt-tpm suspend` adds a
  TPM2 key bound to no PCR and enables `basalt-tpm-resume.service`. The next
  boot unlocks with it whatever PCR 7 now is; the service then seals a new
  key to the new PCR 7, removes the stale and the unbound keys and disables
  itself. If Secure Boot is off on that boot, it removes the unbound key
  without sealing anything, so the next boot needs the recovery key. The
  window is one boot, the same trade-off as suspending BitLocker for a
  firmware update.
- Unplanned change: the boot stops at the prompt; type the recovery key,
  then `basalt-tpm reenroll` (it asks for the recovery key again, or takes
  `--recovery-key-file`).
- `systemd-cryptenroll` skips an enrollment whose PCR set is already
  enrolled, even when the sealed values are stale; `basalt-tpm reenroll`
  therefore removes the stale key first, then enrolls, then removes any
  other old TPM2 key. The recovery key slot is never touched.

The PCR list comes from `/etc/basalt/tpm.conf` (`TPM_PCRS=7`).

## Keys

Development keys (the lab): `scripts/lab/sb-keys.sh` creates a PK, KEK, db,
the module CA and a module signing certificate under the lab directory,
private keys mode 0600, never printed.

Release keys: created offline in a key ceremony
([key-ceremony.md](key-ceremony.md)) and never stored on a build host.
`basalt-security` ships their public certificates:

| File | Certificate | SHA-256 fingerprint |
|---|---|---|
| `/usr/share/basalt/secureboot/basalt-module-ca.der` | OpenBasalt Kernel Module CA (RSA 4096, CA, `keyCertSign, cRLSign`, valid to 2036-10-01): the MOK `basalt-secureboot enroll-mok` enrolls | `15:5E:5E:7C:FE:7C:19:4C:9C:AD:E7:4F:32:0A:FE:11:57:D7:F8:B2:68:0A:93:A2:25:2F:7D:FA:DC:36:C2:DF` |
| `/usr/lib/basalt/module-keys/basalt-module-signing.der` | OpenBasalt Kernel Module Signing 2026 (issued by the CA, `digitalSignature`, code signing, valid to 2028-10-03): loaded into `.secondary_trusted_keys` at boot | `79:DF:9F:05:C4:96:30:57:5F:59:1D:C4:2D:FA:28:8D:CF:AC:E0:A5:C6:99:4A:9B:BA:06:B8:BB:D0:E9:96:FE` |

Check a certificate before enrolling it:

```sh
openssl x509 -inform DER -in /usr/share/basalt/secureboot/basalt-module-ca.der -noout -subject -fingerprint -sha256
```

The MOK flow is the one above: the module CA is the certificate machine
owners enroll; signing certificates follow by package updates. On a
machine where the CA is not enrolled, `basalt-module-keys.service` reports
that the signing certificate was not loaded and boot goes on.

Lab and CI: lab builds replace both certificates with the development ones
of `scripts/lab/sb-keys.sh` through `BASALT_MODULE_CA_CERT` and
`BASALT_MODULE_SIGNING_CERT` in `.env` (an explicit override, logged by
`scripts/build-rpms.sh`), because the lab signs its test modules with those
keys (`scripts/lab/kmod-build.sh`, `scripts/lab/mok-test.sh`). Without the
override a build ships the OpenBasalt certificates.
