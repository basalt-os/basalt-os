#!/usr/bin/env bash
# Build a data package (basalt-knowledge, basalt-vsm-planner) whose payload
# is not kept in this repository: every file listed in the package's
# sources.manifest is fetched by its path and must match the SHA-256 and
# size listed there, then the RPM is built in a Fedora container.
#
#   scripts/data-package.sh basalt-knowledge
#   scripts/data-package.sh basalt-vsm-planner
#
# Where the files come from (the first one set):
#   BASALT_ARTIFACTS_DIR  a local directory with the same layout (a mirror)
#   BASALT_ARTIFACTS_URL  the published artifacts location (https only)
# BASALT_SOURCES_MANIFEST replaces the package's sources.manifest (lab
# builds: knowledge signed with a lab key, see scripts/sign-knowledge.sh).
# The public location is not published yet: the default below is a
# placeholder, so a build without one of these fails at the download.
source "$(dirname "$0")/lib.sh"
: "${BASALT_ARTIFACTS_URL:=https://obpkg.org/artifacts}"
: "${BASALT_ARTIFACTS_DIR:=}"

name="${1:-}"
[[ "$name" == basalt-knowledge || "$name" == basalt-vsm-planner ]] || die "usage: $0 basalt-knowledge|basalt-vsm-planner"
pkg="$REPO_ROOT/packages/$name"
manifest="${BASALT_SOURCES_MANIFEST:-$pkg/sources.manifest}"
[[ -f "$manifest" ]] || die "$manifest missing"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"

# Columns: name sha256 bytes path. Comments and blank lines are skipped.
while read -r file sha bytes path; do
  [[ -z "$file" || "$file" == \#* ]] && continue
  [[ "$file" =~ ^[A-Za-z0-9._-]+$ && "$path" =~ ^[A-Za-z0-9._/+-]+$ && "$path" != *..* ]] || die "bad manifest line for $file"
  dest="$work/SOURCES/$file"
  if [[ -n "$BASALT_ARTIFACTS_DIR" ]]; then
    cp -p "$BASALT_ARTIFACTS_DIR/$path" "$dest" || die "$path not in $BASALT_ARTIFACTS_DIR"
  else
    [[ "$BASALT_ARTIFACTS_URL" == https://* ]] || die "BASALT_ARTIFACTS_URL must be https"
    curl -fsSL --proto '=https' -o "$dest" "$BASALT_ARTIFACTS_URL/$path" || die "download of $path failed"
  fi
  echo "$sha  $dest" | sha256sum -c --quiet - || die "$file: checksum mismatch"
  [[ "$(stat -c %s "$dest")" == "$bytes" ]] || die "$file: size mismatch"
  log "$name: $file verified ($bytes bytes)"
done <"$manifest"

cp -p "$manifest" "$work/SOURCES/sources.manifest"
cp -p "$pkg/LICENSE" "$work/SOURCES/"
# Files kept in the repository next to the spec (the release key).
for f in "$pkg"/*.asc; do [[ -e "$f" ]] && cp -p "$f" "$work/SOURCES/"; done
cp -p "$pkg/$name.spec" "$work/SPECS/"
log "$name: build in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/'"$name"'.spec >/rpmbuild/build.log 2>&1 ||
    { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "$name-*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "$name-*.rpm" -printf "%P  %s bytes\n" | sort
