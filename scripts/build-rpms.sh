#!/usr/bin/env bash
# Build the Basalt OS RPMs in a Fedora container.
#
#   scripts/build-rpms.sh            basalt-release, basalt-logos, basalt-snapshots
#   scripts/build-rpms.sh --lab      also the lab canary package (versions 1, 2, 3)
#
# Output: $BUILD_DIR/rpms/<fedora>/ (binary and source RPMs); lab fixtures in
# $BUILD_DIR/rpms/<fedora>/lab/. FEDORA_RELEASE selects the base (default 44).
# BASALT_GPG_PUBKEY, when set, is the repository key shipped in basalt-release
# (otherwise the placeholder in packages/basalt-release is kept).
source "$(dirname "$0")/lib.sh"

lab=0
[[ "${1:-}" == --lab ]] && lab=1

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$work/lab"

# Sources: every file next to each spec.
for pkg in basalt-release basalt-snapshots; do
  cp -p "$REPO_ROOT/packages/$pkg/"* "$work/SOURCES/"
  mv "$work/SOURCES/$pkg.spec" "$work/SPECS/"
done
if [[ -n "$BASALT_GPG_PUBKEY" ]]; then
  [[ -f "$BASALT_GPG_PUBKEY" ]] || die "BASALT_GPG_PUBKEY=$BASALT_GPG_PUBKEY not found"
  grep -q 'BEGIN PGP PUBLIC KEY BLOCK' "$BASALT_GPG_PUBKEY" || die "$BASALT_GPG_PUBKEY is not an armored public key"
  cp "$BASALT_GPG_PUBKEY" "$work/SOURCES/RPM-GPG-KEY-basalt"
  log "basalt-release ships the key from $BASALT_GPG_PUBKEY"
else
  log "basalt-release ships the placeholder key (set BASALT_GPG_PUBKEY for a usable repository)"
fi

# basalt-logos: the artwork tree travels as a tarball.
logos="$REPO_ROOT/packages/basalt-logos"
lver="$(awk '/^Version:/ {print $2}' "$logos/basalt-logos.spec")"
tar -C "$logos" --owner=0 --group=0 --sort=name -czf "$work/SOURCES/basalt-logos-$lver.tar.gz" tree
cp -p "$logos/basalt-theme.cfg" "$logos/06_basalt_theme" "$work/SOURCES/"
cp -p "$logos/basalt-logos.spec" "$work/SPECS/"

[[ $lab == 1 ]] && cp -p "$REPO_ROOT/packages/lab/basalt-canary/basalt-canary.spec" "$work/lab/"

log "building in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e LAB="$lab" -e BASALT_VERSION="$BASALT_VERSION" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  for spec in /rpmbuild/SPECS/*.spec; do
    rpmbuild --define "_topdir /rpmbuild" --define "basalt_version $BASALT_VERSION" -ba "$spec" >/rpmbuild/$(basename "$spec").log 2>&1 ||
      { tail -40 /rpmbuild/$(basename "$spec").log; exit 1; }
  done
  if [ "$LAB" = 1 ]; then
    for v in 1 2 3; do
      rpmbuild --define "_topdir /rpmbuild/lab/build" --define "canary_version $v" \
        -bb /rpmbuild/lab/basalt-canary.spec >/rpmbuild/lab/canary-$v.log 2>&1 ||
        { tail -40 /rpmbuild/lab/canary-$v.log; exit 1; }
    done
  fi
'

mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "*.rpm" -exec cp {} "$RPM_DIR/" \;
if [[ $lab == 1 ]]; then
  mkdir -p "$RPM_DIR/lab"
  sudo find "$work/lab/build/RPMS" -name "*.rpm" -exec cp {} "$RPM_DIR/lab/" \;
fi
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "*.rpm" -printf "%P  %s bytes\n" | sort
