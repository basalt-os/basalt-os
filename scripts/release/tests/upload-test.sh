#!/usr/bin/env bash
# Tests for scripts/release/upload.sh against a local fake S3 endpoint
# (scripts/release/tests/fake-s3.py): no network, no credentials, no bucket.
#
#   scripts/release/tests/upload-test.sh            run the checks
#   scripts/release/tests/upload-test.sh --time N   time a re-publish of a tree
#                                                   with N already published RPMs
#
# Needs bash, python3, openssl and the AWS CLI v2 (skipped without aws).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
upload="$here/../upload.sh"
command -v aws >/dev/null || { echo "upload-test: aws (AWS CLI v2) not installed, skipped" >&2; exit 0; }
for c in python3 openssl md5sum; do command -v "$c" >/dev/null || { echo "upload-test: $c not installed" >&2; exit 1; }; done

tmp="$(mktemp -d)"
server_pid=
cleanup() {
  if [[ -n "$server_pid" ]]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  rm -rf "$tmp"
}
trap cleanup EXIT
fails=0
pass=0
eq() { # eq DESCRIPTION EXPECTED GOT
  if [[ "$2" == "$3" ]]; then pass=$((pass + 1)); else
    printf 'FAIL %s: expected [%s], got [%s]\n' "$1" "$2" "$3" >&2
    fails=$((fails + 1))
  fi
}
has() { # has DESCRIPTION NEEDLE HAYSTACK
  if [[ "$3" == *"$2"* ]]; then pass=$((pass + 1)); else
    printf 'FAIL %s: [%s] not in output\n' "$1" "$2" >&2
    printf '%s\n' "$3" | sed 's/^/    /' >&2
    fails=$((fails + 1))
  fi
}

# Dummy credentials, as upload.sh wants them (0600 files of the caller).
umask 077
printf 'AKIDTEST\n' >"$tmp/key_id"
printf 'secret-test\n' >"$tmp/secret"
chmod 600 "$tmp/key_id" "$tmp/secret"

# start_server: a fresh, empty fake bucket.
start_server() {
  [[ -n "$server_pid" ]] && { kill "$server_pid" 2>/dev/null; wait "$server_pid" 2>/dev/null || true; }
  rm -rf "$tmp/s3"
  mkdir -p "$tmp/s3/objects" "$tmp/s3/meta"
  : >"$tmp/s3/requests"
  python3 "$here/fake-s3.py" "$tmp/s3" "$tmp/s3/port" &
  server_pid=$!
  for _ in $(seq 100); do [[ -s "$tmp/s3/port" ]] && break; sleep 0.05; done
  [[ -s "$tmp/s3/port" ]] || { echo "fake S3 did not start" >&2; exit 1; }
  endpoint="http://127.0.0.1:$(cat "$tmp/s3/port")"
}

# make_tree DIR N: a signed-looking basalt-testing tree with N binary RPMs,
# one source RPM, two repodata blobs and the repomd.xml pair per directory.
make_tree() {
  local d="$1" n="$2" i sub
  rm -rf "$d"
  for sub in x86_64 source; do mkdir -p "$d/basalt-testing/44/$sub/repodata"; done
  for i in $(seq "$n"); do head -c $((1000 + i)) /dev/urandom >"$d/basalt-testing/44/x86_64/pkg$i-1.0-1.fc44.x86_64.rpm"; done
  head -c 3000 /dev/urandom >"$d/basalt-testing/44/source/pkg1-1.0-1.fc44.src.rpm"
  for sub in x86_64 source; do
    head -c 500 /dev/urandom >"$d/basalt-testing/44/$sub/repodata/aaaa-primary.xml.zst"
    head -c 400 /dev/urandom >"$d/basalt-testing/44/$sub/repodata/bbbb-filelists.xml.zst"
    echo "<repomd $sub $RANDOM/>" >"$d/basalt-testing/44/$sub/repodata/repomd.xml"
    echo "sig $sub $RANDOM" >"$d/basalt-testing/44/$sub/repodata/repomd.xml.asc"
  done
  {
    echo "signed and verified 2026-01-01T00:00:00Z"
    echo "mode: test-key"
    echo "subkey: 0000"
    echo "repo: basalt-testing"
    (cd "$d" && find basalt-testing -type f | sort | xargs -d '\n' sha256sum)
  } >"$d/SIGNED-OK"
}

