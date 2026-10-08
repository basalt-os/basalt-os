#!/usr/bin/env bash
# Build the basalt-audit-suite RPM (noarch) in a Fedora container from
# github.com/basalt-os/ai-audit-suite at the commit pinned in source.conf,
# and copy it to OUT_DIR (default $RPM_DIR/testing: it ships in
# basalt-testing with Security and Activity while that is pre-release).
#
#   packages/basalt-audit-suite/build.sh [OUT_DIR]
#
# AUDIT_SUITE_SRC=DIR builds from a local clone instead of fetching (the
# pinned commit must exist there).
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-audit-suite"
out="${1:-$RPM_DIR/testing}"
# shellcheck source=/dev/null
source "$pkg/source.conf"
[[ "$AUDIT_SUITE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "AUDIT_SUITE_COMMIT must be a full commit id"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$work/git"
git -C "$work/git" init -q
if [[ -n "${AUDIT_SUITE_SRC:-}" ]]; then
  git -C "$work/git" fetch -q "$(cd "$AUDIT_SUITE_SRC" && pwd)" "$AUDIT_SUITE_COMMIT" ||
    die "commit $AUDIT_SUITE_COMMIT not in $AUDIT_SUITE_SRC"
else
  git -C "$work/git" fetch -q --depth 1 "$AUDIT_SUITE_REPO_URL" "$AUDIT_SUITE_COMMIT" ||
    die "cannot fetch $AUDIT_SUITE_COMMIT from $AUDIT_SUITE_REPO_URL"
fi
[[ "$(git -C "$work/git" rev-parse FETCH_HEAD)" == "$AUDIT_SUITE_COMMIT" ]] || die "fetched commit differs from the pin"
ver="$(git -C "$work/git" show "$AUDIT_SUITE_COMMIT:VERSION" | tr -d '[:space:]')"
[[ "$ver" =~ ^[0-9]+(\.[0-9]+)*$ ]] || die "bad VERSION at $AUDIT_SUITE_COMMIT: $ver"
git -C "$work/git" archive --format=tar.gz --prefix="ai-audit-suite-$ver/" -o "$work/SOURCES/ai-audit-suite-$ver.tar.gz" "$AUDIT_SUITE_COMMIT"
cp -p "$pkg/basalt-audit-suite" "$pkg/basalt-audit-suite.sysusers" "$pkg/audit.conf" "$work/SOURCES/"
cp -p "$pkg/basalt-audit-suite.spec" "$work/SPECS/"

log "basalt-audit-suite $ver (commit ${AUDIT_SUITE_COMMIT:0:12}) in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e VER="$ver" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  rpmbuild --define "_topdir /rpmbuild" --define "suite_version $VER" --nodeps \
    -ba /rpmbuild/SPECS/basalt-audit-suite.spec >/rpmbuild/build.log 2>&1 || { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$out"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-audit-suite*.rpm" -exec cp {} "$out/" \;
sudo chown -R "$(id -u):$(id -g)" "$out"
find "$out" -name "basalt-audit-suite*.rpm" | xargs -r -n1 basename | sort
