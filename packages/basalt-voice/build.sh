#!/usr/bin/env bash
# Build basalt-voice in a Fedora container: whisper.cpp from its release
# archive, pinned by version and SHA-256 (the same pin as the spec), and
# the files in this directory. Not part of `make rpms` or CI: compiling
# whisper.cpp takes a few minutes.
#
#   packages/basalt-voice/build.sh
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-voice"
WHISPER_VERSION=1.9.4
WHISPER_SHA256=57e280cee375ab02425b806ad5146b99f6eb9357e3c2b31357c8a6af2e2e44ae
[[ "$(awk '/^%global whisper_version/ {print $3}' "$pkg/basalt-voice.spec")" == "$WHISPER_VERSION" ]] ||
  die "whisper.cpp version in build.sh and basalt-voice.spec differ"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"
tarball="$work/SOURCES/whisper.cpp-$WHISPER_VERSION.tar.gz"
curl -fsSL --proto '=https' -o "$tarball" "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v$WHISPER_VERSION.tar.gz" ||
  die "download of whisper.cpp $WHISPER_VERSION failed"
echo "$WHISPER_SHA256  $tarball" | sha256sum -c --quiet - || die "whisper.cpp archive checksum mismatch"
cp -p "$pkg"/basalt-voice-fetch "$pkg"/models.manifest "$pkg"/LICENSE "$work/SOURCES/"
cp -p "$pkg/basalt-voice.spec" "$work/SPECS/"

log "basalt-voice: build in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build dnf5-plugins >/dev/null 2>&1 && dnf -q -y builddep /rpmbuild/SPECS/basalt-voice.spec >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/basalt-voice.spec >/rpmbuild/build.log 2>&1 ||
    { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-voice*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "basalt-voice*.rpm" -printf "%P  %s bytes\n" | sort
