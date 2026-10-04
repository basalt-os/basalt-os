# Publishing to obpkg.org

The Basalt OS repository is served from https://obpkg.org/basalt/, a
Cloudflare R2 bucket behind the obpkg.org domain. A publish has three
steps, each on the machine that fits it. The scripts are in
`scripts/release/`.

| Step | Script | Runs on | Holds |
|---|---|---|---|
| Build | `build.sh OUT_DIR` | a build host | no keys |
| Sign | `sign.sh --op IN_DIR OUT_DIR` | the release signer | the packages signing subkey, in memory, for the length of the run |
| Upload | `upload.sh OUT_DIR` | the release signer | the bucket's publish credentials |

## Layout

`sign.sh` writes the tree exactly as it appears under https://obpkg.org/:

```
basalt/<releasever>/<arch>/       binary RPMs, repodata/, repodata/repomd.xml.asc
basalt/<releasever>/source/       source RPMs, repodata/, repodata/repomd.xml.asc
```

This is what `basalt.repo` in `basalt-release` reads:
`baseurl=$basalt_repo_url/$releasever/$basearch/` with
`basalt_repo_url=https://obpkg.org/basalt`, `gpgcheck=1` and
`repo_gpgcheck=1`. The public key is published once at
https://obpkg.org/keys/openbasalt-release-key.asc and shipped by
`basalt-release` as `RPM-GPG-KEY-basalt`.

## Build

`build.sh` builds every package from a clean checkout of a commit:
`scripts/build-rpms.sh`, `basalt-llm`, and the data packages
`basalt-knowledge` and `basalt-vsm-planner` (from `BASALT_ARTIFACTS_DIR` or
`BASALT_ARTIFACTS_URL`). It ignores `.env` and refuses the lab overrides
(development repository key, lab module certificates, lab repository
URLs), runs rpmlint, checks that `basalt-release` carries the release key
and the obpkg.org URL, and writes `SHA256SUMS` and `BUILD-INFO.txt`.

## Sign

`sign.sh` runs every tool in a Fedora container started with
`--network none` (the tool image is built before any key material
exists):

1. checks the input against `SHA256SUMS`, and that nothing is signed yet;
2. checks that `packages/basalt-release/RPM-GPG-KEY-basalt` equals the key
   published at obpkg.org;
3. reads the export of the packages signing subkey
   (`gpg --export-secret-subkeys`) and its passphrase into a 0700
   directory on a tmpfs: with `--op` from 1Password (`OB_OP_KEY_REF`, an
   `op://` reference, or `OB_OP_KEY_DOCUMENT`, a document item; and
   `OB_OP_PASSPHRASE_REF`), with `--key-file` from two
   0600 files on a tmpfs (`OB_SIGNING_KEY_FILE`,
   `OB_SIGNING_PASSPHRASE_FILE`);
4. imports it into a keyring on a tmpfs inside the container, refuses an
   export that contains the secret primary key, and signs with exactly
   the packages subkey (`302461D26520E077D07FFCA9AA27C62C36CCFC4B!`):
   `rpmsign --addsign` on every binary and source RPM, `createrepo_c`,
   and a detached armored signature of each `repomd.xml`;
5. shreds the key material;
6. verifies in a second container that never saw a secret, with the public
   key only: `rpmkeys --checksig` in a clean rpm database (each signature
   must name the packages subkey), `gpg --verify` of each
   `repomd.xml.asc` (VALIDSIG of the subkey), and dnf with `gpgcheck=1`
   and `repo_gpgcheck=1` reading the tree;
7. writes `SIGNED-OK` with the SHA-256 of every file.

Secret material is never on a command line, never printed and never
written outside the tmpfs.

`sign.sh --test-key` does the same with a throwaway key generated in the
container (same shape: certify-only primary, signing subkey, subkey-only
export). Its public key lands next to the tree as
`OUT_DIR.test-pubkey.asc` for client tests. A test tree is refused by
`upload.sh` for the obpkg bucket.

## Upload

`upload.sh` checks the tree against `SIGNED-OK`, then uploads source RPMs,
binary RPMs and repodata blobs first and each `repomd.xml` and its
signature last, so a client never sees metadata that points at missing
files.

| Objects | Cache-Control |
|---|---|
| `repomd.xml`, `repomd.xml.asc` | `public, max-age=60` |
| RPMs, repodata blobs (content-addressed names) | `public, max-age=2592000, immutable` |

This matches the obpkg.org cache rules (repository metadata 60 seconds,
artifacts 30 days at the edge). A published RPM or blob is never
replaced: an existing object with different content stops the upload;
an identical one is skipped. Credentials come from 0600 files named by
`OB_R2_ACCESS_KEY_ID_FILE`, `OB_R2_SECRET_ACCESS_KEY_FILE` and
`OB_R2_ENDPOINT_FILE` (or `OB_R2_ENDPOINT`); the AWS CLI configuration is
written to a tmpfs and shredded. `upload.sh --dry-run` prints the plan
without credentials.

## Client test

`client-test.sh SOURCE KEY [PACKAGE...]` installs packages in a fresh
Fedora container with `gpgcheck=1` and `repo_gpgcheck=1`, from a signed
tree (served on 127.0.0.1 for the test) or from a URL such as
https://obpkg.org/basalt, then checks that dnf refuses a tampered
`repomd.xml` (local trees) and a different key.

## Later publishes

The metadata from `sign.sh` lists only the packages of its input. A later
publish that keeps older versions available starts from the whole
published package set.
