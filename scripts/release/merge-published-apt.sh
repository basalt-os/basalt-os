#!/usr/bin/env bash
# Start an APT publish from the whole published package set: download every
# .deb of the suite as served by https://obpkg.org/apt into the input of
# scripts/release/sign-apt.sh, so the new metadata keeps the older versions.
#
#   OB_APT_SUITE=stable scripts/release/merge-published-apt.sh IN_DIR
#
# IN_DIR is a directory of unsigned .deb files with SHA256SUMS (the input of
# sign-apt.sh). For dists/<suite>:
# - InRelease must carry a valid signature of the packages subkey
#   (OB_SIGNING_SUBKEY) made with the release key (OB_RELEASE_PUBKEY); the
#   Packages index of every architecture is checked against the SHA-256 of
#   that signed Release text, and every .deb against its index;
# - every published .deb lands in IN_DIR byte-identical. A built .deb with
#   the same file name is replaced by the published one: a published file is
#   never replaced (upload.sh), so a package whose content changed needs a
#   new version (the script warns);
# - IN_DIR/PUBLISHED lists the published packages (sha256sum format) and
#   IN_DIR/POOL-PATHS where each is served; sign-apt.sh refuses a tree that
#   would move one.
# SHA256SUMS is rewritten over all the .deb files. A suite that is not
# published yet (InRelease answers 404) adds nothing.
source "$(dirname "$0")/../lib.sh"

: "${OB_APT_SUITE:=stable}"
case "$OB_APT_SUITE" in stable | testing) ;; *) die "OB_APT_SUITE must be stable or testing" ;; esac
: "${OB_APT_ARCHES:=amd64 arm64}"
: "${OB_PUBLIC_URL:=https://obpkg.org}"
: "${OB_SIGNING_SUBKEY:=302461D26520E077D07FFCA9AA27C62C36CCFC4B}"
: "${OB_RELEASE_PUBKEY:=$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt}"
in="${1:?usage: OB_APT_SUITE=stable $0 IN_DIR}"
in="$(cd "$in" && pwd)" || die "no $in"
[[ -f "$in/SHA256SUMS" ]] || die "$in/SHA256SUMS missing"
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
  code="$(curl -sSL --proto '=https,http' -H 'Cache-Control: no-cache' -o "$2" -w '%{http_code}' "$1")" || die "cannot fetch $1"
  case "$code" in 200) return 0 ;; 404) return 1 ;; *) die "$1: HTTP $code" ;; esac
}
# Path to URL: "+" and "~" in versions are percent-encoded, as apt does.
urlpath() { local p="$1"; p="${p//+/%2B}"; p="${p//\~/%7E}"; printf '%s' "$p"; }

url="$OB_PUBLIC_URL/apt"
if ! fetch "$url/dists/$OB_APT_SUITE/InRelease?v=$(date +%s)" "$work/InRelease"; then
  log "$url/dists/$OB_APT_SUITE: not published, nothing to merge"
  exit 0
fi
gpg --batch --status-fd 1 --output "$work/Release" --decrypt "$work/InRelease" 2>/dev/null |
  grep -q "^\[GNUPG:\] VALIDSIG ${OB_SIGNING_SUBKEY^^} " || die "$url/dists/$OB_APT_SUITE/InRelease: bad signature"
grep -qx "Suite: $OB_APT_SUITE" "$work/Release" || die "$url/dists/$OB_APT_SUITE/InRelease: not suite $OB_APT_SUITE"

: >"$work/published"
: >"$work/paths"
kept=0 replaced=0
for a in $OB_APT_ARCHES; do
  rel="main/binary-$a/Packages.xz"
  sum="$(awk -v p="$rel" '/^SHA256:/ {s=1; next} /^[^ ]/ {s=0} s && $3 == p {print $1}' "$work/Release")"
  [[ -n "$sum" ]] || { log "$OB_APT_SUITE/$rel: not in Release, skipped"; continue; }
  # The index by its hash (Acquire-By-Hash), as apt fetches it.
  fetch "$url/dists/$OB_APT_SUITE/main/binary-$a/by-hash/SHA256/$sum" "$work/Packages.xz" ||
    die "$url/dists/$OB_APT_SUITE/main/binary-$a/by-hash/SHA256/$sum missing"
  [[ "$(sha256sum "$work/Packages.xz" | cut -d' ' -f1)" == "$sum" ]] || die "$rel: checksum mismatch"
  python3 - "$work/Packages.xz" >"$work/list" <<'PY' || die "cannot read $rel"
import lzma, sys
text = lzma.decompress(open(sys.argv[1], "rb").read()).decode()
for stanza in text.split("\n\n"):
    fields = {}
    for line in stanza.splitlines():
        if line and not line[0].isspace() and ":" in line:
            k, v = line.split(":", 1)
            fields[k] = v.strip()
    if not fields:
        continue
    f, s = fields["Filename"], fields["SHA256"]
    assert f.startswith("pool/main/") and f.endswith(".deb") and ".." not in f, f
    assert len(s) == 64, s
    print(s, f)
PY
  while read -r psum path; do
    name="${path##*/}"
    # A package of architecture all is listed in each index: once is enough.
    awk -v n="$name" '$2 == n {f=1} END {exit !f}' "$work/published" && continue
    if [[ -f "$in/$name" && "$(sha256sum "$in/$name" | cut -d' ' -f1)" == "$psum" ]]; then
      :
    else
      if [[ -f "$in/$name" ]]; then
        log "WARNING: built $name has the same name as a published package; the published one is kept (bump the version if its content changed)"
        replaced=$((replaced + 1))
      fi
      fetch "$url/$(urlpath "$path")" "$work/pkg.deb" || die "$url/$path listed but missing"
      [[ "$(sha256sum "$work/pkg.deb" | cut -d' ' -f1)" == "$psum" ]] || die "$url/$path: checksum mismatch"
      mv -f "$work/pkg.deb" "$in/$name"
    fi
    printf '%s  %s\n' "$psum" "$name" >>"$work/published"
    printf '%s %s\n' "$name" "$path" >>"$work/paths"
    kept=$((kept + 1))
  done <"$work/list"
  log "$url/dists/$OB_APT_SUITE/main/binary-$a: $(wc -l <"$work/list") published packages"
done

[[ $kept -gt 0 ]] || { log "nothing published in $OB_APT_SUITE yet; $in unchanged"; exit 0; }
LC_ALL=C sort -k2 "$work/published" >"$in/PUBLISHED"
LC_ALL=C sort "$work/paths" >"$in/POOL-PATHS"
(cd "$in" && find . -maxdepth 1 -type f -name '*.deb' -printf '%P\n' | LC_ALL=C sort | xargs -d '\n' sha256sum >SHA256SUMS)
[[ -f "$in/BUILD-INFO.txt" ]] &&
  printf 'published set:  %s published .debs of %s/dists/%s kept byte-identical (%s built ones replaced by the published file)\n' \
    "$kept" "$url" "$OB_APT_SUITE" "$replaced" >>"$in/BUILD-INFO.txt"
log "merged $kept published .debs into $in ($replaced built ones replaced by the published file); PUBLISHED lists them"
