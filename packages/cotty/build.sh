#!/usr/bin/env bash
# Build the cotty RPMs (cotty and cotty-guard) in a Fedora container from
# the commit pinned in packages/cotty/source.conf, and copy them to OUT_DIR
# (default $RPM_DIR/testing: cotty ships in basalt-testing while it is
# pre-release, never in the basalt set).
#
#   packages/cotty/build.sh [OUT_DIR]
#
# COTTY_SRC=DIR|BUNDLE builds offline from a local clone or a git bundle
# instead of fetching (the pinned commit must be there; COTTY_COMMIT_OVERRIDE
# changes the pin). Neither is allowed in a release build. The Go modules
# are vendored in the container (go mod vendor, checked against go.sum) into
# a second source archive, so rpmbuild runs without network.
#
# OUT_DIR receives the binary RPMs and the source RPM, as for the other
# testing packages.
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/cotty"
out="${1:-$RPM_DIR/testing}"
# shellcheck source=packages/cotty/source.sh
source "$pkg/source.sh"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SPECS"
cotty_fetch "$work"
cp -p "$pkg/cotty.spec" "$work/SPECS/"
cp -p "$pkg/third-party-licenses.sh" "$work/SOURCES/"

log "cotty $COTTY_VERSION (commit ${COTTY_COMMIT:0:12}) in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e VER="$COTTY_VERSION" -e EPOCH="$COTTY_EPOCH" -e COMMIT="$COTTY_COMMIT" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros golang tar gzip >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  # Vendor the modules go.sum pins (the only network use), as a reproducible
  # archive: sorted names, fixed owner and times.
  mkdir -p /tmp/v
  tar -xzf "/rpmbuild/SOURCES/cotty-$VER.tar.gz" -C /tmp/v
  (cd "/tmp/v/cotty-$VER" && GOTOOLCHAIN=local go mod vendor && GOTOOLCHAIN=local go mod verify >/dev/null)
  tar -C /tmp/v --sort=name --mtime="@$EPOCH" --owner=0 --group=0 --numeric-owner \
    -czf "/rpmbuild/SOURCES/cotty-$VER-vendor.tar.gz" "cotty-$VER/vendor"
  export SOURCE_DATE_EPOCH="$EPOCH"
  rpmbuild --define "_topdir /rpmbuild" --define "cotty_version $VER" --define "cotty_commit $COMMIT" \
    --define "use_source_date_epoch_as_buildtime 1" --define "clamp_mtime_to_source_date_epoch 1" \
    -ba /rpmbuild/SPECS/cotty.spec >/rpmbuild/build.log 2>&1 || { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$out"
sudo find "$work/RPMS" "$work/SRPMS" -name "cotty*.rpm" -exec cp {} "$out/" \;
sudo chown -R "$(id -u):$(id -g)" "$out"
find "$out" -name "cotty*.rpm" -printf "%P  %s bytes\n" | sort
