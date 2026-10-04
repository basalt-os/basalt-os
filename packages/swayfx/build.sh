#!/usr/bin/env bash
# Build swayfx in a Fedora container: SwayFX from its release archive,
# pinned by version and SHA-256 (the version is the spec's), against
# Fedora's wlroots and scenefx. Not part of `make rpms` or CI (a C build
# with its own dependencies); scripts/release/build.sh includes it.
#
#   packages/swayfx/build.sh
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/swayfx"
SWAYFX_VERSION=0.6
SWAYFX_SHA256=854f9d1468b8706718210e026d0bb0ddbc8370f750345fbbdd163f130c1b922d
[[ "$(awk '/^%global tag/ {print $3}' "$pkg/swayfx.spec")" == "$SWAYFX_VERSION" ]] ||
  die "SwayFX version in build.sh and swayfx.spec differ"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"
tarball="$work/SOURCES/swayfx-$SWAYFX_VERSION.tar.gz"
curl -fsSL --proto '=https' -o "$tarball" "https://github.com/WillPower3309/swayfx/archive/refs/tags/$SWAYFX_VERSION/swayfx-$SWAYFX_VERSION.tar.gz" ||
  die "download of SwayFX $SWAYFX_VERSION failed"
echo "$SWAYFX_SHA256  $tarball" | sha256sum -c --quiet - || die "SwayFX archive checksum mismatch"
cp -p "$pkg/swayfx.spec" "$work/SPECS/"

log "swayfx: build in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build dnf5-plugins >/dev/null 2>&1 && dnf -q -y builddep /rpmbuild/SPECS/swayfx.spec >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/swayfx.spec >/rpmbuild/build.log 2>&1 ||
    { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "swayfx-*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "swayfx-*.rpm" -printf "%P  %s bytes\n" | sort
