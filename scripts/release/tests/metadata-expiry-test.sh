#!/usr/bin/env bash
# Tests for repository metadata expiry (security review L-1):
# - scripts/release/metadata-expiry.sh on local repomd.xml pairs: a
#   signature that expires later passes; one that never expires, has
#   expired, expires too soon or comes from another key fails;
# - the client side: dnf, with repo_gpgcheck=1 as basalt.repo sets it,
#   refuses metadata whose signature has expired and accepts a current one.
#   This part needs root, dnf5 and createrepo_c (the CI lint container has
#   them); elsewhere it is skipped, in CI it must run.
#
#   scripts/release/tests/metadata-expiry-test.sh
#
# No network, no real key: throwaway keys in a temporary GNUPGHOME.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
check="$here/../metadata-expiry.sh"
command -v gpg >/dev/null || { echo "metadata-expiry-test: gpg not installed" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export GNUPGHOME="$tmp/gnupg"
mkdir -m 700 "$GNUPGHOME"
fails=0 pass=0
ok() { pass=$((pass + 1)); }
fail() { printf 'FAIL %s\n' "$*" >&2; fails=$((fails + 1)); }

# Two throwaway signing keys, made in the past so that past signatures exist.
t0=20260101T000000
gpg -q --batch --faked-system-time "$t0!" --passphrase '' --quick-gen-key 'expiry test <test@example.invalid>' rsa3072 sign 0 2>/dev/null
gpg -q --batch --faked-system-time "$t0!" --passphrase '' --quick-gen-key 'other key <other@example.invalid>' rsa3072 sign 0 2>/dev/null
fpr="$(gpg --batch --with-colons --list-keys 'expiry test' 2>/dev/null | awk -F: '/^fpr:/ {print $10; exit}')"
other="$(gpg --batch --with-colons --list-keys 'other key' 2>/dev/null | awk -F: '/^fpr:/ {print $10; exit}')"
gpg --batch --armor --export "$fpr" >"$tmp/key.asc"

printf '<repomd>test</repomd>\n' >"$tmp/repomd.xml"
past="$(date -u -d '-40 days' +%Y%m%dT%H%M%S)"
sign() { # sign NAME KEY [gpg options]
  local name="$1" key="$2"
  shift 2
  gpg -q --batch --local-user "$key!" "$@" --detach-sign --armor -o "$tmp/$name.asc" "$tmp/repomd.xml" 2>/dev/null
}
sign fresh "$fpr" --default-sig-expire 30d
sign soon "$fpr" --default-sig-expire 3d
sign never "$fpr"
sign expired "$fpr" --faked-system-time "$past!" --default-sig-expire 30d
sign otherkey "$other" --default-sig-expire 30d

run_check() { # run_check NAME: status in $rc, output in $out
  rc=0
  out="$(OB_RELEASE_PUBKEY="$tmp/key.asc" OB_SIGNING_SUBKEY="$fpr" "$check" --file "$tmp/repomd.xml" "$tmp/$1.asc" 2>&1)" || rc=$?
}
run_check fresh
[[ $rc == 0 && "$out" == *"ok "* ]] && ok || fail "a signature that expires in 30 days must pass: $out"
for c in soon expired otherkey; do
  run_check "$c"
  [[ $rc != 0 && "$out" == *FAIL* ]] && ok || fail "$c must fail: $out"
done
# A signature from before metadata expiry: a warning, not a failure.
run_check never
[[ $rc == 0 && "$out" == *WARN*"never expires"* && "$out" != *FAIL* ]] && ok ||
  fail "a signature without an expiration must warn and pass: $out"
# Under GitHub Actions: annotations and job summary lines.
summary="$tmp/summary.md"
: >"$summary"
rc=0
out="$(GITHUB_ACTIONS=true GITHUB_STEP_SUMMARY="$summary" OB_RELEASE_PUBKEY="$tmp/key.asc" OB_SIGNING_SUBKEY="$fpr" \
  "$check" --file "$tmp/repomd.xml" "$tmp/never.asc" 2>&1)" || rc=$?
[[ $rc == 0 && "$out" == *"::warning title=Repository metadata expiry::"* ]] && ok || fail "no warning annotation: $out"
grep -q '^- warning: .*sign the repository again' "$summary" && ok || fail "no warning line in the job summary: $(cat "$summary")"
: >"$summary"
rc=0
out="$(GITHUB_ACTIONS=true GITHUB_STEP_SUMMARY="$summary" OB_RELEASE_PUBKEY="$tmp/key.asc" OB_SIGNING_SUBKEY="$fpr" \
  "$check" --file "$tmp/repomd.xml" "$tmp/expired.asc" 2>&1)" || rc=$?
[[ $rc != 0 && "$out" == *"::error title=Repository metadata expiry::"* ]] && ok || fail "no error annotation: $out"
grep -q '^- error: .*expired' "$summary" && ok || fail "no error line in the job summary: $(cat "$summary")"
# The check itself cannot run: a missing key fails.
rc=0
out="$(OB_RELEASE_PUBKEY="$tmp/missing.asc" OB_SIGNING_SUBKEY="$fpr" "$check" --file "$tmp/repomd.xml" "$tmp/fresh.asc" 2>&1)" || rc=$?
[[ $rc != 0 ]] && ok || fail "a check that cannot import the key must fail: $out"

# Client side: dnf refuses an expired repository signature.
if [[ $(id -u) == 0 ]] && command -v dnf5 >/dev/null && command -v createrepo_c >/dev/null; then
  root="$tmp/root"
  mkdir -p "$root/etc/yum.repos.d" "$tmp/repo"
  createrepo_c -q "$tmp/repo"
  dnf_try() { # dnf_try SIGNATURE_NAME: status of a metadata download with repo_gpgcheck=1
    cp "$tmp/$1-repomd.asc" "$tmp/repo/repodata/repomd.xml.asc"
    rm -rf "$root/var/cache" "$root/var/lib"
    cat >"$root/etc/yum.repos.d/t.repo" <<EOF
[t]
name=metadata expiry test
baseurl=file://$tmp/repo
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://$tmp/key.asc
skip_if_unavailable=False
EOF
    dnf5 -q -y --installroot="$root" --setopt=reposdir="$root/etc/yum.repos.d" --releasever=44 \
      --disablerepo='*' --enablerepo=t makecache >"$tmp/dnf.log" 2>&1
  }
  for c in fresh expired; do
    o=()
    [[ $c == expired ]] && o=(--faked-system-time "$past!")
    gpg -q --batch --local-user "$fpr!" "${o[@]}" --default-sig-expire 30d --detach-sign --armor \
      -o "$tmp/$c-repomd.asc" "$tmp/repo/repodata/repomd.xml" 2>/dev/null
  done
  if dnf_try fresh; then ok; else fail "dnf refused a current repository signature: $(tail -3 "$tmp/dnf.log")"; fi
  if dnf_try expired; then fail "dnf accepted an expired repository signature"; else
    grep -qi 'not alive\|expired' "$tmp/dnf.log" && ok || fail "dnf refused the expired signature for another reason: $(tail -3 "$tmp/dnf.log")"
  fi
elif [[ -n "${CI:-}" ]]; then
  fail "the dnf part needs root, dnf5 and createrepo_c in CI"
else
  echo "metadata-expiry-test: dnf part skipped (needs root, dnf5 and createrepo_c)" >&2
fi

printf 'metadata-expiry-test: %d passed, %d failed\n' "$pass" "$fails" >&2
[[ $fails == 0 ]]