# seed KEY FILE [ETAG]: put an object in the fake bucket directly.
seed() {
  mkdir -p "$(dirname "$tmp/s3/objects/$1")"
  cp "$2" "$tmp/s3/objects/$1"
  if [[ -n "${3:-}" ]]; then
    mkdir -p "$(dirname "$tmp/s3/meta/$1")"
    printf '{"etag": "%s", "headers": {}}' "$3" >"$tmp/s3/meta/$1.json"
  fi
}

run_upload() { # run_upload TREE: output in $out, status in $rc
  rc=0
  out="$(OB_R2_BUCKET=testbucket OB_R2_ENDPOINT="$endpoint" \
    OB_R2_ACCESS_KEY_ID_FILE="$tmp/key_id" OB_R2_SECRET_ACCESS_KEY_FILE="$tmp/secret" \
    "$upload" "$1" 2>&1)" || rc=$?
}
puts() { grep -c '^PUT ' "$tmp/s3/requests" || true; }

if [[ "${1:-}" == --time ]]; then
  n="${2:?usage: $0 --time N}"
  start_server
  make_tree "$tmp/tree" "$n"
  run_upload "$tmp/tree"
  [[ $rc == 0 ]] || { printf '%s\n' "$out" >&2; exit 1; }
  : >"$tmp/s3/requests"
  t0=$(date +%s.%N)
  run_upload "$tmp/tree"
  t1=$(date +%s.%N)
  [[ $rc == 0 ]] || { printf '%s\n' "$out" >&2; exit 1; }
  printf 're-publish of %s RPMs (all identical): %.1f s, %s requests (%s PUT, %s HEAD, %s LIST, %s GET)\n' \
    "$((n + 1))" "$(echo "$t1 - $t0" | bc)" "$(wc -l <"$tmp/s3/requests")" "$(puts)" \
    "$(grep -c '^HEAD ' "$tmp/s3/requests" || true)" "$(grep -c '^LIST ' "$tmp/s3/requests" || true)" \
    "$(grep -c '^GET ' "$tmp/s3/requests" || true)"
  exit 0
fi

# Paged listings: the bucket answers 3 keys per page.
export FAKE_S3_PAGE_SIZE=3

# --- 1. Empty bucket: everything is uploaded, repomd.xml pairs last ----------------
start_server
make_tree "$tmp/tree" 5
run_upload "$tmp/tree"
eq "first publish exits 0" 0 "$rc"; [[ $rc == 0 ]] || printf "%s\n" "$out" >&2
total_keys=$(cd "$tmp/tree" && find basalt-testing -type f | wc -l)
eq "first publish puts every file" "$total_keys" "$(puts)"
last4="$(grep '^PUT ' "$tmp/s3/requests" | tail -4 | awk '{print $2}' | sed 's|.*/||' | sort | uniq -c | awk '{print $1 $2}' | tr '\n' ' ')"
eq "repomd.xml and its signature go last" "2repomd.xml 2repomd.xml.asc " "$last4"
same=1
while read -r k; do
  cmp -s "$tmp/tree/$k" "$tmp/s3/objects/$k" || same=0
