#!/usr/bin/env bash
# APT release signer: lay out, index and sign the Debian and Ubuntu
# repository that scripts/release/upload.sh publishes at https://obpkg.org/apt.
#
#   scripts/release/sign-apt.sh --op       IN_DIR OUT_DIR   key from 1Password (op), the real run
#   scripts/release/sign-apt.sh --key-file IN_DIR OUT_DIR   key from files (OB_SIGNING_KEY_FILE, OB_SIGNING_PASSPHRASE_FILE)
#   scripts/release/sign-apt.sh --test-key IN_DIR OUT_DIR   throwaway key made here (dry run, never published)
#
# IN_DIR is a directory of .deb files with a SHA256SUMS that lists exactly
# them (and optionally BUILD-INFO.txt). Each file must carry its canonical
# name, <Package>_<Version>_<Architecture>.deb. Packages listed in
# IN_DIR/PUBLISHED (scripts/release/merge-published-apt.sh: the packages
# already on obpkg.org) are kept byte-identical at their published path.
#
# OUT_DIR gets the tree as it appears under https://obpkg.org/:
#   apt/pool/main/<letter>/<package>/<file>.deb
#   apt/dists/<suite>/{InRelease,Release,Release.gpg}
#   apt/dists/<suite>/main/binary-<arch>/{Packages,Packages.gz,Packages.xz,Release}
#   apt/dists/<suite>/main/binary-<arch>/by-hash/SHA256/<sha256>
# <suite> is OB_APT_SUITE: stable (default) or testing (release candidates).
# One suite serves every supported distribution (Debian 13, Ubuntu 26.04 and
# 24.04): the packages are static builds, identical for all of them.
# Architectures: amd64 and arm64 (OB_APT_ARCHES); a package of architecture
# all is listed in each. Every package version of the input is indexed, so
# older versions stay installable (apt install <pkg>=<version>).
# The Release file sets Acquire-By-Hash: apt then fetches the indexes by
# their SHA-256 (immutable objects), so a client never mixes a new InRelease
# with a Packages file still cached at the edge. Release lists SHA-256 only:
# apt asks by-hash/ for the strongest hash listed, and only SHA256/ exists.
#
# Key material: exactly as scripts/release/sign.sh (read its header). The
# export of the packages signing subkey and its passphrase live only in a
# 0700 directory on a tmpfs and in a tmpfs keyring inside a container started
# with --network none; a full secret primary key is refused; InRelease and
# Release.gpg are made with exactly the packages subkey (OB_SIGNING_SUBKEY,
# forced with gpg's "!" suffix). Nothing secret is on a command line or
# printed.
#
# Verification uses only the public key (OB_RELEASE_PUBKEY, which must equal
# https://obpkg.org/keys/openbasalt-release-key.asc), in a second container
# that never sees secret material: gpgv and sqv (apt's verifier on Debian 13
# and Ubuntu 26.04) on InRelease and Release.gpg, the VALIDSIG of the subkey,
# every checksum of Release and Packages against the files, and apt itself
# (signed-by keyring, offline) reading the tree and fetching a package. On
# success it writes OUT_DIR/SIGNED-OK (repo: apt), which upload.sh requires.
source "$(dirname "$0")/../lib.sh"

: "${OB_SIGNING_SUBKEY:=302461D26520E077D07FFCA9AA27C62C36CCFC4B}"
: "${OB_RELEASE_PUBKEY:=$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt}"
: "${OB_PUBKEY_URL:=https://obpkg.org/keys/openbasalt-release-key.asc}"
: "${OP_ACCOUNT:=my.1password.com}"
: "${OB_OP_VAULT:=OpenBasalt}"
: "${SIGN_PODMAN:=podman}"
: "${OB_APT_SUITE:=stable}"
: "${OB_APT_ARCHES:=amd64 arm64}"
: "${OB_APT_LABEL:=OpenBasalt}"
: "${OB_APT_DESCRIPTION:=OpenBasalt packages for Debian 13, Ubuntu 26.04 and Ubuntu 24.04}"
# Debian 13, pinned by digest (multi-arch index), the distribution whose apt
# and sqv are the strictest of the supported ones.
: "${APT_SIGNER_BASE:=docker.io/library/debian:trixie@sha256:9cc080028c43b27d2074d63a5f9caf7166d731494965616c1a6d2827a004585c}"
APT_SIGNER_IMAGE="localhost/openbasalt-apt-signer:trixie"
case "$OB_APT_SUITE" in stable | testing) ;; *) die "OB_APT_SUITE must be stable or testing" ;; esac
for a in $OB_APT_ARCHES; do case "$a" in amd64 | arm64) ;; *) die "unsupported architecture $a" ;; esac; done
export OP_ACCOUNT

