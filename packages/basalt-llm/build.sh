#!/usr/bin/env bash
# Build basalt-llm (+ -selinux) in a Fedora container: llama.cpp from its
# release archive, pinned by version and SHA-256 (the same pin as the
# spec), and the files in this directory. Not part of `make rpms` or CI:
# compiling llama.cpp takes several minutes.
#
#   packages/basalt-llm/build.sh
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-llm"
LLAMA_VERSION=0.5.0
LLAMA_SHA256=fef9ed754f4e031fb5c663c29260feda4ebc241abb68d64a81c0f1df5f1748e2
[[ "$(awk '/^%global llama_version/ {print $3}' "$pkg/basalt-llm.spec")" == "$LLAMA_VERSION" ]] ||
  die "llama.cpp version in build.sh and basalt-llm.spec differ"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"
tarball="$work/SOURCES/llama.cpp-$LLAMA_VERSION.tar.gz"
curl -fsSL --proto '=https' -o "$tarball" "https://github.com/ggml-org/llama.cpp/archive/refs/tags/v$LLAMA_VERSION.tar.gz" ||
  die "download of llama.cpp $LLAMA_VERSION failed"
echo "$LLAMA_SHA256  $tarball" | sha256sum -c --quiet - || die "llama.cpp archive checksum mismatch"
cp -p "$pkg"/basalt-llm.service "$pkg"/basalt-llm-start "$pkg"/basalt-llm-fetch "$pkg"/models.manifest \
  "$pkg"/llm.conf "$pkg"/LICENSE "$pkg"/selinux/basalt_llm.* "$work/SOURCES/"
cp -p "$pkg/basalt-llm.spec" "$work/SPECS/"

log "basalt-llm: build in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build dnf5-plugins >/dev/null 2>&1 && dnf -q -y builddep /rpmbuild/SPECS/basalt-llm.spec >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/basalt-llm.spec >/rpmbuild/build.log 2>&1 ||
    { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-llm*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "basalt-llm*.rpm" -printf "%P  %s bytes\n" | sort
