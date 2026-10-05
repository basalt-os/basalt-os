#!/usr/bin/env bash
# Build the basalt-apps RPMs (basalt-apps-selinux, basalt-security-activity
# and its -selinux, source) in a Fedora container from
# github.com/basalt-os/basalt-apps at the commit pinned in
# packages/basalt-apps/source.conf, and copy them to OUT_DIR (default
# $BUILD_DIR/rpms/<fedora>/testing/: the apps ship in basalt-testing while
# they are pre-release, never in the basalt set of $RPM_DIR).
#
#   packages/basalt-apps/build.sh [OUT_DIR]
#
# BASALT_APPS_SRC=DIR builds from a local clone instead of fetching (the
# pinned commit must exist there; BASALT_APPS_COMMIT overrides the pin).
# The SELinux modules build against the interfaces of basalt_shell (from the
# basalt-shell commit pinned in packages/basalt-shell/source.conf, the file
# basalt-shell-selinux installs) and basalt_ledger (this repository,
# packages/basalt-ledger/selinux, the file basalt-ledger-selinux installs).
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-apps"
out="${1:-$RPM_DIR/testing}"
pin_commit="${BASALT_APPS_COMMIT:-}"
# shellcheck source=/dev/null
source "$pkg/source.conf"
[[ -n "$pin_commit" ]] && BASALT_APPS_COMMIT="$pin_commit"
[[ "$BASALT_APPS_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "BASALT_APPS_COMMIT must be a full commit id"
# shellcheck source=/dev/null
source "$REPO_ROOT/packages/basalt-shell/source.conf"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$work/git" "$work/shell"

if [[ -n "${BASALT_APPS_SRC:-}" ]]; then
  git -C "$work/git" init -q
  git -C "$work/git" fetch -q "$(cd "$BASALT_APPS_SRC" && pwd)" "$BASALT_APPS_COMMIT" ||
    die "commit $BASALT_APPS_COMMIT not in $BASALT_APPS_SRC"
  log "basalt-apps from the local clone $BASALT_APPS_SRC"
else
  git -C "$work/git" init -q
  git -C "$work/git" fetch -q --depth 1 "$BASALT_APPS_REPO_URL" "$BASALT_APPS_COMMIT" ||
    die "cannot fetch $BASALT_APPS_COMMIT from $BASALT_APPS_REPO_URL"
fi
[[ "$(git -C "$work/git" rev-parse FETCH_HEAD)" == "$BASALT_APPS_COMMIT" ]] || die "fetched commit differs from the pin"
ver="$(git -C "$work/git" show "$BASALT_APPS_COMMIT:VERSION" | tr -d '[:space:]')"
[[ "$ver" =~ ^[0-9]+(\.[0-9]+)*$ ]] || die "bad VERSION at $BASALT_APPS_COMMIT: $ver"
# git archive honors the repository's export-ignore (media/ stays out).
git -C "$work/git" archive --format=tar.gz --prefix="basalt-apps-$ver/" -o "$work/SOURCES/basalt-apps-$ver.tar.gz" "$BASALT_APPS_COMMIT"
git -C "$work/git" show "$BASALT_APPS_COMMIT:packaging/basalt-apps.spec" >"$work/SPECS/basalt-apps.spec"

# The interface files of the modules the app policy builds against.
git -C "$work/shell" init -q
if [[ -n "${BASALT_SHELL_SRC:-}" ]]; then
  git -C "$work/shell" fetch -q "$(cd "$BASALT_SHELL_SRC" && pwd)" "$BASALT_SHELL_COMMIT"
else
  git -C "$work/shell" fetch -q --depth 1 "$BASALT_SHELL_REPO_URL" "$BASALT_SHELL_COMMIT"
fi || die "cannot fetch basalt-shell $BASALT_SHELL_COMMIT"
git -C "$work/shell" show "$BASALT_SHELL_COMMIT:selinux/basalt_shell.if" >"$work/basalt_shell.if"
cp -p "$REPO_ROOT/packages/basalt-ledger/selinux/basalt_ledger.if" "$work/"

log "basalt-apps $ver (commit ${BASALT_APPS_COMMIT:0:12}) in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e VER="$ver" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros cmake gcc-c++ make golang qt6-qtbase-devel qt6-qtdeclarative-devel \
    qt6-qttools-devel qt6-linguist desktop-file-utils libappstream-glib selinux-policy-devel bzip2 >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  # Stand in for the basalt-shell-selinux and basalt-ledger-selinux build
  # dependencies (the same files those packages install).
  install -Dpm 0644 /rpmbuild/basalt_shell.if /usr/share/selinux/devel/include/distributed/basalt_shell.if
  install -Dpm 0644 /rpmbuild/basalt_ledger.if /usr/share/selinux/devel/include/distributed/basalt_ledger.if
  rpmbuild --define "_topdir /rpmbuild" --define "basalt_version $VER" --nodeps \
    -ba /rpmbuild/SPECS/basalt-apps.spec >/rpmbuild/build.log 2>&1 || { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$out"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-*.rpm" -exec cp {} "$out/" \;
sudo chown -R "$(id -u):$(id -g)" "$out"
find "$out" -name "basalt-apps*.rpm" -o -name "basalt-security-activity*.rpm" | xargs -r -n1 basename | sort
