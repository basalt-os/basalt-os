#!/usr/bin/env bash
# When does the published repository metadata expire? sign.sh gives every
# repomd.xml.asc a signature expiration time (OB_METADATA_VALID_DAYS) and
# dnf refuses metadata whose signature has expired, so each repository must
# be signed again before then (docs/publishing.md, Metadata validity). This
# check needs no key material and no credentials: it reads the public
# repositories and verifies them with the public release key.
#
#   scripts/release/metadata-expiry.sh [--min-days N] [REPO]  (REPO may be given more than once)
#   scripts/release/metadata-expiry.sh --file REPOMD_XML REPOMD_XML_ASC [--min-days N]
#
# REPO: basalt, basalt-tools, basalt-testing, basalt-nonfree,
# basalt-nonfree-testing (default: all of them), under OB_PUBLIC_URL
# (default https://obpkg.org), for FEDORA_RELEASE and ARCH plus source. A
# repository that is not published (404) is skipped. --file checks one
# local pair instead.
#
# It fails when a signature is not a good one by the packages subkey
# (OB_SIGNING_SUBKEY, with the key in OB_RELEASE_PUBKEY), has expired, or
# expires in fewer than N days (default 10), and when the check cannot run
# (download, key import). A signature without an expiration (made before
# metadata expiry existed) is a warning, not a failure: the repository
# should be signed again, but nothing is about to break. Under GitHub
# Actions both show as annotations and as lines of the job summary.
# The scheduled workflow .github/workflows/metadata-expiry.yml runs it, so
# a repository about to expire shows up as a failed run.
source "$(dirname "$0")/../lib.sh"

: "${OB_PUBLIC_URL:=https://obpkg.org}"
: "${OB_SIGNING_SUBKEY:=302461D26520E077D07FFCA9AA27C62C36CCFC4B}"
: "${OB_RELEASE_PUBKEY:=$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt}"
min_days=10
file_mode=0 repomd="" asc=""
repos=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --min-days) min_days="${2:?--min-days N}"; shift 2 ;;
    --file) file_mode=1 repomd="${2:?--file REPOMD_XML REPOMD_XML_ASC}" asc="${3:?--file REPOMD_XML REPOMD_XML_ASC}"; shift 3 ;;
    -h | --help) sed -n '2,22p' "$0"; exit 0 ;;
    basalt | basalt-tools | basalt-testing | basalt-nonfree | basalt-nonfree-testing) repos+=("$1"); shift ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
[[ "$min_days" =~ ^[0-9]{1,2}$ ]] || die "--min-days must be 0 to 99"
[[ ${#repos[@]} -gt 0 ]] || repos=(basalt basalt-tools basalt-testing basalt-nonfree basalt-nonfree-testing)
for c in curl gpg; do command -v "$c" >/dev/null || die "$c not installed"; done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --import "$OB_RELEASE_PUBKEY" 2>/dev/null || die "cannot import $OB_RELEASE_PUBKEY"
fpr="${OB_SIGNING_SUBKEY^^}"
now="$(date +%s)"
fails=0 warns=0

# note LEVEL TEXT: a GitHub Actions annotation (error or warning) and a
# line of the job summary, when running there.
note() {
  [[ "${GITHUB_ACTIONS:-}" == true ]] || return 0
  echo "::$1 title=Repository metadata expiry::$2"
  [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] && printf -- '- %s: %s\n' "$1" "$2" >>"$GITHUB_STEP_SUMMARY"
  return 0
}
# failed TEXT, warned TEXT: print, annotate, count.
failed() { echo "FAIL $1"; note error "$1"; fails=$((fails + 1)); }
warned() { echo "WARN $1"; note warning "$1"; warns=$((warns + 1)); }

# check NAME REPOMD ASC: print when the signature expires; count a failure.
check() {
  local name="$1" st exp left
  st="$(gpg --batch --status-fd 1 --verify "$3" "$2" 2>/dev/null || true)"
  if ! grep -q "^\[GNUPG:\] VALIDSIG $fpr " <<<"$st" || grep -q '^\[GNUPG:\] REVKEYSIG ' <<<"$st"; then
    failed "$name: not a good signature by the packages subkey $fpr"
    return
  fi
  # VALIDSIG <fpr> <date> <created> <expires> and more fields; 0 means it never expires.
  exp="$(awk '/^\[GNUPG:\] VALIDSIG / {print $6; exit}' <<<"$st")"
  if ! [[ "$exp" =~ ^[0-9]+$ ]] || [[ "$exp" == 0 ]]; then
    warned "$name: the signature never expires (signed before metadata expiry existed): sign the repository again with scripts/release/sign.sh (docs/publishing.md, Metadata validity)"
    return
  fi
  left=$(( (exp - now) / 86400 ))
  if (( exp <= now )); then
    failed "$name: expired on $(date -u -d "@$exp" +%Y-%m-%dT%H:%M:%SZ): clients refuse it, sign the repository again"
  elif (( exp - now < min_days * 86400 )); then
    failed "$name: expires on $(date -u -d "@$exp" +%Y-%m-%dT%H:%M:%SZ), in $left day(s) (under $min_days): sign the repository again"
  else
    echo "ok   $name: expires on $(date -u -d "@$exp" +%Y-%m-%dT%H:%M:%SZ), in $left days"
  fi
}

# fetch URL FILE: 0 when fetched, 1 when the server answers 404, dies otherwise.
fetch() {
  local code
  code="$(curl -sSL --proto '=https' "${CURL_RETRY[@]}" -H 'Cache-Control: no-cache' -o "$2" -w '%{http_code}' "$1")" ||
    die "cannot fetch $1"
  case "$code" in 200) return 0 ;; 404) return 1 ;; *) die "$1: HTTP $code" ;; esac
}

if [[ $file_mode == 1 ]]; then
  check "$repomd" "$repomd" "$asc"
else
  for repo in "${repos[@]}"; do
    for sub in "$ARCH" source; do
      url="$OB_PUBLIC_URL/$repo/$FEDORA_RELEASE/$sub/repodata"
      if ! fetch "$url/repomd.xml?v=$now" "$work/repomd.xml"; then
        echo "--   $repo/$FEDORA_RELEASE/$sub: not published"
        continue
      fi
      fetch "$url/repomd.xml.asc?v=$now" "$work/repomd.xml.asc" || die "$url/repomd.xml.asc missing"
      check "$repo/$FEDORA_RELEASE/$sub" "$work/repomd.xml" "$work/repomd.xml.asc"
    done
  done
fi
if [[ "${GITHUB_ACTIONS:-}" == true && -n "${GITHUB_STEP_SUMMARY:-}" && $((fails + warns)) == 0 ]]; then
  echo "- every published repository metadata signature expires in $min_days days or more" >>"$GITHUB_STEP_SUMMARY"
fi
(( warns == 0 )) || log "$warns signature(s) without an expiration: sign those repositories again"
[[ $fails == 0 ]] || die "$fails repository metadata signature(s) need attention"