done < <(cd "$tmp/tree" && find basalt-testing -type f)
eq "objects are byte-identical to the tree" 1 "$same"
k=basalt-testing/44/x86_64/pkg1-1.0-1.fc44.x86_64.rpm
meta="$(cat "$tmp/s3/meta/$k.json")"
has "RPM cache-control" "max-age=2592000, immutable" "$meta"
has "RPM content type" "application/x-rpm" "$meta"
has "RPM carries its sha256" "$(sha256sum "$tmp/tree/$k" | cut -d' ' -f1)" "$meta"
has "repomd cache-control" "max-age=60" "$(cat "$tmp/s3/meta/basalt-testing/44/x86_64/repodata/repomd.xml.json")"
eq "one HEAD per new object, no GET" "$((total_keys - 4)) 0" "$(grep -c '^HEAD ' "$tmp/s3/requests" || true) $(grep -c '^GET ' "$tmp/s3/requests" || true)"
eq "listing is paged (3 keys per page)" 1 "$(($(grep -c '^LIST ' "$tmp/s3/requests") >= 2))"

# --- 2. Same tree again: only the metadata is sent --------------------------------
: >"$tmp/s3/requests"
run_upload "$tmp/tree"
eq "re-publish exits 0" 0 "$rc"
eq "re-publish puts only the 4 repomd files" 4 "$(puts)"
eq "every immutable object reported identical" $((total_keys - 4)) "$(grep -c 'already published, identical' <<<"$out" || true)"
eq "re-publish lists, never HEADs or GETs" 0 "$(grep -cE '^(HEAD|GET) ' "$tmp/s3/requests" || true)"

# --- 3. A published RPM with different content: stop before any upload ------------
start_server
make_tree "$tmp/tree" 5
head -c 1001 /dev/urandom >"$tmp/other"
seed basalt-testing/44/x86_64/pkg1-1.0-1.fc44.x86_64.rpm "$tmp/other"
run_upload "$tmp/tree"
eq "different content fails" 1 "$rc"
has "different content message" "already published with different content" "$out"
eq "different content: nothing uploaded" 0 "$(puts)"
eq "different content: object untouched" "$(md5sum <"$tmp/other")" "$(md5sum <"$tmp/s3/objects/basalt-testing/44/x86_64/pkg1-1.0-1.fc44.x86_64.rpm")"

# --- 4. Same name, same MD5 but another size can not happen; another size alone stops --
start_server
make_tree "$tmp/tree" 5
seed basalt-testing/44/source/pkg1-1.0-1.fc44.src.rpm "$tmp/tree/basalt-testing/44/source/pkg1-1.0-1.fc44.src.rpm" "$(md5sum <"$tmp/tree/basalt-testing/44/source/pkg1-1.0-1.fc44.src.rpm" | cut -d' ' -f1)"
printf 'x' >>"$tmp/s3/objects/basalt-testing/44/source/pkg1-1.0-1.fc44.src.rpm"
run_upload "$tmp/tree"
eq "size mismatch fails" 1 "$rc"
eq "size mismatch: nothing uploaded" 0 "$(puts)"

# --- 5. Multipart-style ETag: proved by download, identical is skipped -------------
start_server
make_tree "$tmp/tree" 5
k=basalt-testing/44/x86_64/pkg2-1.0-1.fc44.x86_64.rpm
seed "$k" "$tmp/tree/$k" "0123456789abcdef0123456789abcdef-2"
run_upload "$tmp/tree"
eq "multipart identical exits 0" 0 "$rc"
has "multipart identical skipped" "already published, identical: $k" "$out"
eq "multipart identical not re-uploaded" 0 "$(grep -c "^PUT $k\$" "$tmp/s3/requests" || true)"
eq "multipart identical fetched once" 1 "$(grep -c "^GET $k\$" "$tmp/s3/requests" || true)"

# --- 6. Multipart-style ETag with different content: stop --------------------------
start_server
make_tree "$tmp/tree" 5
seed "$k" "$tmp/other" "0123456789abcdef0123456789abcdef-2"
run_upload "$tmp/tree"
eq "multipart different fails" 1 "$rc"
has "multipart different message" "already published with different content" "$out"
eq "multipart different: nothing uploaded" 0 "$(puts)"

# --- 7. An upload that fails stops before the metadata ----------------------------
start_server
make_tree "$tmp/tree" 5
kill "$server_pid"
wait "$server_pid" 2>/dev/null || true
server_pid=
run_upload "$tmp/tree"
eq "unreachable endpoint fails" 1 "$rc"
has "unreachable endpoint stops at the listing" "cannot list" "$out"

