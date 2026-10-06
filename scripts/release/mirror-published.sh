#!/usr/bin/env bash
# Mirror a published repository from https://obpkg.org, byte-identical and
# verified, for image builds that must contain only packages signed with the
# OpenBasalt release key (packages/basalt-installer/live/build-live.sh with
# LIVE_SOURCE=obpkg).
#
#   OB_REPO=basalt scripts/release/mirror-published.sh OUT_DIR
#
# OUT_DIR gets <releasever>/<arch>/ exactly as served (repodata/ with
# repomd.xml and repomd.xml.asc, every RPM) and RPM-GPG-KEY-basalt, the
# release key, which must equal https://obpkg.org/keys/openbasalt-release-key.asc.
# Checks: repomd.xml carries a valid signature of the packages subkey
# (OB_SIGNING_SUBKEY) made with the release key; every metadata file
# matches the checksum in that signed repomd.xml; every RPM matches the
# checksum in the signed primary metadata. Anything else stops the mirror.
# An existing OUT_DIR/<releasever>/<arch> is replaced. dnf then checks the
# signatures again when it installs from the mirror (gpgcheck=1,
# repo_gpgcheck=1).
source "$(dirname "$0")/../lib.sh"

: "${OB_REPO:=basalt}"
case "$OB_REPO" in basalt | basalt-tools | basalt-testing | basalt-nonfree | basalt-nonfree-testing) ;; *) die "OB_REPO must be basalt, basalt-tools, basalt-testing, basalt-nonfree or basalt-nonfree-testing" ;; esac
: "${OB_PUBLIC_URL:=https://obpkg.org}"
: "${OB_SIGNING_SUBKEY:=302461D26520E077D07FFCA9AA27C62C36CCFC4B}"
: "${OB_RELEASE_PUBKEY:=$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt}"
: "${OB_PUBKEY_URL:=$OB_PUBLIC_URL/keys/openbasalt-release-key.asc}"
out="${1:?usage: OB_REPO=basalt $0 OUT_DIR}"
for c in curl gpg python3 zstd; do command -v "$c" >/dev/null || die "$c not installed"; done
mkdir -p "$out"
out="$(cd "$out" && pwd)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export GNUPGHOME="$work/gnupg"
mkdir -m 700 "$GNUPGHOME"
curl -fsSL --proto '=https' -o "$work/published-key.asc" "$OB_PUBKEY_URL" || die "cannot fetch $OB_PUBKEY_URL"
cmp -s "$work/published-key.asc" "$OB_RELEASE_PUBKEY" || die "$OB_RELEASE_PUBKEY differs from $OB_PUBKEY_URL"
gpg --batch --quiet --import "$OB_RELEASE_PUBKEY" 2>/dev/null || die "cannot import $OB_RELEASE_PUBKEY"

fetch() {
  local code
  code="$(curl -sSL --proto '=https' -H 'Cache-Control: no-cache' -o "$2" -w '%{http_code}' "$1")" || die "cannot fetch $1"
  [[ "$code" == 200 ]] || die "$1: HTTP $code"
}

url="$OB_PUBLIC_URL/$OB_REPO/$FEDORA_RELEASE/$ARCH"
tmp="$work/tree"
mkdir -p "$tmp/repodata"
fetch "$url/repodata/repomd.xml?v=$(date +%s)" "$tmp/repodata/repomd.xml"
fetch "$url/repodata/repomd.xml.asc?v=$(date +%s)" "$tmp/repodata/repomd.xml.asc"
gpg --batch --status-fd 1 --verify "$tmp/repodata/repomd.xml.asc" "$tmp/repodata/repomd.xml" 2>/dev/null |
  grep -q "^\[GNUPG:\] VALIDSIG ${OB_SIGNING_SUBKEY^^} " || die "$url/repodata/repomd.xml: bad signature"

# Every metadata file named by the signed repomd.xml, with its checksum.
python3 - "$tmp/repodata/repomd.xml" >"$work/meta" <<'PY' || die "cannot read $url/repodata/repomd.xml"
import sys, xml.etree.ElementTree as ET
ns = {"r": "http://linux.duke.edu/metadata/repo"}
for d in ET.parse(sys.argv[1]).findall("r:data", ns):
    c = d.find("r:checksum", ns)
    assert c.get("type") == "sha256", "metadata checksum is not sha256"
    href = d.find("r:location", ns).get("href")
    assert href.startswith("repodata/") and "/" not in href[len("repodata/"):], href
    print(c.text, href, d.get("type"))
PY
primary=""
while read -r sum href type; do
  fetch "$url/$href" "$tmp/$href"
  [[ "$(sha256sum "$tmp/$href" | cut -d' ' -f1)" == "$sum" ]] || die "$url/$href: checksum mismatch"
  [[ "$type" == primary ]] && primary="$tmp/$href"
done <"$work/meta"
[[ -n "$primary" ]] || die "$url: no primary metadata"

python3 - "$primary" >"$work/list" <<'PY' || die "cannot read the primary metadata of $url"
import gzip, lzma, subprocess, sys, xml.etree.ElementTree as ET
raw = open(sys.argv[1], "rb").read()
if raw[:2] == b"\x1f\x8b":
    raw = gzip.decompress(raw)
elif raw[:6] == b"\xfd7zXZ\x00":
    raw = lzma.decompress(raw)
elif raw[:4] == b"\x28\xb5\x2f\xfd":
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
while read -r sum name; do
  # Reuse an identical file from a previous mirror.
  if [[ -f "$out/$FEDORA_RELEASE/$ARCH/$name" && "$(sha256sum "$out/$FEDORA_RELEASE/$ARCH/$name" | cut -d' ' -f1)" == "$sum" ]]; then
    cp -p "$out/$FEDORA_RELEASE/$ARCH/$name" "$tmp/$name"
  else
    fetch "$url/$name" "$tmp/$name"
    [[ "$(sha256sum "$tmp/$name" | cut -d' ' -f1)" == "$sum" ]] || die "$url/$name: checksum mismatch"
  fi
  n=$((n + 1))
done <"$work/list"

rm -rf "${out:?}/${FEDORA_RELEASE:?}/${ARCH:?}"
mkdir -p "$out/$FEDORA_RELEASE"
mv "$tmp" "$out/$FEDORA_RELEASE/$ARCH"
cp "$OB_RELEASE_PUBKEY" "$out/RPM-GPG-KEY-basalt"
log "mirrored $url: $n packages, metadata signed by subkey ${OB_SIGNING_SUBKEY^^}, in $out/$FEDORA_RELEASE/$ARCH"
