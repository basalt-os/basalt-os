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

### Other repositories

The same scripts publish the `basalt-tools` repository (OpenBasalt tools
such as Samba Conductor, built in their own repositories) and
`basalt-testing`: set `OB_REPO=basalt-tools` (or `basalt-testing`) for
`sign.sh` and `client-test.sh`. The tree is then

```
basalt-tools/<releasever>/<arch>/   binary and noarch RPMs, repodata/, repodata/repomd.xml.asc
basalt-tools/<releasever>/source/   only when the input has source RPMs
```

matching `baseurl=$basalt_tools_url/$releasever/$basearch/` with
`basalt_tools_url=https://obpkg.org/basalt-tools`, written by the
installer in `basalt-tools.repo` with `gpgcheck=1`, `repo_gpgcheck=1` and
the same key (one packages signing subkey signs every repository).
`sign.sh` takes any directory of unsigned RPMs with a `SHA256SUMS` that
lists exactly them, so a tools build (for Samba Conductor, its x86_64
and noarch RPMs) is signed the same way. `sign.sh` records the repository in
`SIGNED-OK` (`repo:`) and `upload.sh` publishes that repository only.
`client-test.sh` needs the package names for a repository other than
`basalt`; dependencies come from Fedora's repositories in the test
container, as on an installed system.
Where rootless podman cannot reach the network while building (pasta
sandbox errors), build the signer image once by hand with
`podman build --network=host -t localhost/basalt-signer:<releasever>`;
`sign.sh` reuses an existing image.

### basalt-testing

