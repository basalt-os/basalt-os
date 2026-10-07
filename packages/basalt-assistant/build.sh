#!/usr/bin/env bash
# Build the basalt-assistant RPMs (binary, -selinux, source) in a Fedora
# container and copy them next to the other packages ($BUILD_DIR/rpms/<fedora>/),
# where `make repo` signs and publishes them.
#
#   packages/basalt-assistant/build.sh          build
#   packages/basalt-assistant/build.sh test     go vet + go test in the container only (plus eval/cases checks)
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-assistant"
ver="$(awk '/^Version:/ {print $2}' "$pkg/basalt-assistant.spec")"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS"
tar -C "$pkg" --owner=0 --group=0 --sort=name --exclude=./bin --exclude='*.pp' --exclude='*.pp.bz2' \
  --exclude=./selinux/tmp -czf "$work/SOURCES/basalt-assistant-$ver.tar.gz" .
cp -p "$pkg/basalt-assistant.spec" "$work/SPECS/"

mode="${1:-build}"
log "basalt-assistant $ver: $mode in $FEDORA_IMAGE"
# The test mode also mounts the shared evaluation cases, so the check that
# every expected action passes the action validators runs with the tests.
in_fedora -v "$work:/rpmbuild" -v "$REPO_ROOT/eval/cases:/eval-cases:ro" -e BASALT_EVAL_CASES=/eval-cases \
  -e MODE="$mode" -e VER="$ver" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros golang gcc selinux-policy-devel make bzip2 gettext >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  if [ "$MODE" = test ]; then
    mkdir -p /src && tar -C /src -xzf /rpmbuild/SOURCES/basalt-assistant-$VER.tar.gz && cd /src
    GOFLAGS=-mod=mod GOTOOLCHAIN=local GOPROXY=off go vet ./... && GOFLAGS=-mod=mod GOTOOLCHAIN=local GOPROXY=off go test ./...
    exit
  fi
  rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/basalt-assistant.spec >/rpmbuild/build.log 2>&1 || exit 1
  grep -E "^ok|^---|FAIL" /rpmbuild/build.log || true
' || rpmbuild_failed basalt-assistant "$work/build.log"
[[ "$mode" == test ]] && exit 0
mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-assistant*.rpm" -exec cp {} "$RPM_DIR/" \;
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "basalt-assistant*.rpm" -printf "%P  %s bytes\n" | sort
