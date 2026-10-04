# Key ceremony

Status: the OpenBasalt release key exists (2026-10-04). Its primary key
(certify only) has the fingerprint `3601734842BD4E482D19DE4AE4EED5ECA395B302`;
the signing subkey for packages is `302461D26520E077D07FFCA9AA27C62C36CCFC4B`.
The public key is published at https://obpkg.org/keys/openbasalt-release-key.asc
and `basalt-release` ships it as `RPM-GPG-KEY-basalt`. The kernel module CA
and the first module signing certificate exist too; `basalt-security` ships
their certificates (fingerprints in [secure-boot.md](secure-boot.md#keys)).
The lab and CI still
sign with development keys that never leave their host. The rest of this
document is the procedure for release keys and for using them afterwards.

## Keys

| Key | Algorithm | Signs | Lives | Used |
|---|---|---|---|---|
| Repository key | RSA 4096 (OpenPGP) | RPM packages and repository metadata | offline primary key; a signing subkey on the release signer | every repository publish |
| Secure Boot PK | RSA 2048 (X.509) | updates of KEK | offline only | when KEK changes (rare) |
| Secure Boot KEK | RSA 2048 | updates of db and dbx | offline only | when db or dbx change |
| Secure Boot db | RSA 2048 | shim (custom db mode) | offline; or a release signer, see below | each shim release |
| Kernel module CA | RSA 4096, CA, keyCertSign only | module signing certificates | offline only | when a signing certificate is issued |
| Module signing certificate | RSA 4096, digitalSignature, code signing | kernel modules (.ko) | release signer | every module build |

RSA 2048 for the UEFI keys because every firmware supports it; some
reject larger keys. The module CA carries no `digitalSignature` usage:
Fedora kernels only trust a MOK CA without it.

Validity: PK, KEK and db 10 years; module CA 10 years; module signing
certificates 2 years, rotated by a package update.

## Material and roles

- An air-gapped machine: no network hardware enabled, booted from a
  verified live medium, its disk not used.
- Two hardware tokens (smart card or HSM token able to hold RSA keys)
  or, at the start, two encrypted USB drives: primary and backup, kept in
  different places.
- A passphrase for each, stored in the project's password vault, split so
  that no single person holds a token and its passphrase.
- Two people: one operates, one reads the checklist and verifies every
  fingerprint aloud. Both sign the ceremony log.
- A printed copy of this procedure and a blank log.

## Procedure

1. Boot the air-gapped machine; record the live medium's checksum in the
   log.
2. Generate the keys (`openssl req -x509` with the key sizes above and the
   extensions of `scripts/lab/sb-keys.sh`, release subject names, and
   `gpg --full-generate-key` for the repository key), directly on the
   token when it supports key generation, otherwise in a RAM-only
   directory.
3. Issue the first module signing certificate from the module CA.
4. Export the public parts: certificates in PEM and DER, the OpenPGP public
   key, fingerprints. These go into the repository (`basalt-security`,
   `basalt-release`) and onto the website.
5. Create the signed UEFI update files (`.auth`) for custom db mode: PK
   signed by PK, KEK signed by PK, db signed by KEK, with a fixed owner
   GUID recorded in the log (`cert-to-efi-sig-list`, `sign-efi-sig-list`
   from efitools). These let a machine in setup mode take the keys from the
   running system.
6. Sign the current Fedora shim with the db key (`sbsign` appends to the
   Microsoft signatures) and record its SHA-256 before and after.
7. Write private keys to the primary and the backup token; verify that
   each token can sign; wipe the RAM directory; power off.
8. Both people sign the log, which lists every fingerprint, serial number
   and checksum. The log is kept with the backup token; a scan goes to the
   vault.

## Routine use

- Repository and module signing run on a release signer: a dedicated
  machine (or a hardware token attached to the release pipeline) holding
  only the repository signing subkey and the module signing key. It never
  holds the PK, KEK, db or module CA private keys.
- A new shim release (Fedora's) is signed with the db key in a short
  ceremony with the token, then shipped in a Basalt package.
- A new module signing certificate is issued by the module CA in a short
  ceremony, then shipped in `basalt-security`.

## Compromise and revocation

| Compromised | Do |
|---|---|
| Module signing certificate | ship a new certificate in `basalt-security` (old one no longer loaded at boot); rebuild modules; optionally add its hash to the MOK revocation list (MokListX) |
| Module CA | new CA; every machine enrolls it at the console (MokManager) and removes the old one (`basalt-secureboot unenroll-mok`) |
| db key | new db key: update db with a KEK-signed file, add the old certificate to dbx, re-sign and ship shim |
| KEK or PK | new keys; every custom db machine goes through setup mode again |
| Repository key | revoke it with its revocation certificate, publish a new key in a `basalt-release` update signed with the old key while it is still trusted, re-sign the repository |

Every compromise is announced with the fingerprints involved.
