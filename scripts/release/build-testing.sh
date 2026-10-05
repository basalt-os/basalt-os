#!/usr/bin/env bash
# Release build of the basalt-testing repository: pre-release packages,
# unsigned, for the release signer. Kept apart from scripts/release/build.sh
# (the basalt set) so a testing package never lands in basalt by mistake.
#
#   scripts/release/build-testing.sh OUT_DIR
#
# Packages: basalt-shell (the desktop shell, from github.com/basalt-os/basalt-shell
# at the commit pinned in packages/basalt-shell/source.conf, with -selinux,
# -niri and basalt-desktop, the desktop edition's package set) and basalt-voice
# (whisper.cpp speech to text from its release archive, pinned by version and
# SHA-256 in packages/basalt-voice/build.sh; no speech model is packaged:
# basalt-voice-fetch downloads them on the person's machine, checked against
# packages/basalt-voice/models.manifest).
#
# OUT_DIR receives the binary and source RPMs, BUILD-INFO.txt and
# SHA256SUMS. Then, as for the basalt set but with OB_REPO=basalt-testing:
#   OB_REPO=basalt-testing scripts/release/sign.sh --op OUT_DIR SIGNED_DIR
#   OB_REPO=basalt-testing scripts/release/client-test.sh SIGNED_DIR KEY basalt-shell basalt-voice
#   scripts/release/upload.sh SIGNED_DIR
# The tree is basalt-testing/<releasever>/<arch>/, which basalt-release's
# [basalt-testing] repository (off by default) reads.
ENV_FILE=/dev/null
export ENV_FILE
source "$(dirname "$0")/../lib.sh"

out="${1:?usage: $0 OUT_DIR}"
for var in BASALT_SHELL_SRC BASALT_SHELL_COMMIT; do
  [[ -z "${!var:-}" ]] || die "$var is set: a release build uses the pinned public commit, unset it"
done
[[ -z "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]] || die "the checkout has local changes; release builds come from a clean commit"
commit="$(git -C "$REPO_ROOT" rev-parse HEAD)" || die "not a git checkout"
# shellcheck source=/dev/null
source "$REPO_ROOT/packages/basalt-shell/source.conf"

stage="$BUILD_DIR/rpms/$FEDORA_RELEASE/testing"
rm -rf "$stage"
"$REPO_ROOT/packages/basalt-shell/build.sh" "$stage"
"$REPO_ROOT/packages/basalt-voice/build.sh" "$stage"

log "rpmlint on the built packages"
in_fedora -v "$stage:/rpms:ro" -v "$REPO_ROOT/scripts/ci:/ci:ro" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpmlint >/dev/null 2>&1 || { echo "dnf install failed" >&2; exit 1; }
  rpmlint -c /ci/rpmlint.toml /rpms/*.rpm' || die "rpmlint found errors in the built packages"

mkdir -p "$out"
find "$out" -maxdepth 1 -name '*.rpm' -delete
rm -f "$out/PUBLISHED"
find "$stage" -maxdepth 1 -name '*.rpm' -exec cp -p {} "$out/" \;

cat >"$out/BUILD-INFO.txt" <<INFO
Basalt OS testing build (UNSIGNED, input for OB_REPO=basalt-testing scripts/release/sign.sh)
repository:     basalt-testing
fedora release: $FEDORA_RELEASE
architecture:   $ARCH
commit:         $commit
basalt-shell:   $BASALT_SHELL_REPO_URL $BASALT_SHELL_COMMIT
basalt-voice:   whisper.cpp $(awk '/^%global whisper_version/ {print $3}' "$REPO_ROOT/packages/basalt-voice/basalt-voice.spec")
built:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
INFO
(cd "$out" && find . -maxdepth 1 -type f -name '*.rpm' -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
log "testing build in $out:"
find "$out" -maxdepth 1 -name '*.rpm' -printf '  %P  %s bytes\n' | sort >&2
