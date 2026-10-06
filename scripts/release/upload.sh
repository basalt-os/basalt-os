#!/usr/bin/env bash
# Publish a signed repository tree (scripts/release/sign.sh) to the obpkg
# bucket (Cloudflare R2, served as https://obpkg.org).
#
#   scripts/release/upload.sh [--dry-run] TREE_DIR
#
# TREE_DIR is sign.sh's OUT_DIR: SIGNED-OK and <repo>/<releasever>/{<arch>,source}/,
# where <repo> (basalt, basalt-tools, basalt-testing or basalt-nonfree) is the "repo:" line
# of SIGNED-OK (basalt when the line is missing); or sign-apt.sh's OUT_DIR:
# SIGNED-OK with "repo: apt" and apt/{pool,dists}/.
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
# Order: RPMs and repodata blobs first (OB_UPLOAD_JOBS at a time, default
# 4), then, once all of them are in the bucket, repomd.xml and repomd.xml.asc
# of each repository, so a client never sees metadata that points at missing
# files. Cache-Control: repomd.xml and its signature public, max-age=60; RPMs
# and repodata blobs (content-addressed names) public, max-age=2592000,
# immutable. A published RPM is never replaced: one listing of each
# repository directory gives the ETag (the MD5 of a single-part upload) and
# size of every published object, and an existing object with the same key
# and other content stops the upload before anything is sent. A new object
# is checked once more with a HEAD right before its upload. Objects carry
# their SHA-256 as metadata (x-amz-meta-sha256).
# APT: .deb files and the by-hash indexes, then Packages* and the
# per-architecture Release, then Release, Release.gpg and InRelease last.
# .deb files and by-hash objects are immutable (never replaced, max-age
# 30 days); the other files under dists/ are metadata (max-age=60).
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
: "${OB_UPLOAD_JOBS:=4}"
[[ "$OB_UPLOAD_JOBS" =~ ^[1-9][0-9]?$ ]] || die "OB_UPLOAD_JOBS must be 1 to 99"

# 1. The tree is exactly what sign.sh verified.
[[ -f "$tree/SIGNED-OK" ]] || die "$tree/SIGNED-OK missing: sign and verify with scripts/release/sign.sh first"
mode="$(awk '/^mode: / {print $2}' "$tree/SIGNED-OK")"
if [[ "$mode" == test-key && "$OB_R2_BUCKET" == obpkg ]]; then
  die "this tree was signed with a throwaway test key; it never goes to the obpkg bucket"
fi
repo="$(awk '/^repo: / {print $2}' "$tree/SIGNED-OK")"
repo="${repo:-basalt}"
case "$repo" in basalt | basalt-tools | basalt-testing | basalt-nonfree | basalt-nonfree-testing | apt) ;; *) die "unknown repository \"$repo\" in SIGNED-OK" ;; esac
listed="$(grep -E "^[0-9a-f]{64}  $repo/" "$tree/SIGNED-OK")"
(cd "$tree" && sha256sum -c --quiet <<<"$listed") || die "the tree changed after it was verified"
[[ "$(awk '{print $2}' <<<"$listed" | sort)" == "$(cd "$tree" && find "$repo" -type f | sort)" ]] ||
  die "files in $tree/$repo and SIGNED-OK differ"

