#!/usr/bin/env bash
# CI build: packages, unsigned repository, installer ISO, checksums.
#
#   scripts/ci/build.sh [rpms|repo|iso|collect|all]   default: all
#
#   rpms     scripts/build-rpms.sh (Fedora container), then rpmlint on the results (errors fail)
#   repo     scripts/ci/repo-unsigned.sh: createrepo_c metadata, no signatures
#   iso      scripts/iso.sh fetch + build, without site files (the installer
#            asks for accounts), from the unsigned repository
#   collect  $CI_OUT/: repo/, the ISO, BUILD-INFO.txt and SHA256SUMS
#
# The same commands run locally (`make ci-build`). CI holds no keys: the
# repository and the ISO are unsigned test builds, see repo-unsigned.sh.
source "$(dirname "$0")/../lib.sh"

# The CI repository lives apart from a lab's signed one, and the CI ISO
# never carries site files, whatever .env says.
export REPO_DIR="$BUILD_DIR/repo-unsigned"
export SITE_DIR=""
: "${CI_OUT:=$BUILD_DIR/ci-out}"
iso_name="$(iso_name server netinst).iso"

rpms() {
  "$REPO_ROOT/scripts/build-rpms.sh"
  log "rpmlint on the built packages"
  in_fedora -v "$RPM_DIR:/rpms:ro" -v "$REPO_ROOT/scripts/ci:/ci:ro" "$FEDORA_IMAGE" bash -euc '
    dnf -q -y install rpmlint >/dev/null 2>&1 || { echo "dnf install failed" >&2; exit 1; }
    rpmlint -c /ci/rpmlint.toml /rpms/*.rpm' || die "rpmlint found errors in the built packages"
}

repo() { "$REPO_ROOT/scripts/ci/repo-unsigned.sh"; }

iso() {
  "$REPO_ROOT/scripts/iso.sh" fetch
  "$REPO_ROOT/scripts/iso.sh" build
}

collect() {
  [[ -f "$BUILD_DIR/iso/$iso_name" ]] || die "no ISO at $BUILD_DIR/iso/$iso_name"
  rm -rf "$CI_OUT"
  mkdir -p "$CI_OUT"
  cp -a "$REPO_DIR" "$CI_OUT/repo"
  cp "$BUILD_DIR/iso/$iso_name" "$CI_OUT/"
  local fedora_iso
  fedora_iso="$(cat "$BUILD_DIR/iso-cache/latest" 2>/dev/null || echo unknown)"
  cat >"$CI_OUT/BUILD-INFO.txt" <<EOF
Basalt OS CI build (UNSIGNED, for testing only)
version:        $BASALT_FULL_VERSION
fedora release: $FEDORA_RELEASE
architecture:   $ARCH
commit:         $(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)
built:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
base ISO:       $fedora_iso
signatures:     none. Packages, repository metadata and ISO are unsigned;
                RPM-GPG-KEY-basalt is a placeholder. Release builds are signed
                on the release signer (docs/key-ceremony.md).
installer:      no site files; the installer asks for a root password or a user.
EOF
  (cd "$CI_OUT" && find . -type f ! -name SHA256SUMS -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
  log "artifacts in $CI_OUT:"
  find "$CI_OUT" -maxdepth 1 -printf '  %P %s bytes\n' | sort >&2
}

case "${1:-all}" in
  rpms) rpms ;;
  repo) repo ;;
  iso) iso ;;
  collect) collect ;;
  all) rpms; repo; iso; collect ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac
