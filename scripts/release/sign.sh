#!/usr/bin/env bash
# Release signer: sign a release build and lay out the repository tree that
# scripts/release/upload.sh publishes to https://obpkg.org.
#
#   scripts/release/sign.sh --op       IN_DIR OUT_DIR   key from 1Password (op), the real run
#   scripts/release/sign.sh --key-file IN_DIR OUT_DIR   key from files (OB_SIGNING_KEY_FILE, OB_SIGNING_PASSPHRASE_FILE)
#   scripts/release/sign.sh --test-key IN_DIR OUT_DIR   throwaway key made here (dry run, never published)
#
# IN_DIR is the output of scripts/release/build.sh (RPMs and SHA256SUMS), or
# any directory of unsigned RPMs with a SHA256SUMS that lists exactly them.
# RPMs listed in IN_DIR/PUBLISHED (scripts/release/merge-published.sh: the
# packages already on obpkg.org) are already signed: they are kept
# byte-identical, not signed again, and verified with the release key.
# OUT_DIR gets the tree as it appears under https://obpkg.org/:
#   <repo>/<releasever>/<arch>/    binary RPMs + repodata (repomd.xml.asc)
#   <repo>/<releasever>/source/    source RPMs + repodata (repomd.xml.asc),
#                                  only when IN_DIR has source RPMs
# <repo> is OB_REPO: basalt (default), basalt-tools or basalt-testing. This
# is what basalt-release's repository files expect
# ($basalt_repo_url/$releasever/$basearch/ with basalt_repo_url =
# https://obpkg.org/basalt, likewise basalt_tools_url and basalt_testing_url).
#
# Key material
# - --op reads from 1Password the ASCII armored export of the packages
#   signing subkey (made with --export-secret-subkeys): OB_OP_KEY_REF, an
#   op:// reference, or OB_OP_KEY_DOCUMENT, a document item in vault
#   OB_OP_VAULT (default OpenBasalt); and its passphrase, OB_OP_PASSPHRASE_REF
#   (op:// reference). Both go with --out-file into a 0700 directory on a
#   tmpfs. OP_ACCOUNT defaults to my.1password.com.
# - --key-file takes the same two files, which must be 0600, owned by the
#   caller and on a tmpfs. They are shredded when this script exits.
# - The keyring exists only in a tmpfs inside a container started with
#   --network none. The secret primary key must not be in the export (only
#   its stub): the script refuses a full secret key. Signing uses exactly the
#   packages subkey (OB_SIGNING_SUBKEY, forced with gpg's "!" suffix).
# - Nothing secret is passed on a command line or printed.
#
# Verification uses only the public key (OB_RELEASE_PUBKEY, default
# packages/basalt-release/RPM-GPG-KEY-basalt, which must equal the key
# published at https://obpkg.org/keys/openbasalt-release-key.asc), in a
# second container that never sees secret material: rpmkeys --checksig in a
# clean rpm database, gpg --verify of every repomd.xml.asc, and dnf with
# gpgcheck=1 and repo_gpgcheck=1 against the tree. On success it writes
# OUT_DIR/SIGNED-OK, which upload.sh requires.
# With --test-key and a PUBLISHED list, the tree mixes two keys (the test key
# and the release key of the published RPMs): client tests then need both,
# for example cat OUT_DIR.test-pubkey.asc packages/basalt-release/RPM-GPG-KEY-basalt.
source "$(dirname "$0")/../lib.sh"

: "${OB_SIGNING_SUBKEY:=302461D26520E077D07FFCA9AA27C62C36CCFC4B}"
: "${OB_RELEASE_PUBKEY:=$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt}"
: "${OB_PUBKEY_URL:=https://obpkg.org/keys/openbasalt-release-key.asc}"
: "${OP_ACCOUNT:=my.1password.com}"
: "${OB_OP_VAULT:=OpenBasalt}"
: "${SIGN_PODMAN:=podman}"
SIGNER_IMAGE="localhost/basalt-signer:$FEDORA_RELEASE"
: "${OB_REPO:=basalt}"
case "$OB_REPO" in basalt | basalt-tools | basalt-testing) ;; *) die "OB_REPO must be basalt, basalt-tools or basalt-testing" ;; esac
export OP_ACCOUNT

mode="${1:-}"
case "$mode" in --op | --key-file | --test-key) shift ;; *) sed -n '2,41p' "$0"; exit 2 ;; esac
in="${1:?IN_DIR}"
out="${2:?OUT_DIR}"
in="$(cd "$in" && pwd)" || die "no $in"