# --- 8. Parallel uploads still put the metadata last, with OB_UPLOAD_JOBS=1 too -----
for jobs in 1 8; do
  start_server
  make_tree "$tmp/tree" 12
  rc=0
  out="$(OB_UPLOAD_JOBS=$jobs OB_R2_BUCKET=testbucket OB_R2_ENDPOINT="$endpoint" \
    OB_R2_ACCESS_KEY_ID_FILE="$tmp/key_id" OB_R2_SECRET_ACCESS_KEY_FILE="$tmp/secret" \
    "$upload" "$tmp/tree" 2>&1)" || rc=$?
  eq "jobs=$jobs exits 0" 0 "$rc"
  first_meta="$(grep -n '^PUT .*/repomd.xml' "$tmp/s3/requests" | head -1 | cut -d: -f1)"
  last_blob="$(grep -n '^PUT ' "$tmp/s3/requests" | grep -v '/repomd.xml' | tail -1 | cut -d: -f1)"
  eq "jobs=$jobs: metadata after every other object" 1 "$((first_meta > last_blob))"
done

# --- 9. A bad OB_UPLOAD_JOBS is refused --------------------------------------------
rc=0
OB_UPLOAD_JOBS=0 OB_R2_BUCKET=testbucket OB_R2_ENDPOINT="$endpoint" \
  OB_R2_ACCESS_KEY_ID_FILE="$tmp/key_id" OB_R2_SECRET_ACCESS_KEY_FILE="$tmp/secret" \
  "$upload" "$tmp/tree" >/dev/null 2>&1 || rc=$?
eq "OB_UPLOAD_JOBS=0 refused" 1 "$rc"

# --- 10. APT tree: .deb and by-hash objects immutable, InRelease last --------------
start_server
a="$tmp/apt-tree"
rm -rf "$a"
mkdir -p "$a/apt/pool/main/c/conductor" "$a/apt/dists/stable/main/binary-amd64/by-hash/SHA256"
for v in 1 2; do head -c 2000 /dev/urandom >"$a/apt/pool/main/c/conductor/conductor_${v}_amd64.deb"; done
echo "Package: conductor" >"$a/apt/dists/stable/main/binary-amd64/Packages"
cp "$a/apt/dists/stable/main/binary-amd64/Packages" "$a/apt/dists/stable/main/binary-amd64/by-hash/SHA256/$(sha256sum <"$a/apt/dists/stable/main/binary-amd64/Packages" | cut -d' ' -f1)"
echo "Archive: stable" >"$a/apt/dists/stable/main/binary-amd64/Release"
for f in Release Release.gpg InRelease; do echo "$f $RANDOM" >"$a/apt/dists/stable/$f"; done
{
  echo "mode: test-key"
  echo "repo: apt"
  (cd "$a" && find apt -type f | sort | xargs -d '\n' sha256sum)
} >"$a/SIGNED-OK"
run_upload "$a"
eq "apt first publish exits 0" 0 "$rc"
eq "apt first publish puts every file" 8 "$(puts)"
eq "apt InRelease goes last" "PUT apt/dists/stable/InRelease" "$(tail -1 "$tmp/s3/requests")"
: >"$tmp/s3/requests"
run_upload "$a"
eq "apt re-publish exits 0" 0 "$rc"
eq "apt re-publish skips the .deb and by-hash objects" 3 "$(grep -c 'already published, identical' <<<"$out" || true)"
eq "apt re-publish puts only metadata" 5 "$(puts)"
eq "apt re-publish lists apt/ only, never HEADs" "0 0" "$(grep '^LIST ' "$tmp/s3/requests" | grep -vc '^LIST apt/$' || true) $(grep -c '^HEAD ' "$tmp/s3/requests" || true)"

printf 'upload-test: %d passed, %d failed\n' "$pass" "$fails" >&2
[[ $fails == 0 ]]
