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

# Run a command in a throwaway Fedora container (host network: on some hosts
# DNS does not resolve inside rootful podman's default network).
in_fedora() {
  $PODMAN run --rm --network=host --security-opt label=disable "$@"
}
