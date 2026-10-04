#!/usr/bin/env bash
# Build the basalt-installer RPMs (binary, -gui, source) in a Fedora
# container and copy them next to the other packages ($BUILD_DIR/rpms/<fedora>/),
# where `make repo` signs and publishes them.
#
#   packages/basalt-installer/build.sh          build
#   packages/basalt-installer/build.sh test     go vet + go test in the container only
#
# Go toolchain: tui-kit needs Go 1.27 and Fedora 44 ships Go 1.26, so the
# container uses the official Go release named by go.mod's toolchain line,
# downloaded from go.dev and checked against the SHA-256 pinned below
# (cached in $BUILD_DIR/cache). The Go modules are fetched by `go mod
# vendor` (verified against go.sum and the checksum database) into a vendor
# tarball that becomes the RPM's second source; rpmbuild itself runs with
# GOPROXY=off. Nothing is vendored into the repository.
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-installer"
ver="$(awk '/^Version:/ {print $2}' "$pkg/basalt-installer.spec")"

GO_VERSION=1.27.1
GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
grep -q "^toolchain go$GO_VERSION\$" "$pkg/go.mod" || die "go.mod's toolchain line and GO_VERSION ($GO_VERSION) disagree"

cache="$BUILD_DIR/cache"
mkdir -p "$cache"
tarball="$cache/go$GO_VERSION.linux-amd64.tar.gz"
if ! echo "$GO_SHA256  $tarball" | sha256sum -c --quiet - 2>/dev/null; then
  log "downloading Go $GO_VERSION"
  curl -fsSL -o "$tarball.part" "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
  echo "$GO_SHA256  $tarball.part" | sha256sum -c --quiet - || die "Go $GO_VERSION checksum mismatch"
  mv "$tarball.part" "$tarball"
fi

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"
tar -C "$pkg" --owner=0 --group=0 --sort=name --exclude=./vendor --exclude=./bin --exclude=./live/build \
  -czf "$work/SOURCES/basalt-installer-$ver.tar.gz" .
cp -p "$pkg/basalt-installer.spec" "$work/SPECS/"

mode="${1:-build}"
log "basalt-installer $ver: $mode in $FEDORA_IMAGE (Go $GO_VERSION)"
in_fedora -v "$work:/rpmbuild" -v "$tarball:/go.tar.gz:ro" -e MODE="$mode" -e VER="$ver" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build tar gzip gcc >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  tar -C /opt -xzf /go.tar.gz
  export PATH=/opt/go/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=mod GOPATH=/tmp/gopath GOCACHE=/tmp/gocache
  mkdir -p /src && tar -C /src -xzf /rpmbuild/SOURCES/basalt-installer-$VER.tar.gz && cd /src
  go mod verify >/dev/null
  go mod vendor
  if [ "$MODE" = test ]; then
    GOFLAGS=-mod=vendor GOPROXY=off go vet ./... && GOFLAGS=-mod=vendor GOPROXY=off go test ./...
    exit
  fi
  tar --owner=0 --group=0 --sort=name -czf /rpmbuild/SOURCES/basalt-installer-vendor-$VER.tar.gz vendor
  unset GOFLAGS
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/basalt-installer.spec >/rpmbuild/build.log 2>&1 ||
    { tail -60 /rpmbuild/build.log; exit 1; }
  grep -E "^ok|^---|FAIL" /rpmbuild/build.log || true
'
[[ "$mode" == test ]] && exit 0
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-installer*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "basalt-installer*.rpm" -printf "%P  %s bytes\n" | sort