mode="${1:-}"
case "$mode" in --op | --key-file | --test-key) shift ;; *) sed -n '2,45p' "$0"; exit 2 ;; esac
in="${1:?IN_DIR}"
out="${2:?OUT_DIR}"
in="$(cd "$in" && pwd)" || die "no $in"

# Secret work directory on a tmpfs, removed (files shredded) on exit.
base="${XDG_RUNTIME_DIR:-/dev/shm}"
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || base=/dev/shm
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || die "no tmpfs for the key material"
umask 077
secret="$(mktemp -d "$base/openbasalt-apt-sign.XXXXXX")"
key_file="" pass_file="" lists=""
cleanup() {
  local f
  for f in "$secret"/* "$key_file" "$pass_file"; do
    [[ -n "$f" && -f "$f" ]] && shred -u "$f" 2>/dev/null
  done
  rm -rf "$secret"
}
trap 'cleanup; [[ -n "$lists" ]] && rm -rf "$lists"' EXIT

on_tmpfs() { [[ "$(stat -f -c %T "$1")" == tmpfs ]]; }
check_secret_file() {
  local f="$1"
  [[ -f "$f" && ! -L "$f" ]] || die "$f: not a regular file"
  on_tmpfs "$f" || die "$f: not on a tmpfs"
  [[ "$(stat -c '%a %u' "$f")" == "600 $(id -u)" ]] || die "$f: must be mode 0600 and owned by $(id -un)"
}

# 1. Input: every .deb listed in SHA256SUMS, checksums good.
[[ -f "$in/SHA256SUMS" ]] || die "$in/SHA256SUMS missing"
(cd "$in" && sha256sum -c --quiet SHA256SUMS) || die "checksum mismatch in $in"
mapfile -t debs < <(cd "$in" && find . -maxdepth 1 -name '*.deb' -printf '%P\n' | LC_ALL=C sort)
[[ ${#debs[@]} -gt 0 ]] || die "no .deb files in $in"
[[ "$(awk '{print $2}' "$in/SHA256SUMS" | LC_ALL=C sort)" == "$(printf '%s\n' "${debs[@]}")" ]] ||
  die "the .deb files in $in and SHA256SUMS differ"
find "$in" -maxdepth 1 -name '*.rpm' | grep -q . && die "$in has RPMs; this signer takes .deb files only"
presigned=()
if [[ -f "$in/PUBLISHED" ]]; then
  while read -r psum name; do
    grep -qxF "$psum  $name" "$in/SHA256SUMS" || die "$name in PUBLISHED does not match SHA256SUMS"
    presigned+=("$name")
  done <"$in/PUBLISHED"
fi

# 2. The public key the tree is verified with.
if [[ "$mode" != --test-key ]]; then
  [[ "${OB_SIGNING_SUBKEY^^}" =~ ^[0-9A-F]{40}$ ]] || die "OB_SIGNING_SUBKEY must be a fingerprint"
  published="$secret/published.asc"
  curl -fsSL --proto '=https' -o "$published" "$OB_PUBKEY_URL" || die "cannot fetch $OB_PUBKEY_URL"
  cmp -s "$published" "$OB_RELEASE_PUBKEY" || die "$OB_RELEASE_PUBKEY differs from $OB_PUBKEY_URL"
  rm -f "$published"
  pubkey="$OB_RELEASE_PUBKEY"
fi

# 3. Signer image: Debian 13 with the tools, built while no secret exists yet.
if ! $SIGN_PODMAN image exists "$APT_SIGNER_IMAGE"; then
  log "building $APT_SIGNER_IMAGE"
  printf 'FROM %s\nRUN apt-get update -qq && apt-get install -y -qq --no-install-recommends apt-utils dpkg-dev gnupg gpgv sqv xz-utils gzip coreutils >/dev/null && rm -rf /var/lib/apt/lists/*\n' "$APT_SIGNER_BASE" |
    $SIGN_PODMAN build --network=host -q -t "$APT_SIGNER_IMAGE" -f - >/dev/null 2>&1 ||
    die "cannot build $APT_SIGNER_IMAGE"
fi
signer() {
  $SIGN_PODMAN run --rm -i --network none --security-opt label=disable --tmpfs /gnupg:rw,mode=0700,size=32m \
    -e GNUPGHOME=/gnupg "$@"
}

# 4. Package names, versions and architectures from the control files; the
# canonical file name and the pool path follow from them.
lists="$(mktemp -d)"
chmod 755 "$lists"
signer -v "$in:/in:ro" "$APT_SIGNER_IMAGE" bash -euc '
  cd /in
  for f in *.deb; do
    printf "%s\t%s\n" "$f" "$(dpkg-deb -f "$f" Package Version Architecture | tr "\n" "\t")"
  done' >"$lists/fields" || die "cannot read the control files"
declare -A pool_of=()
while IFS=$'\t' read -r f p v a _; do
  p="${p#Package: }" v="${v#Version: }" a="${a#Architecture: }"
  [[ "$p" =~ ^[a-z0-9][a-z0-9.+-]+$ ]] || die "$f: bad package name $p"
  [[ "$f" == "${p}_${v#*:}_${a}.deb" ]] || die "$f: not the canonical name ${p}_${v#*:}_${a}.deb"
  case " $OB_APT_ARCHES all " in *" $a "*) ;; *) die "$f: architecture $a is not one of $OB_APT_ARCHES all" ;; esac
  [[ "$v" == *dirty* ]] && die "$f: built from a dirty tree, never released"
  letter="${p:0:1}"
  [[ "$p" == lib?* ]] && letter="${p:0:4}"
  pool_of["$f"]="pool/main/$letter/$p/$f"
done <"$lists/fields"
[[ ${#pool_of[@]} -eq ${#debs[@]} ]] || die "could not read every control file"
if [[ -f "$in/POOL-PATHS" ]]; then
  # merge-published-apt.sh records where each published package is served.
  while read -r name path; do
    [[ "${pool_of[$name]:-}" == "$path" ]] || die "$name is published at $path, not ${pool_of[$name]:-?}"
  done <"$in/POOL-PATHS"
fi

# 5. Key material into the tmpfs.
case "$mode" in
  --op)
    command -v op >/dev/null || die "op (1Password CLI) not installed"
    [[ "${OB_OP_PASSPHRASE_REF:-}" == op://* ]] || die "set OB_OP_PASSPHRASE_REF to an op:// reference"
    [[ "${OB_OP_KEY_REF:-}" == op://* || -n "${OB_OP_KEY_DOCUMENT:-}" ]] ||
      die "set OB_OP_KEY_REF (op:// reference) or OB_OP_KEY_DOCUMENT (document item in vault $OB_OP_VAULT)"
    log "reading the signing subkey and its passphrase from 1Password ($OP_ACCOUNT); approve the request"
    if [[ "${OB_OP_KEY_REF:-}" == op://* ]]; then
      op read --no-newline --out-file "$secret/key.asc" "$OB_OP_KEY_REF" >/dev/null || die "op read of the key failed"
    else
      op document get "$OB_OP_KEY_DOCUMENT" --vault "$OB_OP_VAULT" --out-file "$secret/key.asc" >/dev/null ||
        die "op document get of the key failed"
    fi
    op read --no-newline --out-file "$secret/passphrase" "$OB_OP_PASSPHRASE_REF" >/dev/null || die "op read of the passphrase failed"
    chmod 600 "$secret/key.asc" "$secret/passphrase"
    ;;
  --key-file)
    key_file="${OB_SIGNING_KEY_FILE:?set OB_SIGNING_KEY_FILE}"
    pass_file="${OB_SIGNING_PASSPHRASE_FILE:?set OB_SIGNING_PASSPHRASE_FILE}"
    check_secret_file "$key_file"
    check_secret_file "$pass_file"
    cp "$key_file" "$secret/key.asc"
    cp "$pass_file" "$secret/passphrase"
    ;;
  --test-key)
    # Same shape as the release key: certify-only primary, signing subkey,
    # export of the subkey only. Made in a network-less container; the
    # primary key never leaves it.
    log "generating a throwaway test key (dry run only)"
    head -c 24 /dev/urandom | base64 >"$secret/passphrase"
    signer -v "$secret:/secret" "$APT_SIGNER_IMAGE" bash -euc '
      gpg --batch --pinentry-mode loopback --passphrase-file /secret/passphrase \
        --quick-generate-key "OpenBasalt APT dry run (THROWAWAY, never published) <dry-run@invalid>" rsa4096 cert 1d >/dev/null 2>&1
      fpr=$(gpg --batch --with-colons --list-keys 2>/dev/null | awk -F: "/^fpr:/ {print \$10; exit}")
      gpg --batch --pinentry-mode loopback --passphrase-file /secret/passphrase \
        --quick-add-key "$fpr" rsa4096 sign 1d >/dev/null 2>&1
      sub=$(gpg --batch --with-colons --list-keys 2>/dev/null | awk -F: "/^fpr:/ {n++} n==2 && /^fpr:/ {print \$10; exit}")
      gpg --batch --pinentry-mode loopback --passphrase-file /secret/passphrase --armor \
        --export-secret-subkeys "$sub!" >/secret/key.asc 2>/dev/null
      gpg --batch --armor --export "$fpr" >/secret/pub.asc 2>/dev/null
      echo "$sub" >/secret/subkey'
    OB_SIGNING_SUBKEY="$(cat "$secret/subkey")"
    pubkey="$secret/pub.asc"
    ;;
esac
grep -q 'BEGIN PGP PRIVATE KEY BLOCK' "$secret/key.asc" || die "the key material is not an armored secret key export"
[[ -s "$secret/passphrase" ]] || die "empty passphrase"
fpr="${OB_SIGNING_SUBKEY^^}"

# 6. Repository tree: the pool.
repo="$out/apt"
[[ ! -e "$repo" ]] || die "$repo exists; sign into a new directory"
mkdir -p "$repo"
for f in "${debs[@]}"; do
  mkdir -p "$repo/$(dirname "${pool_of[$f]}")"
  cp -p "$in/$f" "$repo/${pool_of[$f]}"
  chmod 644 "$repo/${pool_of[$f]}"
done
out="$(cd "$out" && pwd)"
repo="$out/apt"

# 7. Index and sign, in one network-less container: Packages per
# architecture (every version in the pool), by-hash copies, Release, then
# InRelease (clear-signed) and Release.gpg (detached) with exactly the
# packages subkey.
log "indexing ${#debs[@]} packages (${#presigned[@]} already published) for suite $OB_APT_SUITE and signing with subkey $fpr"
signer -v "$secret:/secret:ro" -v "$repo:/repo" -e FPR="$fpr" -e SUITE="$OB_APT_SUITE" -e ARCHES="$OB_APT_ARCHES" \
  -e LABEL="$OB_APT_LABEL" -e DESC="$OB_APT_DESCRIPTION" "$APT_SIGNER_IMAGE" bash -euc '
  cd /repo
  gpg --batch --quiet --import /secret/key.asc 2>/dev/null || { echo "key import failed" >&2; exit 1; }
  # The primary secret key must be a stub (sec#), the subkey present (ssb).
  gpg --batch --with-colons --list-secret-keys >/gnupg/list
  awk -F: "/^sec:/ && \$15 != \"#\" {bad=1} END {exit bad}" /gnupg/list ||
    { echo "the export contains the secret PRIMARY key; use the packages subkey export (--export-secret-subkeys)" >&2; exit 1; }
  awk -F: -v f="$FPR" "/^ssb:/ {s=\$15} /^fpr:/ && \$10 == f {found=1; ok=(s != \"#\")} END {exit !(found && ok)}" /gnupg/list ||
    { echo "secret subkey $FPR not in the export" >&2; exit 1; }
  rm -f /gnupg/list
  d="dists/$SUITE"
  for a in $ARCHES; do
    b="$d/main/binary-$a"
    mkdir -p "$b/by-hash/SHA256"
    apt-ftparchive --arch "$a" -o APT::FTPArchive::Packages::SHA1=false -o APT::FTPArchive::Packages::MD5=false \
      packages pool/main >"$b/Packages"
    gzip -9 -n -k "$b/Packages"
    xz -9 -k "$b/Packages"
    printf "Archive: %s\nSuite: %s\nCodename: %s\nOrigin: OpenBasalt\nLabel: %s\nComponent: main\nArchitecture: %s\n" \
      "$SUITE" "$SUITE" "$SUITE" "$LABEL" "$a" >"$b/Release"
    for f in Packages Packages.gz Packages.xz; do
      cp "$b/$f" "$b/by-hash/SHA256/$(sha256sum "$b/$f" | cut -d" " -f1)"
    done
  done
  apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=OpenBasalt \
    -o APT::FTPArchive::Release::Label="$LABEL" \
    -o APT::FTPArchive::Release::Suite="$SUITE" \
    -o APT::FTPArchive::Release::Codename="$SUITE" \
    -o APT::FTPArchive::Release::Architectures="$ARCHES" \
    -o APT::FTPArchive::Release::Components=main \
    -o APT::FTPArchive::Release::Description="$DESC" \
    -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
    -o APT::FTPArchive::Release::MD5=false -o APT::FTPArchive::Release::SHA1=false \
    -o APT::FTPArchive::Release::SHA512=false \
    release "$d" >/tmp/Release
  mv /tmp/Release "$d/Release"
  gpgopts="--batch --pinentry-mode loopback --passphrase-file /secret/passphrase --digest-algo SHA512"
  gpg $gpgopts --local-user "$FPR!" --clearsign -o "$d/InRelease" "$d/Release"
  gpg $gpgopts --local-user "$FPR!" --armor --detach-sign -o "$d/Release.gpg" "$d/Release"
  gpgconf --kill gpg-agent 2>/dev/null || true
  find /repo -type d -exec chmod 755 {} + ; find /repo -type f -exec chmod 644 {} +'

# 8. Shred the key material now, before verification. A dry run keeps its
# test PUBLIC key next to the tree (OUT_DIR.test-pubkey.asc) for the
# verification below and for client tests; it is not part of the tree.
if [[ "$mode" == --test-key ]]; then
  cp "$secret/pub.asc" "$out.test-pubkey.asc"
  chmod 644 "$out.test-pubkey.asc"
  pubkey="$out.test-pubkey.asc"
fi
cleanup
log "key material shredded"

# 9. Verify with the public key only, in a container that never saw a secret.
# In a test-key dry run with published packages the metadata is still signed
# by the test key only (packages themselves carry no signature in APT).
first_pkg="$(awk -F'\t' 'NR==1 {sub(/^Package: /, "", $2); print $2}' "$lists/fields")"
signer -v "$repo:/repo/apt:ro" -v "$pubkey:/pub/key.asc:ro" -e FPR="$fpr" -e SUITE="$OB_APT_SUITE" \
  -e ARCHES="$OB_APT_ARCHES" -e PKG="$first_pkg" "$APT_SIGNER_IMAGE" bash -euc '
  bad=0
  d=/repo/apt/dists/$SUITE
  gpg --batch --quiet --import /pub/key.asc 2>/dev/null
  gpg --batch --yes --export >/tmp/keyring.gpg
  # gpgv (apt on Ubuntu 24.04) and sqv (apt on Debian 13 and Ubuntu 26.04).
  gpgv --keyring /tmp/keyring.gpg "$d/InRelease" >/dev/null 2>&1 || { echo "gpgv rejects InRelease" >&2; bad=1; }
  gpgv --keyring /tmp/keyring.gpg "$d/Release.gpg" "$d/Release" >/dev/null 2>&1 || { echo "gpgv rejects Release.gpg" >&2; bad=1; }
  sqv --keyring /tmp/keyring.gpg "$d/Release.gpg" "$d/Release" >/dev/null 2>&1 || { echo "sqv rejects Release.gpg" >&2; bad=1; }
  for f in InRelease Release.gpg; do
    if [ $f = InRelease ]; then st=$(gpg --batch --status-fd 1 --verify "$d/InRelease" 2>/dev/null); else st=$(gpg --batch --status-fd 1 --verify "$d/Release.gpg" "$d/Release" 2>/dev/null); fi
    echo "$st" | grep -q "^\[GNUPG:\] VALIDSIG $FPR " || { echo "$f: not signed by the packages subkey $FPR" >&2; bad=1; }
  done
  # InRelease carries exactly the Release text.
  gpg --batch --quiet --output /tmp/inrelease.txt --decrypt "$d/InRelease" 2>/dev/null
  cmp -s /tmp/inrelease.txt "$d/Release" || { echo "InRelease and Release differ" >&2; bad=1; }
  # Every SHA256 line of Release, and every package of every Packages file.
  awk "/^SHA256:/ {s=1; next} /^[^ ]/ {s=0} s {print \$1, \$3}" "$d/Release" >/tmp/sums
  [ -s /tmp/sums ] || { echo "Release has no SHA256 entries" >&2; bad=1; }
  while read -r sum path; do
    [ "$(sha256sum "$d/$path" | cut -d" " -f1)" = "$sum" ] || { echo "Release: bad checksum for $path" >&2; bad=1; }
  done </tmp/sums
  n=0
  for a in $ARCHES; do
    p="$d/main/binary-$a/Packages"
    [ -f "$d/main/binary-$a/by-hash/SHA256/$(sha256sum "$p" | cut -d" " -f1)" ] || { echo "by-hash copy missing: $p" >&2; bad=1; }
    awk "/^Filename: / {f=\$2} /^SHA256: / {print \$2, f}" "$p" >/tmp/pk
    while read -r sum file; do
      n=$((n+1))
      [ "$(sha256sum "/repo/apt/$file" | cut -d" " -f1)" = "$sum" ] || { echo "Packages: bad checksum for $file" >&2; bad=1; }
    done </tmp/pk
  done
  indexed=$(cd /repo/apt && find pool -name "*.deb" | wc -l)
  # apt end to end, offline: signed-by keyring, nothing else configured.
  mkdir -p /tmp/apt/parts /tmp/apt/keyrings /tmp/apt/lists/partial /tmp/dl
  chown -R _apt /tmp/apt/lists /tmp/dl 2>/dev/null || true
  cp /tmp/keyring.gpg /tmp/apt/keyrings/openbasalt.gpg
  printf "Types: deb\nURIs: file:/repo/apt\nSuites: %s\nComponents: main\nSigned-By: /tmp/apt/keyrings/openbasalt.gpg\n" "$SUITE" >/tmp/apt/parts/openbasalt.sources
  aptopts="-o Dir::Etc::SourceList=/dev/null -o Dir::Etc::SourceParts=/tmp/apt/parts -o Dir::State::Lists=/tmp/apt/lists -o Debug::NoLocking=1"
  apt-get $aptopts update >/tmp/update.log 2>&1 && ! grep -qE "^(W|E):" /tmp/update.log ||
    { cat /tmp/update.log >&2; echo "apt rejected the repository" >&2; bad=1; }
  (cd /tmp/dl && apt-get $aptopts download "$PKG" >/dev/null 2>&1) || { echo "apt could not fetch $PKG" >&2; bad=1; }
  echo "verified: suite $SUITE, $indexed packages in the pool, $n index entries, failures: $bad"
  [ "$bad" = 0 ]' || die "verification failed; do not publish $out"

# 10. Marker for upload.sh: what was verified, with which key.
{
  echo "signed and verified $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "mode: ${mode#--}"
  echo "subkey: $fpr"
  echo "repo: apt"
  echo "suite: $OB_APT_SUITE"
  echo "published kept: ${#presigned[@]}"
  [[ -f "$in/BUILD-INFO.txt" ]] && sed 's/^/build: /' "$in/BUILD-INFO.txt"
  (cd "$out" && find apt -type f | LC_ALL=C sort | xargs -d '\n' sha256sum)
} >"$out/SIGNED-OK"
log "signed repository: $out/apt (suite $OB_APT_SUITE, ${#debs[@]} packages, ${#presigned[@]} of them already published)"