# 2. Upload plan.
if [[ "$repo" == apt ]]; then
  # Suites in this tree (sign-apt.sh signs one suite per run).
  mapfile -t repos < <(cd "$tree" && find apt/dists -mindepth 2 -maxdepth 2 -name InRelease -printf '%h\n' | sort)
  [[ ${#repos[@]} -gt 0 ]] || die "no suites in $tree/apt/dists"
  plan=()
  mapfile -t -O 0 plan < <(cd "$tree" && find apt/pool -type f -name '*.deb' | LC_ALL=C sort)
  mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find apt/dists -type f -path '*/by-hash/*' | LC_ALL=C sort)
  mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find apt/dists -mindepth 3 -type f ! -path '*/by-hash/*' | LC_ALL=C sort)
  for r in "${repos[@]}"; do plan+=("$r/Release" "$r/Release.gpg" "$r/InRelease"); done
  [[ ${#plan[@]} -eq $(cd "$tree" && find apt -type f | wc -l) ]] || die "unexpected files in $tree/apt"
else
mapfile -t repos < <(cd "$tree" && find "$repo" -type f -path '*/repodata/repomd.xml' -printf '%h\n' | xargs -n1 dirname | sort)
[[ ${#repos[@]} -gt 0 ]] || die "no repositories in $tree"
plan=()
for r in "${repos[@]}"; do [[ "$r" == */source ]] && mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find "$r" -maxdepth 1 -name '*.rpm' | sort); done
for r in "${repos[@]}"; do [[ "$r" != */source ]] && mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find "$r" -maxdepth 1 -name '*.rpm' | sort); done
for r in "${repos[@]}"; do
  mapfile -t -O "${#plan[@]}" plan < <(cd "$tree" && find "$r/repodata" -type f ! -name 'repomd.xml*' | sort)
done
for r in "${repos[@]}"; do plan+=("$r/repodata/repomd.xml" "$r/repodata/repomd.xml.asc"); done
fi

content_type() {
  case "$1" in
    *.rpm) echo application/x-rpm ;;
    *.deb) echo application/vnd.debian.binary-package ;;
    */by-hash/SHA256/*) echo application/octet-stream ;;
    */InRelease | */Release | */Packages) echo "text/plain; charset=utf-8" ;;
    */Release.gpg) echo application/pgp-signature ;;
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
# Mutable metadata: replaced on every publish.
is_metadata() {
  case "$1" in
    */repomd.xml | */repomd.xml.asc) return 0 ;;
    apt/dists/*/by-hash/*) return 1 ;;
    apt/dists/*) return 0 ;;
    *) return 1 ;;
  esac
}
cache_control() {
  case "$1" in
    */by-hash/*) echo "public, max-age=2592000, immutable" ;;
    */repomd.xml | */repomd.xml.asc | apt/dists/*) echo "public, max-age=60" ;;
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
# One paged listing per repository directory gives the key, ETag and size of
# every published object (instead of one request per object). put-object is
# a single-part upload (multipart_threshold above), so the ETag of what this
# script published is the MD5 of the content; an object with another ETag form
# (multipart) is proved by its sha256 metadata or by downloading it.
log "listing published objects in $OB_R2_BUCKET"
if [[ "$repo" == apt ]]; then prefixes=(apt/); else prefixes=(); for r in "${repos[@]}"; do prefixes+=("$r/"); done; fi
declare -A remote_etag=() remote_size=()
i=0
for p in "${prefixes[@]}"; do
  i=$((i + 1))
  # A failed listing must stop here: read as empty, it would let a different
  # object be overwritten.
  s3api list-objects-v2 --bucket "$OB_R2_BUCKET" --prefix "$p" \
    --query 'Contents[].[Key,ETag,Size]' --output text >"$cfg/list.$i" || die "cannot list s3://$OB_R2_BUCKET/$p"
  while IFS=$'\t' read -r key etag size; do
    [[ -z "$key" || "$key" == None ]] && continue
    remote_etag["$key"]="${etag//\"/}"
    remote_size["$key"]="$size"
  done <"$cfg/list.$i"
done
log "${#remote_etag[@]} objects published under ${prefixes[*]}"

dl="$(mktemp -d "${TMPDIR:-/tmp}/basalt-upload-check.XXXXXX")"
trap 'find "$cfg" -type f -exec shred -u {} + 2>/dev/null; rm -rf "$cfg" "$dl"' EXIT

# same_object KEY ETAG SIZE: 0 when the published object with this ETag and
# size has the content of the tree's file, 1 when it differs, dies when it
# cannot tell.
same_object() {
  local k="$1" etag="$2" size="$3" f="$tree/$1" sha meta tmpf
  [[ "$size" == "$(stat -c %s "$f")" ]] || return 1
  if [[ "$etag" =~ ^[0-9a-f]{32}$ ]]; then
    [[ "$etag" == "$(md5sum "$f" | cut -d' ' -f1)" ]]
    return
  fi
  sha="$(sha256sum "$f" | cut -d' ' -f1)"
  meta="$(s3api head-object --bucket "$OB_R2_BUCKET" --key "$k" --query 'Metadata.sha256' --output text)" ||
    die "cannot read the metadata of $k"
  if [[ "$meta" =~ ^[0-9a-f]{64}$ ]]; then
    [[ "$meta" == "$sha" ]]
    return
  fi
  tmpf="$(mktemp "$dl/obj.XXXXXX")"
  s3api get-object --bucket "$OB_R2_BUCKET" --key "$k" "$tmpf" >/dev/null || die "cannot download $k to compare it"
  [[ "$(sha256sum "$tmpf" | cut -d' ' -f1)" == "$sha" ]] && { rm -f "$tmpf"; return 0; }
  rm -f "$tmpf"
  return 1
}

immutable=() metadata=()
for k in "${plan[@]}"; do
  if is_metadata "$k"; then
    metadata+=("$k")
  elif [[ -n "${remote_etag[$k]+x}" ]]; then
    if same_object "$k" "${remote_etag[$k]}" "${remote_size[$k]}"; then
      log "already published, identical: $k"
    else
      die "$k is already published with different content; a published file is never replaced"
    fi
  else
    immutable+=("$k")
  fi
done

# put_object KEY: one single-part upload, checked by the server against
# Content-MD5, with the SHA-256 of the content as object metadata.
put_object() {
  local k="$1"
  s3api put-object --bucket "$OB_R2_BUCKET" --key "$k" --body "$tree/$k" \
    --content-type "$(content_type "$k")" --cache-control "$(cache_control "$k")" \
    --metadata "sha256=$(sha256sum "$tree/$k" | cut -d' ' -f1)" \
    --content-md5 "$(openssl dgst -md5 -binary "$tree/$k" | base64)" >/dev/null
}

# new_object KEY: upload an object the listing did not show. A HEAD first
# covers an object published since the listing (or missing from it): that one
# is compared like a listed one and never replaced.
new_object() {
  local k="$1" head err etag size ef="$cfg/err.$BASHPID"
  if head="$(s3api head-object --bucket "$OB_R2_BUCKET" --key "$k" --query '[ETag,ContentLength]' --output text 2>"$ef")"; then
    read -r etag size <<<"${head//\"/}"
    if same_object "$k" "$etag" "$size"; then
      log "already published, identical: $k"
      return 0
    fi
    printf 'error: %s is already published with different content; a published file is never replaced\n' "$k" >&2
    return 1
  fi
  err="$(cat "$ef")"
  [[ "$err" == *"(404)"* || "$err" == *"Not Found"* ]] || { printf 'error: cannot check %s: %s\n' "$k" "$err" >&2; return 1; }
  put_object "$k" || { printf 'error: upload of %s failed\n' "$k" >&2; return 1; }
  log "uploaded $k"
}

# 5. Upload: new RPMs and blobs in parallel (at most OB_UPLOAD_JOBS at once),
# then, once every one of them is in the bucket, the metadata in order.
log "uploading ${#immutable[@]} new objects (${OB_UPLOAD_JOBS} at a time), then ${#metadata[@]} metadata files"
failed=0 running=0
for k in "${immutable[@]}"; do
  if ((running >= OB_UPLOAD_JOBS)); then
    wait -n || failed=1
    running=$((running - 1))
  fi
  [[ $failed == 0 ]] || break
  new_object "$k" &
  running=$((running + 1))
done
while ((running > 0)); do
  wait -n || failed=1
  running=$((running - 1))
done
[[ $failed == 0 ]] || die "an upload failed; no metadata was published"
n=0
for k in "${metadata[@]}"; do
  put_object "$k" || die "upload of $k failed"
  n=$((n + 1))
  log "[$n/${#metadata[@]}] $k"
done
todo=("${immutable[@]}" "${metadata[@]}")

# 6. Public check: the metadata served by obpkg.org is the one just signed.
if [[ "$OB_R2_BUCKET" == obpkg && "$repo" == apt ]]; then
  for r in "${repos[@]}"; do
    for f in InRelease Release; do
      got="$(curl -fsSL --proto '=https' -H 'Cache-Control: no-cache' "$OB_PUBLIC_URL/$r/$f?v=$(date +%s)" | sha256sum | cut -d' ' -f1)"
      [[ "$got" == "$(sha256sum "$tree/$r/$f" | cut -d' ' -f1)" ]] &&
        log "public: $OB_PUBLIC_URL/$r/$f matches" ||
        log "WARNING: $OB_PUBLIC_URL/$r/$f does not match yet (edge cache, up to 60 s)"
    done
  done
elif [[ "$OB_R2_BUCKET" == obpkg ]]; then
  for r in "${repos[@]}"; do
    for f in repomd.xml repomd.xml.asc; do
      got="$(curl -fsSL --proto '=https' -H 'Cache-Control: no-cache' "$OB_PUBLIC_URL/$r/repodata/$f?v=$(date +%s)" | sha256sum | cut -d' ' -f1)"
      [[ "$got" == "$(sha256sum "$tree/$r/repodata/$f" | cut -d' ' -f1)" ]] &&
        log "public: $OB_PUBLIC_URL/$r/repodata/$f matches" ||
        log "WARNING: $OB_PUBLIC_URL/$r/repodata/$f does not match yet (edge cache, up to 60 s)"
    done
  done
fi
log "published ${#todo[@]} objects to $OB_R2_BUCKET ($OB_PUBLIC_URL/$repo/)"
