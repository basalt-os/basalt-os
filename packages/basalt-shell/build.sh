#!/usr/bin/env bash
# Build the basalt-shell RPMs (binary, -selinux, source) in a Fedora
# container from github.com/basalt-os/basalt-shell at the commit pinned in
# packages/basalt-shell/source.conf, and copy them to OUT_DIR (default
# $BUILD_DIR/rpms/<fedora>/testing/: the shell ships in basalt-testing while
# it is pre-release, never in the basalt set of $RPM_DIR).
#
#   packages/basalt-shell/build.sh [OUT_DIR]
#
# BASALT_SHELL_SRC=DIR builds from a local clone instead of fetching (the
# pinned commit must exist there; BASALT_SHELL_COMMIT overrides the pin).
# The SELinux subpackage builds against basalt_agent_base from this
# repository (packages/basalt-agent/selinux), the interface that
# basalt-agent-selinux installs.
source "$(dirname "$0")/../../scripts/lib.sh"
pkg="$REPO_ROOT/packages/basalt-shell"
out="${1:-$RPM_DIR/testing}"
pin_commit="${BASALT_SHELL_COMMIT:-}"
# shellcheck source=/dev/null
source "$pkg/source.conf"
[[ -n "$pin_commit" ]] && BASALT_SHELL_COMMIT="$pin_commit"
[[ "$BASALT_SHELL_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "BASALT_SHELL_COMMIT must be a full commit id"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$work/git"

if [[ -n "${BASALT_SHELL_SRC:-}" ]]; then
  git -C "$work/git" init -q
  git -C "$work/git" fetch -q "$(cd "$BASALT_SHELL_SRC" && pwd)" "$BASALT_SHELL_COMMIT" ||
    die "commit $BASALT_SHELL_COMMIT not in $BASALT_SHELL_SRC"
  log "basalt-shell from the local clone $BASALT_SHELL_SRC"
else
  git -C "$work/git" init -q
  git -C "$work/git" fetch -q --depth 1 "$BASALT_SHELL_REPO_URL" "$BASALT_SHELL_COMMIT" ||
    die "cannot fetch $BASALT_SHELL_COMMIT from $BASALT_SHELL_REPO_URL"
fi
[[ "$(git -C "$work/git" rev-parse FETCH_HEAD)" == "$BASALT_SHELL_COMMIT" ]] || die "fetched commit differs from the pin"
ver="$(git -C "$work/git" show "$BASALT_SHELL_COMMIT:VERSION" | tr -d '[:space:]')"
[[ "$ver" =~ ^[0-9]+(\.[0-9]+)*$ ]] || die "bad VERSION at $BASALT_SHELL_COMMIT: $ver"
# git archive honors the shell's export-ignore (media/ stays out).
git -C "$work/git" archive --format=tar.gz --prefix="basalt-shell-$ver/" -o "$work/SOURCES/basalt-shell-$ver.tar.gz" "$BASALT_SHELL_COMMIT"
git -C "$work/git" show "$BASALT_SHELL_COMMIT:packaging/basalt-shell.spec" >"$work/SPECS/basalt-shell.spec"
cp -p "$REPO_ROOT/packages/basalt-agent/selinux/basalt_agent_base.if" "$work/"

log "basalt-shell $ver (commit ${BASALT_SHELL_COMMIT:0:12}) in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e VER="$ver" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros golang make selinux-policy-devel bzip2 >/dev/null 2>&1 ||
    { echo "dnf install failed"; exit 1; }
  # Stands in for the basalt-agent-selinux build dependency (same file).
  install -Dpm 0644 /rpmbuild/basalt_agent_base.if /usr/share/selinux/devel/include/distributed/basalt_agent_base.if
  rpmbuild --define "_topdir /rpmbuild" --define "basalt_version $VER" --nodeps \
    -ba /rpmbuild/SPECS/basalt-shell.spec >/rpmbuild/build.log 2>&1 || { tail -60 /rpmbuild/build.log; exit 1; }
'
mkdir -p "$out"
sudo find "$work/RPMS" "$work/SRPMS" -name "basalt-shell*.rpm" -exec cp {} "$out/" \;
sudo chown -R "$(id -u):$(id -g)" "$out"
find "$out" -name "basalt-shell*.rpm" -printf "%P  %s bytes\n" | sort