# Secret work directory on a tmpfs, removed (files shredded) on exit.
base="${XDG_RUNTIME_DIR:-/dev/shm}"
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || base=/dev/shm
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || die "no tmpfs for the key material"
umask 077
secret="$(mktemp -d "$base/basalt-sign.XXXXXX")"
key_file="" pass_file=""
cleanup() {
  local f
  for f in "$secret"/* "$key_file" "$pass_file"; do
    [[ -n "$f" && -f "$f" ]] && shred -u "$f" 2>/dev/null
  done
  rm -rf "$secret"
}
trap cleanup EXIT

on_tmpfs() { [[ "$(stat -f -c %T "$1")" == tmpfs ]]; }
check_secret_file() {
  local f="$1"
  [[ -f "$f" && ! -L "$f" ]] || die "$f: not a regular file"
  on_tmpfs "$f" || die "$f: not on a tmpfs"
  [[ "$(stat -c '%a %u' "$f")" == "600 $(id -u)" ]] || die "$f: must be mode 0600 and owned by $(id -un)"
}

# 1. Input: every RPM listed in SHA256SUMS, checksums good, nothing signed yet.
[[ -f "$in/SHA256SUMS" ]] || die "$in/SHA256SUMS missing (scripts/release/build.sh)"
(cd "$in" && sha256sum -c --quiet SHA256SUMS) || die "checksum mismatch in $in"
mapfile -t rpms < <(cd "$in" && find . -maxdepth 1 -name '*.rpm' -printf '%P\n' | sort)
[[ ${#rpms[@]} -gt 0 ]] || die "no RPMs in $in"
[[ "$(awk '{print $2}' "$in/SHA256SUMS" | sort)" == "$(printf '%s\n' "${rpms[@]}")" ]] ||
  die "the RPMs in $in and SHA256SUMS differ"
# Published RPMs (already signed, kept as they are): each must be in
# SHA256SUMS with the same checksum.
release_subkey="${OB_SIGNING_SUBKEY^^}"
presigned=()
if [[ -f "$in/PUBLISHED" ]]; then
  while read -r psum name; do
    grep -qxF "$psum  $name" "$in/SHA256SUMS" || die "$name in PUBLISHED does not match SHA256SUMS"
    presigned+=("$name")
  done <"$in/PUBLISHED"
fi

# 2. The public key the tree is verified with.
if [[ "$mode" != --test-key ]]; then
  [[ "$(cut -c1-40 <<<"${OB_SIGNING_SUBKEY^^}")" =~ ^[0-9A-F]{40}$ ]] || die "OB_SIGNING_SUBKEY must be a fingerprint"
  published="$secret/published.asc"
  curl -fsSL --proto '=https' -o "$published" "$OB_PUBKEY_URL" || die "cannot fetch $OB_PUBKEY_URL"
  cmp -s "$published" "$OB_RELEASE_PUBKEY" || die "$OB_RELEASE_PUBKEY differs from $OB_PUBKEY_URL"
  rm -f "$published"
  pubkey="$OB_RELEASE_PUBKEY"
fi

# 3. Signer image: Fedora with the tools, built while no secret exists yet.
if ! $SIGN_PODMAN image exists "$SIGNER_IMAGE"; then
  log "building $SIGNER_IMAGE"
  printf 'FROM %s\nRUN dnf -q -y install rpm-sign createrepo_c gnupg2 coreutils && dnf clean all\n' "$FEDORA_IMAGE" |
    $SIGN_PODMAN build -q -t "$SIGNER_IMAGE" -f - >/dev/null 2>&1 || die "cannot build $SIGNER_IMAGE"
fi
signer() {
  $SIGN_PODMAN run --rm -i --network none --security-opt label=disable --tmpfs /gnupg:rw,mode=0700,size=32m \
    -e GNUPGHOME=/gnupg "$@"
}

# 4. Key material into the tmpfs.
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
    signer -v "$secret:/secret" "$SIGNER_IMAGE" bash -euc '
      gpg --batch --pinentry-mode loopback --passphrase-file /secret/passphrase \
        --quick-generate-key "Basalt OS dry run (THROWAWAY, never published) <dry-run@invalid>" rsa4096 cert 1d >/dev/null 2>&1
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

# 5. Repository tree.
bin="$out/$OB_REPO/$FEDORA_RELEASE/$ARCH"
src="$out/$OB_REPO/$FEDORA_RELEASE/source"
[[ ! -e "$out/$OB_REPO" ]] || die "$out/$OB_REPO exists; sign into a new directory"
mkdir -p "$bin"
printf '%s\n' "${rpms[@]}" | grep -q '\.src\.rpm$' && mkdir -p "$src"
for f in "${rpms[@]}"; do
  case "$f" in
    *.src.rpm) cp -p "$in/$f" "$src/" ;;
    *) cp -p "$in/$f" "$bin/" ;;
  esac
done
out="$(cd "$out" && pwd)"
presigned_list="$(mktemp)"
printf '%s\n' "${presigned[@]}" | grep -v '^$' >"$presigned_list" || true
chmod 644 "$presigned_list"
trap 'cleanup; rm -f "$presigned_list"' EXIT

# 6. Sign: import the subkey into the container's tmpfs keyring, sign every
# RPM with exactly that subkey, index, sign repomd.xml.
log "signing $((${#rpms[@]} - ${#presigned[@]})) packages with subkey $fpr (${#presigned[@]} published ones kept as they are)"
signer -v "$secret:/secret:ro" -v "$out/$OB_REPO:/repo/basalt" -v "$presigned_list:/presigned.list:ro" -e FPR="$fpr" -e REL="$FEDORA_RELEASE" -e ARCH="$ARCH" \
  "$SIGNER_IMAGE" bash -euc '
  shopt -s nullglob
  gpg --batch --quiet --import /secret/key.asc 2>/dev/null || { echo "key import failed" >&2; exit 1; }
  # The primary secret key must be a stub (sec#), the subkey present (ssb).
  gpg --batch --with-colons --list-secret-keys >/gnupg/list
  awk -F: "/^sec:/ && \$15 != \"#\" {bad=1} END {exit bad}" /gnupg/list ||
    { echo "the export contains the secret PRIMARY key; use the packages subkey export (--export-secret-subkeys)" >&2; exit 1; }
  awk -F: -v f="$FPR" "/^ssb:/ {s=\$15} /^fpr:/ && \$10 == f {found=1; ok=(s != \"#\")} END {exit !(found && ok)}" /gnupg/list ||
    { echo "secret subkey $FPR not in the export" >&2; exit 1; }
  rm -f /gnupg/list
  gpgopts="--batch --pinentry-mode loopback --passphrase-file /secret/passphrase"
  # Published RPMs (/presigned.list) are kept as they are; the others are signed here.
  tosign=()
  for f in /repo/basalt/$REL/$ARCH/*.rpm /repo/basalt/$REL/source/*.rpm; do
    grep -qxF "$(basename "$f")" /presigned.list && continue
    tosign+=("$f")
    if rpm -qp --qf "%{SIGPGP:pgpsig}%{RSAHEADER:pgpsig}%{OPENPGP:pgpsig}\n" "$f" 2>/dev/null | grep -q "Key ID"; then
      echo "already signed: $(basename "$f")" >&2; exit 1
    fi
  done
  [ ${#tosign[@]} -gt 0 ] && rpmsign --define "_gpg_name $FPR!" --define "_gpg_path /gnupg" \
    --define "_gpg_sign_cmd_extra_args $gpgopts" \
    --addsign "${tosign[@]}" 2>&1 >/dev/null | grep -v "GPG_TTY" >&2 || true
  for f in "${tosign[@]}"; do
    rpm -qp --qf "%{OPENPGP:pgpsig}%{RSAHEADER:pgpsig}\n" "$f" 2>/dev/null | grep -q "Key ID" ||
      { echo "rpmsign did not sign $(basename "$f")" >&2; exit 1; }
  done
  for d in /repo/basalt/$REL/$ARCH /repo/basalt/$REL/source; do
    [ -d "$d" ] || continue
    createrepo_c -q "$d"
    gpg $gpgopts --local-user "$FPR!" --detach-sign --armor -o "$d/repodata/repomd.xml.asc" "$d/repodata/repomd.xml"
  done
  gpgconf --kill gpg-agent 2>/dev/null || true'

# 7. Shred the key material now, before verification. A dry run keeps its
# test PUBLIC key next to the tree (OUT_DIR.test-pubkey.asc) for the
# verification below and for client tests; it is not part of the tree.
if [[ "$mode" == --test-key ]]; then
  cp "$secret/pub.asc" "$out.test-pubkey.asc"
  pubkey="$out.test-pubkey.asc"
fi
cleanup
trap 'rm -f "$presigned_list"' EXIT
log "key material shredded"

# 8. Verify with the public key only, in a container that never saw a secret.
verify() {
  local key="$1"
  signer -v "$out/$OB_REPO:/repo/basalt:ro" -v "$key:/pub/key.asc:ro" -v "$OB_RELEASE_PUBKEY:/pub/release.asc:ro" \
    -v "$presigned_list:/presigned.list:ro" -e FPR="$fpr" -e RFPR="$release_subkey" -e REL="$FEDORA_RELEASE" -e ARCH="$ARCH" \
    -e PKG="$verify_pkg" "$SIGNER_IMAGE" bash -euc '
    shopt -s nullglob
    db=$(mktemp -d)
    rpmkeys --dbpath "$db" --import /pub/key.asc
    # Published RPMs carry the release key (the same key, except in a test-key dry run).
    [ -s /presigned.list ] && rpmkeys --dbpath "$db" --import /pub/release.asc
    bad=0 n=0
    for f in /repo/basalt/$REL/$ARCH/*.rpm /repo/basalt/$REL/source/*.rpm; do
      n=$((n+1))
      want="$FPR"
      grep -qxF "$(basename "$f")" /presigned.list && want="$RFPR"
      long=$(echo "$want" | tr A-F a-f)
      rpmkeys --dbpath "$db" --checksig "$f" 2>&1 | grep -q ": digests signatures OK$" ||
        { echo "BAD signature: $(basename "$f")" >&2; bad=1; continue; }
      # The signature names the signing key by its id: the packages subkey.
      rpm -qp --qf "%{OPENPGP:pgpsig}|%{RSAHEADER:pgpsig}\n" "$f" 2>/dev/null | grep -q "Key ID ${long: -16}" ||
        { echo "not signed by $want: $(basename "$f")" >&2; bad=1; }
    done
    gpg --batch --quiet --import /pub/key.asc 2>/dev/null
    for d in /repo/basalt/$REL/$ARCH /repo/basalt/$REL/source; do
      [ -d "$d" ] || continue
      gpg --batch --status-fd 1 --verify "$d/repodata/repomd.xml.asc" "$d/repodata/repomd.xml" 2>/dev/null |
        grep -q "^\[GNUPG:\] VALIDSIG $FPR " || { echo "BAD metadata signature: $d" >&2; bad=1; }
    done
    # dnf end to end: package and metadata signatures checked, offline.
    cat >/etc/yum.repos.d/verify.repo <<EOF
[verify]
name=verify
baseurl=file:///repo/basalt/$REL/$ARCH/
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///pub/key.asc file:///pub/release.asc
EOF
    dnf -q -y --disablerepo="*" --enablerepo=verify makecache >/dev/null 2>&1 || { echo "dnf rejected the repository metadata" >&2; bad=1; }
    dnf -q -y --disablerepo="*" --enablerepo=verify download --destdir /tmp/dl "$PKG" >/dev/null 2>&1 ||
      { echo "dnf could not fetch $PKG" >&2; bad=1; }
    echo "verified: $n packages, $(ls /repo/basalt/$REL/*/repodata/repomd.xml.asc | wc -l) repomd.xml.asc, failures: $bad"
    [ "$bad" = 0 ]'
}
# The package dnf fetches end to end: basalt-release in the basalt
# repository, otherwise the first binary package.
if [[ "$OB_REPO" == basalt ]]; then
  verify_pkg=basalt-release
else
  first="$(printf '%s\n' "${rpms[@]}" | grep -v '\.src\.rpm$' | head -1)"
  verify_pkg="$(rpm -qp --qf '%{NAME}' "$in/$first" 2>/dev/null)" || die "cannot read the name of $first"
fi
verify "$pubkey" || die "verification failed; do not publish $out"

# 9. Marker for upload.sh: what was verified, with which key.
{
  echo "signed and verified $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "mode: ${mode#--}"
  echo "subkey: $fpr"
  echo "repo: $OB_REPO"
  echo "published kept: ${#presigned[@]}"
  [[ -f "$in/BUILD-INFO.txt" ]] && sed 's/^/build: /' "$in/BUILD-INFO.txt"
  (cd "$out" && find "$OB_REPO" -type f | sort | xargs -d '\n' sha256sum)
} >"$out/SIGNED-OK"
log "signed repository: $out/$OB_REPO/$FEDORA_RELEASE ($(find "$bin" -name '*.rpm' | wc -l) binary, $( [[ -d "$src" ]] && find "$src" -name '*.rpm' | wc -l || echo 0) source)"
