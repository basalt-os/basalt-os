#!/usr/bin/env bash
# Publish a signed repository tree (scripts/release/sign.sh) to the obpkg
# bucket (Cloudflare R2, served as https://obpkg.org).
#
#   scripts/release/upload.sh [--dry-run] TREE_DIR
#
# TREE_DIR is sign.sh's OUT_DIR: SIGNED-OK and <repo>/<releasever>/{<arch>,source}/,
# where <repo> (basalt, basalt-tools or basalt-testing) is the "repo:" line
# of SIGNED-OK (basalt when the line is missing).
# Every file is checked against SIGNED-OK before anything is sent, and a
# tree signed with a test key is refused for the obpkg bucket.
#
# Credentials (never on a command line, never printed): files of mode 0600
# owned by the caller, named by
#   OB_R2_ACCESS_KEY_ID_FILE      S3 access key id of the publish token
#   OB_R2_SECRET_ACCESS_KEY_FILE  S3 secret access key
#   OB_R2_ENDPOINT_FILE           https://<account id>.r2.cloudflarestorage.com
#                                 (or the URL itself in OB_R2_ENDPOINT)
# OB_R2_BUCKET defaults to obpkg. The AWS CLI configuration is written to a
# tmpfs and shredded on exit.
#
# Order: source RPMs, binary RPMs, repodata blobs, then repomd.xml and
# repomd.xml.asc of each repository last, so a client never sees metadata
# that points at missing files. Cache-Control: repomd.xml and its signature
# public, max-age=60; RPMs and repodata blobs (content-addressed names)
# public, max-age=2592000, immutable. A published RPM is never replaced: an
# existing object with the same key and a different MD5 stops the upload.
#
# Later publishes must start from the whole published package set (the
# metadata built by sign.sh lists only the packages in its input).
source "$(dirname "$0")/../lib.sh"

dry=0
[[ "${1:-}" == --dry-run ]] && { dry=1; shift; }
tree="${1:?usage: $0 [--dry-run] TREE_DIR}"
tree="$(cd "$tree" && pwd)" || die "no $tree"
: "${OB_R2_BUCKET:=obpkg}"
: "${OB_PUBLIC_URL:=https://obpkg.org}"

# 1. The tree is exactly what sign.sh verified.
[[ -f "$tree/SIGNED-OK" ]] || die "$tree/SIGNED-OK missing: sign and verify with scripts/release/sign.sh first"
mode="$(awk '/^mode: / {print $2}' "$tree/SIGNED-OK")"
if [[ "$mode" == test-key && "$OB_R2_BUCKET" == obpkg ]]; then
  die "this tree was signed with a throwaway test key; it never goes to the obpkg bucket"
fi
repo="$(awk '/^repo: / {print $2}' "$tree/SIGNED-OK")"
repo="${repo:-basalt}"
case "$repo" in basalt | basalt-tools | basalt-testing) ;; *) die "unknown repository \"$repo\" in SIGNED-OK" ;; esac
listed="$(grep -E "^[0-9a-f]{64}  $repo/" "$tree/SIGNED-OK")"
(cd "$tree" && sha256sum -c --quiet <<<"$listed") || die "the tree changed after it was verified"
[[ "$(awk '{print $2}' <<<"$listed" | sort)" == "$(cd "$tree" && find "$repo" -type f | sort)" ]] ||
  die "files in $tree/$repo and SIGNED-OK differ"

# 2. Upload plan.
mapfile -t repos < <(cd "$tree" && find "$repo" -type f -path '*/repodata/repomd.xml' -printf '%h\n' | xargs -n1 dirname | sort)
[[ ${#repos[@]} -gt 0 ]] || die "no repositories in $tree"
plan=()
for r in "${repos[@]}"; do [[ "$r" == */source ]] && mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find "$r" -maxdepth 1 -name '*.rpm' | sort); done
for r in "${repos[@]}"; do [[ "$r" != */source ]] && mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find "$r" -maxdepth 1 -name '*.rpm' | sort); done
for r in "${repos[@]}"; do
  mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find "$r/repodata" -type f ! -name 'repomd.xml*' | sort)
done
for r in "${repos[@]}"; do plan+=("$r/repodata/repomd.xml" "$r/repodata/repomd.xml.asc"); done

content_type() {
  case "$1" in
    *.rpm) echo application/x-rpm ;;
    *.xml) echo application/xml ;;
    *.asc) echo application/pgp-signature ;;
    *.gz) echo application/gzip ;;
    *.zst) echo application/zstd ;;
    *.xz) echo application/x-xz ;;
    *.bz2) echo application/x-bzip2 ;;
    *.sqlite) echo application/vnd.sqlite3 ;;
    *) echo application/octet-stream ;;
  esac
}
cache_control() {
  case "$1" in
    */repomd.xml | */repomd.xml.asc) echo "public, max-age=60" ;;
    *) echo "public, max-age=2592000, immutable" ;;
  esac
}

