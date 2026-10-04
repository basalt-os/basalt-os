#!/usr/bin/env bash
# Sign a knowledge index (basalt-knowledge) with the OpenBasalt knowledge
# subkey and check the result the way the assistant will.
#
#   scripts/sign-knowledge.sh INDEX_DIR SUBKEY_SECRET_FILE PASSPHRASE_FILE [SOURCES_MANIFEST]
#
# INDEX_DIR          manifest.json, cases.jsonl, index.bin (VSM's builder; the
#                    manifest must carry index_sha256)
# SUBKEY_SECRET_FILE the knowledge subkey's secret part, exported alone
#                    (gpg --export-secret-subkeys --armor 85D61430...659A!),
#                    a regular file of the invoking user, mode 0600
# PASSPHRASE_FILE    its passphrase, same rules
# SOURCES_MANIFEST   optional: a sources.manifest whose manifest.json.sig
#                    line is (re)written with the new signature's SHA-256,
#                    size and path (artifacts layout knowledge/<release>/<date>/)
#
# Environment: SIGNER (subkey fingerprint, default the OpenBasalt knowledge
# subkey), PRIMARY (default the OpenBasalt release key), PUBLIC_CERT (the
# certificate the result is checked against, default
# packages/basalt-knowledge/openbasalt-release-key.asc), ARTIFACT_PATH (the
# manifest line's path for the signature; default derived from the line of
# manifest.json in SOURCES_MANIFEST).
#
# The secret never leaves a temporary GnuPG home in /dev/shm (memory only),
# created for this run and shredded at exit. Nothing is uploaded: publish
# manifest.json.sig next to the index with the release scripts.
source "$(dirname "$0")/lib.sh"
: "${SIGNER:=85D61430B70438680F6EB955E79E4020605A659A}"
: "${PRIMARY:=3601734842BD4E482D19DE4AE4EED5ECA395B302}"
: "${PUBLIC_CERT:=$REPO_ROOT/packages/basalt-knowledge/openbasalt-release-key.asc}"

dir="${1:-}" secret="${2:-}" pass="${3:-}" srcman="${4:-}"
[[ -n "$dir" && -n "$secret" && -n "$pass" ]] || die "usage: $0 INDEX_DIR SUBKEY_SECRET_FILE PASSPHRASE_FILE [SOURCES_MANIFEST]"
for f in "$dir/manifest.json" "$dir/cases.jsonl" "$dir/index.bin" "$PUBLIC_CERT"; do
  [[ -f "$f" ]] || die "$f missing"
done
for f in "$secret" "$pass"; do
  [[ -f "$f" && ! -L "$f" ]] || die "$f: not a regular file"
  [[ "$(stat -c %u "$f")" == "$(id -u)" ]] || die "$f: not owned by $(id -un)"
  [[ "$(stat -c %a "$f")" == 600 ]] || die "$f: mode $(stat -c %a "$f"), want 600"
done
command -v gpg >/dev/null || die "gpg is needed to sign (the assistant needs no gpg to verify)"
grep -q '"index_sha256"' "$dir/manifest.json" || die "manifest.json does not address index.bin (index_sha256): rebuild the index"
python3 - "$dir" <<'PY' || die "manifest.json does not match the index files"
import hashlib, json, sys
d = sys.argv[1]
m = json.load(open(d + "/manifest.json"))
for name, key in (("cases.jsonl", "sha256"), ("index.bin", "index_sha256")):
    if hashlib.sha256(open(f"{d}/{name}", "rb").read()).hexdigest() != m[key]:
        sys.exit(f"{name}: SHA-256 differs from the manifest")
PY

home="$(mktemp -d /dev/shm/basalt-sign.XXXXXX)"
chmod 700 "$home"
cleanup() { find "$home" -type f -exec shred -u {} + 2>/dev/null; rm -rf -- "$home"; }
trap cleanup EXIT
g() { gpg --homedir "$home" --batch --no-tty --pinentry-mode loopback --passphrase-file "$pass" "$@"; }
gpg --homedir "$home" --batch --import "$PUBLIC_CERT" 2>/dev/null || die "cannot import $PUBLIC_CERT"
g --import "$secret" 2>/dev/null || die "cannot import the secret subkey"
# Exactly the expected subkey must have its secret here.
have="$(gpg --homedir "$home" --with-colons --list-secret-keys | awk -F: '$1=="ssb" && $15!="#" {k=1} $1=="fpr" && k {print $10; k=0}')"
[[ "$have" == *"$SIGNER"* ]] || die "the secret file does not hold the signing subkey $SIGNER"
gpg --homedir "$home" --with-colons --list-keys "$PRIMARY" >/dev/null 2>&1 || die "primary $PRIMARY not in $PUBLIC_CERT"

out="$dir/manifest.json.sig"
g --local-user "$SIGNER!" --digest-algo SHA256 --detach-sign -o "$out.tmp" "$dir/manifest.json" || die "signing failed"
mv -f "$out.tmp" "$out"
log "signed: $out"

# Check: gpg against the public certificate only, then the assistant's
# own verifier (Go standard library, pinned fingerprints).
pub="$(mktemp -d /dev/shm/basalt-verify.XXXXXX)"
gpg --homedir "$pub" --batch --import "$PUBLIC_CERT" 2>/dev/null
gpg --homedir "$pub" --batch --status-fd 1 --verify "$out" "$dir/manifest.json" 2>/dev/null | grep -q "VALIDSIG $SIGNER " ||
  { rm -rf -- "$pub"; die "gpg does not accept the signature"; }
rm -rf -- "$pub"
log "gpg: good signature by $SIGNER"
( cd "$REPO_ROOT/packages/basalt-assistant" &&
  GOFLAGS=-mod=mod GOTOOLCHAIN=local GOPROXY=off go run ./tools/basalt-eval knowledge-verify \
    -dir "$dir" -key "$PUBLIC_CERT" -signer "$PRIMARY $SIGNER" ) || die "the assistant's verifier refuses the signature"

if [[ -n "$srcman" ]]; then
  sha="$(sha256sum "$out" | cut -d' ' -f1)" bytes="$(stat -c %s "$out")"
  path="${ARTIFACT_PATH:-$(awk '$1=="manifest.json" {sub(/manifest\.json$/, "manifest.json.sig", $4); print $4}' "$srcman")}"
  [[ -n "$path" ]] || die "no path for the signature (set ARTIFACT_PATH)"
  tmp="$(mktemp)"
  awk '$1!="manifest.json.sig"' "$srcman" >"$tmp"
  printf 'manifest.json.sig  %s  %s  %s\n' "$sha" "$bytes" "$path" >>"$tmp"
  cat "$tmp" >"$srcman" && rm -f "$tmp"
  log "updated $srcman"
fi
