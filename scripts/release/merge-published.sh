#!/usr/bin/env bash
# Start a publish from the whole published package set: download every RPM
# of the repository as served by https://obpkg.org into a release build, so
# the metadata that scripts/release/sign.sh writes keeps the older versions.
#
#   OB_REPO=basalt scripts/release/merge-published.sh IN_DIR
#
# IN_DIR is the output of scripts/release/build.sh (or build-testing.sh with
# OB_REPO=basalt-testing). For <repo>/<releasever>/{<arch>,source}:
# - repomd.xml must carry a valid signature of the packages subkey
#   (OB_SIGNING_SUBKEY) made with the release key (OB_RELEASE_PUBKEY), and
#   every file is checked against the checksums of that signed metadata;
# - every published RPM lands in IN_DIR byte-identical (it is already
#   signed). A built RPM with the same file name is replaced by the
#   published one: a published file is never replaced (upload.sh), so a
#   package whose content changed needs a new release number;
# - IN_DIR/PUBLISHED lists the published RPMs (sha256sum format). sign.sh
#   keeps them as they are (no re-signing) and checks their signature.
# SHA256SUMS is rewritten over all the RPMs. A repository that is not
# published yet (repomd.xml answers 404) adds nothing.
source "$(dirname "$0")/../lib.sh"

: "${OB_REPO:=basalt}"
case "$OB_REPO" in basalt | basalt-tools | basalt-testing) ;; *) die "OB_REPO must be basalt, basalt-tools or basalt-testing" ;; esac
: "${OB_PUBLIC_URL:=https://obpkg.org}"
: "${OB_SIGNING_SUBKEY:=302461D26520E077D07FFCA9AA27C62C36CCFC4B}"
: "${OB_RELEASE_PUBKEY:=$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt}"
in="${1:?usage: OB_REPO=basalt $0 IN_DIR}"
in="$(cd "$in" && pwd)" || die "no $in"
[[ -f "$in/SHA256SUMS" ]] || die "$in/SHA256SUMS missing (scripts/release/build.sh)"
(cd "$in" && sha256sum -c --quiet SHA256SUMS) || die "checksum mismatch in $in"
[[ ! -e "$in/PUBLISHED" ]] || die "$in/PUBLISHED exists: the published set is merged already"
for c in curl gpg python3; do command -v "$c" >/dev/null || die "$c not installed"; done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg --batch --quiet --import "$OB_RELEASE_PUBKEY" 2>/dev/null || die "cannot import $OB_RELEASE_PUBKEY"

# fetch URL FILE: 0 when fetched, 1 when the server answers 404, dies otherwise.
fetch() {
  local code
  code="$(curl -sSL --proto '=https' -H 'Cache-Control: no-cache' -o "$2" -w '%{http_code}' "$1")" || die "cannot fetch $1"
  case "$code" in 200) return 0 ;; 404) return 1 ;; *) die "$1: HTTP $code" ;; esac
}

: >"$work/published"
kept=0 replaced=0
for sub in "$ARCH" source; do
  url="$OB_PUBLIC_URL/$OB_REPO/$FEDORA_RELEASE/$sub"
  if ! fetch "$url/repodata/repomd.xml?v=$(date +%s)" "$work/repomd.xml"; then
    log "$url: not published, nothing to merge"
    continue
  fi
  fetch "$url/repodata/repomd.xml.asc?v=$(date +%s)" "$work/repomd.xml.asc" || die "$url/repodata/repomd.xml.asc missing"
  gpg --batch --status-fd 1 --verify "$work/repomd.xml.asc" "$work/repomd.xml" 2>/dev/null |
    grep -q "^\[GNUPG:\] VALIDSIG ${OB_SIGNING_SUBKEY^^} " || die "$url/repodata/repomd.xml: bad signature"
  # primary.xml location and checksum from the signed repomd.xml, then each
  # package's file name and sha256 from primary.xml.
  read -r href sum < <(python3 - "$work/repomd.xml" <<'PY'
import sys, xml.etree.ElementTree as ET
ns = {"r": "http://linux.duke.edu/metadata/repo"}
d = ET.parse(sys.argv[1]).find("r:data[@type='primary']", ns)
c = d.find("r:checksum", ns)
assert c.get("type") == "sha256", "primary checksum is not sha256"
print(d.find("r:location", ns).get("href"), c.text)
PY
  ) || die "cannot read $url/repodata/repomd.xml"
  fetch "$url/$href" "$work/primary" || die "$url/$href missing"
  [[ "$(sha256sum "$work/primary" | cut -d' ' -f1)" == "$sum" ]] || die "$url/$href: checksum mismatch"
  python3 - "$work/primary" >"$work/list" <<'PY' || die "cannot read $url/$href"
import gzip, lzma, sys, xml.etree.ElementTree as ET
raw = open(sys.argv[1], "rb").read()
if raw[:2] == b"\x1f\x8b":
    raw = gzip.decompress(raw)
elif raw[:6] == b"\xfd7zXZ\x00":
    raw = lzma.decompress(raw)
elif raw[:4] == b"\x28\xb5\x2f\xfd":
    import subprocess
    raw = subprocess.run(["zstd", "-dc"], input=raw, capture_output=True, check=True).stdout
ns = {"c": "http://linux.duke.edu/metadata/common"}
for p in ET.fromstring(raw).findall("c:package", ns):
    c = p.find("c:checksum", ns)
    assert c.get("type") == "sha256", "package checksum is not sha256"
    href = p.find("c:location", ns).get("href")
    assert "/" not in href and href.endswith(".rpm"), href
    print(c.text, href)
PY
  n=0
  while read -r psum name; do
    n=$((n + 1))
    if [[ -f "$in/$name" && "$(sha256sum "$in/$name" | cut -d' ' -f1)" == "$psum" ]]; then
      :
    else
      if [[ -f "$in/$name" ]]; then
        log "WARNING: built $name has the same name as a published package; the published one is kept (bump the release if its content changed)"
        replaced=$((replaced + 1))
      fi
      fetch "$url/$name" "$work/pkg.rpm" || die "$url/$name listed but missing"
      [[ "$(sha256sum "$work/pkg.rpm" | cut -d' ' -f1)" == "$psum" ]] || die "$url/$name: checksum mismatch"
      mv -f "$work/pkg.rpm" "$in/$name"
    fi
    printf '%s  %s\n' "$psum" "$name" >>"$work/published"
    kept=$((kept + 1))
  done <"$work/list"
  log "$url: $n published packages"
done

[[ $kept -gt 0 ]] || { log "nothing published in $OB_REPO yet; $in unchanged"; exit 0; }
sort -k2 "$work/published" >"$in/PUBLISHED"
(cd "$in" && find . -maxdepth 1 -type f -name '*.rpm' -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
[[ -f "$in/BUILD-INFO.txt" ]] &&
  printf 'published set:  %s published RPMs of %s/%s/%s kept byte-identical (%s built ones replaced by the published file)\n' \
    "$kept" "$OB_PUBLIC_URL" "$OB_REPO" "$FEDORA_RELEASE" "$replaced" >>"$in/BUILD-INFO.txt"
log "merged $kept published RPMs into $in ($replaced built ones replaced by the published file); PUBLISHED lists them"
