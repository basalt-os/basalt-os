#!/usr/bin/env bash
# Build the basalt-ledger RPMs (binary, -selinux, source) in a Fedora
# container and copy them next to the other packages ($BUILD_DIR/rpms/<fedora>/),
# where `make repo` signs and publishes them.
#
#   packages/basalt-ledger/build.sh          build
#   packages/basalt-ledger/build.sh test     go vet + go test in the container only
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-ledger"
ver="$(awk '/^Version:/ {print $2}' "$pkg/basalt-ledger.spec")"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"
tar -C "$pkg" --owner=0 --group=0 --sort=name --exclude=./bin --exclude='*.pp' --exclude='*.pp.bz2' \
  --exclude=./selinux/tmp -czf "$work/SOURCES/basalt-ledger-$ver.tar.gz" .
cp -p "$pkg/basalt-ledger.spec" "$work/SPECS/"

mode="${1:-build}"
log "basalt-ledger $ver: $mode in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e MODE="$mode" -e VER="$ver" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros golang gcc selinux-policy-devel make bzip2 >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  if [ "$MODE" = test ]; then
    mkdir -p /src && tar -C /src -xzf /rpmbuild/SOURCES/basalt-ledger-$VER.tar.gz && cd /src
    GOFLAGS=-mod=mod GOTOOLCHAIN=local GOPROXY=off go vet ./... && GOFLAGS=-mod=mod GOTOOLCHAIN=local GOPROXY=off go test ./...
    exit
  fi
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/basalt-ledger.spec >/rpmbuild/build.log 2>&1 ||
    { tail -60 /rpmbuild/build.log; exit 1; }
  grep -E "^ok|^---|FAIL" /rpmbuild/build.log || true
'
[[ "$mode" == test ]] && exit 0
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-ledger*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "basalt-ledger*.rpm" -printf "%P  %s bytes\n" | sort
