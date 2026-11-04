#!/usr/bin/env bash
# Release build of the APT testing suite (https://obpkg.org/apt, suite
# testing): pre-release .deb packages, unsigned, for the release signer.
# The Debian and Ubuntu counterpart of scripts/release/build-testing.sh.
#
#   scripts/release/build-testing-apt.sh OUT_DIR
#
# Packages: cotty and cotty-guard (amd64, arm64) from the commit pinned in
# packages/cotty/source.conf (packages/cotty/build-deb.sh).
#
# OUT_DIR receives the .deb files, BUILD-INFO.txt and SHA256SUMS. Then:
#   OB_APT_SUITE=testing scripts/release/merge-published-apt.sh OUT_DIR
#   OB_APT_SUITE=testing scripts/release/sign-apt.sh --op OUT_DIR SIGNED_DIR
#   CLIENT_TEST_SUITE=testing CLIENT_TEST_NO_VERSION="cotty cotty-guard" scripts/release/client-test-apt.sh SIGNED_DIR packages/basalt-release/RPM-GPG-KEY-basalt cotty cotty-guard
#   scripts/release/upload.sh SIGNED_DIR
# merge-published-apt.sh brings in every package already in the suite, so
# the new metadata keeps them. (cotty has no "version" command, hence
# CLIENT_TEST_NO_VERSION.)
ENV_FILE=/dev/null
export ENV_FILE
source "$(dirname "$0")/../lib.sh"

out="${1:?usage: $0 OUT_DIR}"
[[ -z "${COTTY_COMMIT_OVERRIDE:-}" ]] || die "COTTY_COMMIT_OVERRIDE is set: a release build uses the pinned commit, unset it"
[[ -z "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]] || die "the checkout has local changes; release builds come from a clean commit"
commit="$(git -C "$REPO_ROOT" rev-parse HEAD)" || die "not a git checkout"
# shellcheck source=packages/cotty/source.conf
source "$REPO_ROOT/packages/cotty/source.conf"

stage="$BUILD_DIR/debs/testing"
rm -rf "$stage"
"$REPO_ROOT/packages/cotty/build-deb.sh" "$stage" amd64 arm64

mkdir -p "$out"
find "$out" -maxdepth 1 -name '*.deb' -delete
rm -f "$out/PUBLISHED" "$out/POOL-PATHS"
find "$stage" -maxdepth 1 -name '*.deb' -exec cp -p {} "$out/" \;

cat >"$out/BUILD-INFO.txt" <<INFO
OpenBasalt APT testing build (UNSIGNED, input for OB_APT_SUITE=testing scripts/release/sign-apt.sh)
repository:     apt, suite testing
commit:         $commit
cotty:          $COTTY_REPO_URL $COTTY_COMMIT
built:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
INFO
(cd "$out" && find . -maxdepth 1 -type f -name '*.deb' -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
log "apt testing build in $out:"
find "$out" -maxdepth 1 -name '*.deb' -printf '  %P  %s bytes\n' | sort >&2