if [[ $dry == 1 ]]; then
  log "dry run: ${#plan[@]} objects to s3://$OB_R2_BUCKET/ (signed: $mode)"
  for k in "${plan[@]}"; do printf '  %-90s %-26s %s\n' "$k" "$(content_type "$k")" "$(cache_control "$k")"; done
  exit 0
fi

# 3. Credentials into a tmpfs AWS configuration.
for var in OB_R2_ACCESS_KEY_ID_FILE OB_R2_SECRET_ACCESS_KEY_FILE; do
  f="${!var:-}"
  [[ -n "$f" ]] || die "set $var"
  [[ -f "$f" && ! -L "$f" ]] || die "$var: $f is not a regular file"
  [[ "$(stat -c '%a %u' "$f")" == "600 $(id -u)" ]] || die "$var: $f must be mode 0600 and owned by $(id -un)"
done
endpoint="${OB_R2_ENDPOINT:-}"
[[ -z "$endpoint" && -n "${OB_R2_ENDPOINT_FILE:-}" ]] && endpoint="$(tr -d '[:space:]' <"$OB_R2_ENDPOINT_FILE")"
[[ "$endpoint" =~ ^https?://[A-Za-z0-9.:-]+/?$ ]] || die "set OB_R2_ENDPOINT or OB_R2_ENDPOINT_FILE (https://<account id>.r2.cloudflarestorage.com)"
command -v aws >/dev/null || die "aws (AWS CLI v2) not installed"

base="${XDG_RUNTIME_DIR:-/dev/shm}"
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || base=/dev/shm
umask 077
cfg="$(mktemp -d "$base/basalt-upload.XXXXXX")"
trap 'find "$cfg" -type f -exec shred -u {} + 2>/dev/null; rm -rf "$cfg"' EXIT
{
  echo "[default]"
  printf 'aws_access_key_id = %s\n' "$(tr -d '[:space:]' <"$OB_R2_ACCESS_KEY_ID_FILE")"
  printf 'aws_secret_access_key = %s\n' "$(tr -d '[:space:]' <"$OB_R2_SECRET_ACCESS_KEY_FILE")"
} >"$cfg/credentials"
printf '[default]\nregion = auto\ns3 =\n  multipart_threshold = 5GB\n' >"$cfg/config"
export AWS_SHARED_CREDENTIALS_FILE="$cfg/credentials" AWS_CONFIG_FILE="$cfg/config" AWS_PROFILE=default
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
export AWS_EC2_METADATA_DISABLED=true AWS_PAGER=""
s3api() { aws --endpoint-url "$endpoint" s3api "$@"; }

# 4. Immutable objects: refuse to replace a different RPM or blob; skip identical ones.
log "checking existing objects in $OB_R2_BUCKET"
todo=()
for k in "${plan[@]}"; do
  case "$k" in */repomd.xml | */repomd.xml.asc) todo+=("$k"); continue ;; esac
  etag="$(s3api head-object --bucket "$OB_R2_BUCKET" --key "$k" --query ETag --output text 2>/dev/null || true)"
  etag="${etag//\"/}"
  if [[ -z "$etag" || "$etag" == None ]]; then
    todo+=("$k")
  elif [[ "$etag" == "$(md5sum "$tree/$k" | cut -d' ' -f1)" ]]; then
    log "already published, identical: $k"
  else
    die "$k is already published with different content; a published file is never replaced"
  fi
done

# 5. Upload in order.
n=0
for k in "${todo[@]}"; do
  s3api put-object --bucket "$OB_R2_BUCKET" --key "$k" --body "$tree/$k" \
    --content-type "$(content_type "$k")" --cache-control "$(cache_control "$k")" \
    --content-md5 "$(openssl dgst -md5 -binary "$tree/$k" | base64)" >/dev/null || die "upload of $k failed"
  n=$((n + 1))
  log "[$n/${#todo[@]}] $k"
done

# 6. Public check: the metadata served by obpkg.org is the one just signed.
if [[ "$OB_R2_BUCKET" == obpkg ]]; then
  for r in "${repos[@]}"; do
    for f in repomd.xml repomd.xml.asc; do
      got="$(curl -fsSL --proto '=https' -H 'Cache-Control: no-cache' "$OB_PUBLIC_URL/$r/repodata/$f?v=$(date +%s)" | sha256sum | cut -d' ' -f1)"
      [[ "$got" == "$(sha256sum "$tree/$r/repodata/$f" | cut -d' ' -f1)" ]] &&
        log "public: $OB_PUBLIC_URL/$r/repodata/$f matches" ||
        log "WARNING: $OB_PUBLIC_URL/$r/repodata/$f does not match yet (edge cache, up to 60 s)"
    done
  done
fi
log "published ${#todo[@]} objects to $OB_R2_BUCKET ($OB_PUBLIC_URL/${repos[0]%/*}/)"