Pre-release packages go to `basalt-testing`, which `basalt-release` ships
off by default (`[basalt-testing]`, `gpgcheck=1`, `repo_gpgcheck=1`, the
same key; `dnf config-manager setopt basalt-testing.enabled=1` turns it
on). Today it holds the desktop shell, built from
[basalt-os/basalt-shell](https://github.com/basalt-os/basalt-shell) at the
commit pinned in `packages/basalt-shell/source.conf`, and `basalt-voice`
(whisper.cpp speech to text for push to talk, see `docs/voice.md`; its
speech models are downloaded on the machine, never published here).
`scripts/release/build-testing.sh OUT_DIR` builds that set apart from the
basalt set (it refuses a local source or commit override), then the usual
steps with `OB_REPO=basalt-testing`:

```sh
scripts/release/build-testing.sh /tmp/testing-in
OB_REPO=basalt-testing scripts/release/sign.sh --op /tmp/testing-in /tmp/testing-out
OB_REPO=basalt-testing CLIENT_TEST_DEPS_URL=https://obpkg.org/basalt scripts/release/client-test.sh /tmp/testing-out packages/basalt-release/RPM-GPG-KEY-basalt basalt-shell basalt-shell-selinux basalt-voice
scripts/release/upload.sh /tmp/testing-out
```

`CLIENT_TEST_DEPS_URL` adds the published basalt repository to the test
container, because `basalt-shell-selinux` needs `basalt-agent-selinux`.

### basalt-nonfree

Non-free drivers that may be redistributed, today the NVIDIA driver
(docs/nvidia.md), go to `basalt-nonfree`, which `basalt-nonfree-release`
(in the basalt repository) defines off by default. Its kernel modules need
one more step, on the release signer, between two builds:

```sh
scripts/release/build-nonfree.sh modules /tmp/nvidia-modules
scripts/release/sign-modules.sh --op /tmp/nvidia-modules /tmp/nvidia-sigs
scripts/release/build-nonfree.sh packages /tmp/nvidia-sigs /tmp/nonfree-in
OB_REPO=basalt-nonfree scripts/release/merge-published.sh /tmp/nonfree-in
OB_REPO=basalt-nonfree scripts/release/sign.sh --op /tmp/nonfree-in /tmp/nonfree-out
OB_REPO=basalt-nonfree CLIENT_TEST_DEPS_URL=https://obpkg.org/basalt scripts/release/client-test.sh /tmp/nonfree-out packages/basalt-release/RPM-GPG-KEY-basalt nvidia-driver-compute
scripts/release/upload.sh /tmp/nonfree-out
```

- `build-nonfree.sh modules` builds the open kernel modules for every
  kernel of the release, unsigned, without network, in a build image made
  for the driver version; `sign-modules.sh` (modes `--op`, `--key-file`,
  `--test-key`, like `sign.sh`) signs them with the basalt-nonfree module
  signing key in a container without network and returns signatures
  only; `build-nonfree.sh packages` builds every package, checks that each
  module it builds is byte-identical to the one signed before it appends
  the signature, runs `packages/nvidia/check-identical.sh` (NVIDIA files
  byte-identical to the `.run`, the Agreement in every NVIDIA package,
  signed modules) and rpmlint.
- The source packages carry the corresponding source: the `.run`, the open
  module archive with the signatures, the open tools' archives. They are
  published in `basalt-nonfree/<releasever>/source/` next to the binaries.
- `merge-published.sh` keeps the modules of older kernels that are still
  published; a newer `kmod-nvidia-open` (the package that follows the
  newest kernel) is published together with the module of a new kernel.
- A new Fedora kernel needs a new build of the module stages, a signing
  and a publish; until then dnf holds the kernel back on machines with the
  NVIDIA driver (docs/nvidia.md, Kernel updates).

## Build

`build.sh` builds every package from a clean checkout of a commit:
`scripts/build-rpms.sh` (with `basalt-nonfree-release`, whose
repository must point at https://obpkg.org/basalt-nonfree and be off,
and `basalt-models`, the desktop's consented model downloads), `basalt-llm`, `swayfx` (the desktop session's
compositor: SwayFX from its pinned release archive, Provides and
Conflicts with Fedora's sway, no Obsoletes, so switching stays explicit
with `dnf swap`), and the data packages
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

`upload.sh` checks the tree against `SIGNED-OK`, then uploads the RPMs
and repodata blobs first (`OB_UPLOAD_JOBS` at a time, default 4) and,
once all of them are in the bucket, each `repomd.xml` and its signature,
so a client never sees metadata that points at missing files.

| Objects | Cache-Control |
|---|---|
| `repomd.xml`, `repomd.xml.asc` | `public, max-age=60` |
| RPMs, repodata blobs (content-addressed names) | `public, max-age=2592000, immutable` |

This matches the obpkg.org cache rules (repository metadata 60 seconds,
artifacts 30 days at the edge). A published RPM or blob is never
replaced: an existing object with different content stops the upload;
an identical one is skipped. One listing of each repository directory
(`ListObjectsV2`) gives the ETag and size of every published object, so a
publish that keeps a hundred published RPMs makes a few requests instead
of one per RPM. The script uploads every object in a single part, whose
ETag is the MD5 of the content; an object with another ETag form is
compared through its `sha256` metadata (set on every upload) or, without
it, downloaded and compared. Any difference stops the upload before
anything is sent. A new object gets one more check (HEAD) right before
its upload, for an object published since the listing.
`scripts/release/tests/upload-test.sh` (`make upload-test`, and in
`scripts/ci/lint.sh`) runs `upload.sh` against a local fake S3 endpoint.

Credentials come from 0600 files named by
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

## APT repository (Debian and Ubuntu)

https://obpkg.org/apt/ carries OpenBasalt packages for Debian 13,
Ubuntu 26.04 and Ubuntu 24.04 (today Samba Conductor, built in its own
repositories). The packages are static builds, identical for every
supported release, so there is one suite for all of them: `stable`, and
`testing` for release candidates, component `main`, architectures amd64
and arm64. Per-release suites (trixie, noble and so on) would only multiply
metadata. The same three steps apply, with the APT variants of the sign
and client test scripts:

```sh
OB_APT_SUITE=stable scripts/release/merge-published-apt.sh /tmp/apt-in
OB_APT_SUITE=stable scripts/release/sign-apt.sh --op /tmp/apt-in /tmp/apt-out
scripts/release/client-test-apt.sh /tmp/apt-out packages/basalt-release/RPM-GPG-KEY-basalt conductor
scripts/release/upload.sh /tmp/apt-out
```

The input is a directory of unsigned .deb files with a `SHA256SUMS` that
lists exactly them, each under its canonical name
(`<package>_<version>_<arch>.deb`). The tree is

```
apt/pool/main/<letter>/<package>/<package>_<version>_<arch>.deb
apt/dists/<suite>/InRelease, Release, Release.gpg
apt/dists/<suite>/main/binary-<arch>/Packages, Packages.gz, Packages.xz, Release
apt/dists/<suite>/main/binary-<arch>/by-hash/SHA256/<sha256>
```

- `sign-apt.sh` keeps the key handling of `sign.sh` (subkey export only,
  tmpfs, a Debian 13 container with `--network none`), indexes the pool
  with `apt-ftparchive` and signs `Release` twice with exactly the packages
  subkey: `InRelease` (clear-signed) and `Release.gpg` (detached). It
  verifies in a second container with the public key only: gpgv (apt's
  verifier on Ubuntu 24.04) and sqv (Debian 13, Ubuntu 26.04), the
  `VALIDSIG` of the subkey, every checksum of `Release` and of the
  `Packages` files, and apt itself reading the tree offline through a
  `Signed-By` keyring. `SIGNED-OK` records `repo: apt` and the suite.
  `--test-key` works as in `sign.sh`.
- `Release` sets `Acquire-By-Hash: yes`: apt fetches each index by its
  SHA-256 from `by-hash/`, whose objects never change, so a client never
  combines a new `InRelease` with a `Packages` file still cached at the
  edge.
- `upload.sh` sends the .deb files and the `by-hash/` objects (in
  parallel), then the other index files, and `Release`, `Release.gpg` and
  `InRelease` last.
  .deb files and `by-hash/` objects are immutable (30 days, never
  replaced: different content stops the upload); the rest of `dists/` is
  metadata (60 seconds).
- `merge-published-apt.sh` is the APT form of `merge-published.sh`: it
  checks the published `InRelease` (packages subkey, release key), each
  `Packages` index against it and each .deb against its index, and puts
  every published .deb byte-identical into the input (`PUBLISHED`,
  `POOL-PATHS`), so the new index keeps every older version
  (`apt install <package>=<version>` goes back to one). A built .deb with
  the name of a published one is replaced by the published file; a
  changed package needs a new version.
- `client-test-apt.sh SOURCE KEY PACKAGE...` sets the repository up in
  fresh Debian 13, Ubuntu 26.04 and Ubuntu 24.04 containers as the
  obpkg.org page documents (the key dearmored into
  `/etc/apt/keyrings/openbasalt.gpg`, a deb822 `.sources` file with
  `Signed-By`), installs the packages, runs `<binary> version`, and checks
  that apt refuses a tampered `InRelease` (local trees) and another key.

## Images from the published repositories

`scripts/release/mirror-published.sh OUT_DIR` (with `OB_REPO`) copies a
published repository byte-identical: it checks the signature of
`repomd.xml` (packages subkey, release key), every metadata file against
the signed `repomd.xml` and every RPM against the signed primary metadata.
`LIVE_SOURCE=obpkg` image builds use it, so an image holds only packages
signed with the release key (see docs/installer.md).

## Later publishes

The metadata from `sign.sh` lists only the packages of its input. A later
publish that keeps older versions available starts from the whole
published package set: `merge-published.sh` runs between the build and
the signature.

```sh
scripts/release/build.sh /tmp/release-in
OB_REPO=basalt scripts/release/merge-published.sh /tmp/release-in
scripts/release/sign.sh --op /tmp/release-in /tmp/release-out
```

It reads `<repo>/<releasever>/{<arch>,source}/` from https://obpkg.org,
checks the signature of each `repomd.xml` (packages subkey, release key)
and every file against the checksums of that signed metadata, and puts
each published RPM, already signed, byte-identical into the input. A
built RPM with the same file name as a published one is replaced by the
published file (a published file is never replaced, so a changed package
needs a new release number; the script warns). `IN_DIR/PUBLISHED` lists
the published RPMs and `SHA256SUMS` is rewritten. `sign.sh` keeps those
RPMs as they are (no new signature), checks their signature with the
release key and indexes them with the new ones; `upload.sh` skips them as
already published and identical. A repository that is not published yet
adds nothing. In a `--test-key` dry run the tree then carries two keys:
client tests take both (`cat OUT_DIR.test-pubkey.asc
packages/basalt-release/RPM-GPG-KEY-basalt`).
