# Shared helpers for the build and lab scripts. Source it, do not execute it.
# shellcheck shell=bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"

# Load site configuration. Values already in the environment win, so one-off
# overrides (FEDORA_RELEASE=45 make rpms) work without editing .env.
if [[ -f "$ENV_FILE" ]]; then
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
    key="${line%%=*}"
    [[ -n "${!key+x}" ]] && continue
    eval "export $line"
  done <"$ENV_FILE"
fi

: "${FEDORA_RELEASE:=44}"
: "${BASALT_VERSION:=$(tr -d "[:space:]" <"$REPO_ROOT/VERSION")}"
# Version and stage (docs/versioning.md): BASALT_VERSION is <fedora>.<n>
# (VERSION file), BASALT_STAGE one of dev, alpha.N, beta.N, rc.N or final
# (the official release), BASALT_BUILD the build date (YYYYMMDD, UTC).
# BASALT_FULL_VERSION names the images: 44.0-dev.20261005, 44.0-alpha.1, 44.0.
: "${BASALT_STAGE:=dev}"
: "${BASALT_BUILD:=$(date -u +%Y%m%d)}"
[[ "$BASALT_VERSION" =~ ^[0-9]+\.[0-9]+$ ]] || { printf 'error: BASALT_VERSION must be <fedora>.<n>, got %s\n' "$BASALT_VERSION" >&2; exit 1; }
[[ "$BASALT_STAGE" =~ ^(dev|final|(alpha|beta|rc)\.[1-9][0-9]*)$ ]] ||
  { printf 'error: BASALT_STAGE must be dev, alpha.N, beta.N, rc.N or final, got %s\n' "$BASALT_STAGE" >&2; exit 1; }
[[ "$BASALT_BUILD" =~ ^20[0-9]{6}$ ]] || { printf 'error: BASALT_BUILD must be a date (YYYYMMDD), got %s\n' "$BASALT_BUILD" >&2; exit 1; }
case "$BASALT_STAGE" in
  dev) BASALT_FULL_VERSION="$BASALT_VERSION-dev.$BASALT_BUILD" ;;
  final) BASALT_FULL_VERSION="$BASALT_VERSION" ;;
  *) BASALT_FULL_VERSION="$BASALT_VERSION-$BASALT_STAGE" ;;
esac
export BASALT_VERSION BASALT_STAGE BASALT_BUILD BASALT_FULL_VERSION
# Image file name without .iso: basalt-os-<full version>-<edition>-<arch>,
# plus an optional suffix for lab and test variants.
iso_name() { printf 'basalt-os-%s-%s-%s%s' "$BASALT_FULL_VERSION" "$1" "$ARCH" "${2:+-$2}"; }
# BUILD_ID in os-release: UTC build date and the commit of this tree.
: "${BASALT_BUILD_ID:=$(date -u +%Y%m%d).$(git -C "$REPO_ROOT" rev-parse --short=12 HEAD 2>/dev/null || echo local)}"
: "${PODMAN:=sudo podman}"
: "${LAB_DIR:=/srv/basalt-lab}"
: "${BUILD_DIR:=$REPO_ROOT/build}"
# Signed RPM repository tree (served over HTTP in the lab).
: "${REPO_DIR:=$BUILD_DIR/repo}"
# Development signing key (lab only). Release keys never live on a build host.
: "${GNUPGHOME_LAB:=$LAB_DIR/gpg}"
: "${BASALT_GPG_PUBKEY:=}"
: "${ARCH:=x86_64}"
: "${FEDORA_IMAGE:=registry.fedoraproject.org/fedora:$FEDORA_RELEASE}"

# shellcheck disable=SC2034  # used by the scripts that source this file
RPM_DIR="$BUILD_DIR/rpms/$FEDORA_RELEASE"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Print a command, then run it. Secrets are never passed on a command line.
run() { printf '  $ %s\n' "$*" >&2; "$@"; }

# curl options for downloads from the network (mirrors, release pages):
# transient errors are retried instead of failing a build.
# shellcheck disable=SC2034  # used by the scripts that source this file
CURL_RETRY=(--retry 5 --retry-all-errors --retry-delay 5)

# Run a command in a throwaway Fedora container (host network: on some hosts
# DNS does not resolve inside rootful podman's default network).
in_fedora() {
  $PODMAN run --rm --network=host --security-opt label=disable "$@"
}

# rpmbuild_failed NAME LOG: explain why the container build of NAME failed,
# then exit. LOG is rpmbuild's output as the host sees it. Failed Go tests
# in %check are named with their output (and as a GitHub Actions error
# annotation), so a test failure reads as one instead of rpm's opaque
# "Bad exit status from ... (%check)".
rpmbuild_failed() {
  local name="$1" logf="$2" stage tests
  [[ -r "$logf" ]] || die "$name: the container build failed before rpmbuild ran (see the output above)"
  stage="$(grep -oE 'Bad exit status from [^ ]+ \(%[a-z]+\)' "$logf" | grep -oE '%[a-z]+' | tail -1 || true)"
  tests="$(sed -nE 's/^[[:space:]]*--- FAIL: ([^ ]+).*/\1/p' "$logf" | sort -u | paste -sd' ' || true)"
  if [[ -n "$tests" ]]; then
    printf '\n' >&2
    log "$name: Go tests failed in ${stage:-the build}: $tests"
    # Each failure with its indented output, data races and panics, and the
    # FAIL line of the package it belongs to.
    awk '/^[[:space:]]*--- FAIL: |^WARNING: DATA RACE|^panic: / { p = 1 }
         p && /^(ok|\?)[[:space:]]/ { p = 0 }
         p { print } /^FAIL[[:space:]]/ { p = 0 }' "$logf" | head -200 >&2
    [[ "${GITHUB_ACTIONS:-}" == true ]] && echo "::error title=$name: Go tests failed in ${stage:-the build}::$tests"
    die "$name: Go tests failed in ${stage:-the build}: $tests"
  fi
  tail -60 "$logf" >&2
  [[ "${GITHUB_ACTIONS:-}" == true ]] && echo "::error title=$name: rpmbuild failed::in ${stage:-an unknown stage} (log above)"
  die "$name: rpmbuild failed in ${stage:-an unknown stage} (last lines above)"
}
