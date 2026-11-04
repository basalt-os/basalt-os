#!/usr/bin/env bash
# Build the cotty .deb packages (cotty and cotty-guard, amd64 and arm64)
# from the commit pinned in packages/cotty/source.conf, and copy them to
# OUT_DIR (default $BUILD_DIR/debs/testing). One static build (CGO off)
# serves Debian 13, Ubuntu 26.04 and Ubuntu 24.04, as for the other packages
# of https://obpkg.org/apt.
#
#   packages/cotty/build-deb.sh [OUT_DIR] [ARCH...]     ARCH: amd64 arm64 (default both)
#
# COTTY_SRC and COTTY_COMMIT_OVERRIDE as for packages/cotty/build.sh.
# Everything runs in a Fedora container: the Go modules (go.sum checked),
# the binaries, and the packages with nfpm (NFPM_VERSION, the version the
# Samba Conductor packages use). The file names are the canonical
# <Package>_<Version>_<Architecture>.deb that scripts/release/sign-apt.sh
# expects. SOURCE_DATE_EPOCH is the commit time, so a rebuild of the same
# commit gives the same bytes.
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/cotty"
out="${1:-$BUILD_DIR/debs/testing}"
shift || true
arches=("$@")
[[ ${#arches[@]} -gt 0 ]] || arches=(amd64 arm64)
for a in "${arches[@]}"; do
  case "$a" in amd64 | arm64) ;; *) die "unsupported architecture $a" ;; esac
done
NFPM_VERSION="v2.47.0"
# shellcheck source=packages/cotty/source.sh
source "$pkg/source.sh"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
cotty_fetch "$work"
mkdir -p "$work/deb" "$work/dist"
cp -p "$pkg"/deb/* "$pkg/third-party-licenses.sh" "$work/deb/"

log "cotty $COTTY_VERSION .deb (commit ${COTTY_COMMIT:0:12}; ${arches[*]}) in $FEDORA_IMAGE"
in_fedora -v "$work:/w" -e VER="$COTTY_VERSION" -e EPOCH="$COTTY_EPOCH" -e ARCHES="${arches[*]}" \
  -e NFPM_VERSION="$NFPM_VERSION" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install golang tar gzip >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  export SOURCE_DATE_EPOCH="$EPOCH" GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux
  mkdir -p /tmp/src /tmp/tools
  tar -xzf "/w/SOURCES/cotty-$VER.tar.gz" -C /tmp/src
  src="/tmp/src/cotty-$VER"
  (cd "$src" && go mod verify >/dev/null && go mod vendor)
  # nfpm for the build host, at the pinned version.
  (cd / && GOBIN=/tmp/tools GOFLAGS= go install "github.com/goreleaser/nfpm/v2/cmd/nfpm@$NFPM_VERSION") >/dev/null 2>&1 ||
    { echo "cannot build nfpm $NFPM_VERSION"; exit 1; }
  changes="$(LC_ALL=C date -u -d "@$EPOCH" -R)"
  for arch in $ARCHES; do
    build="/tmp/build/$arch"
    mkdir -p "$build/bin" "$build/doc"
    for bin in cotty cotty-guard; do
      (cd "$src" && GOARCH="$arch" GOFLAGS=-mod=vendor go build -trimpath -ldflags "-s -w -buildid=" -o "$build/bin/$bin" "./cmd/$bin")
    done
    printf "cotty (%s-1) testing; urgency=medium\n\n  * Snapshot of cotty at commit %s for the testing suite.\n\n -- Basalt OS project <noreply@basalt-os.org>  %s\n" \
      "$VER" "${VER##*.}" "$changes" | gzip -9n >"$build/doc/changelog.Debian.gz"
    bash /w/deb/third-party-licenses.sh "$src/vendor" | gzip -9n >"$build/doc/THIRD-PARTY-LICENSES.gz"
    for p in cotty cotty-guard; do
      (cd /w/deb && ARCH="$arch" PKG_VERSION="$VER" BUILD="$build" SRC="$src" \
        /tmp/tools/nfpm package --config "$p.yaml" --packager deb --target /w/dist/ >/dev/null)
      test -f "/w/dist/${p}_${VER}-1_${arch}.deb" || { echo "nfpm did not write ${p}_${VER}-1_${arch}.deb"; exit 1; }
    done
  done
'
mkdir -p "$out"
sudo find "$work/dist" -name "cotty*.deb" -exec cp {} "$out/" \;
sudo chown -R "$(id -u):$(id -g)" "$out"
find "$out" -name "cotty*.deb" -printf "%P  %s bytes\n" | sort
