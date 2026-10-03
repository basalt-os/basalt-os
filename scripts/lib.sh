# Shared helpers for the build and lab scripts. Source it, do not execute it.
# shellcheck shell=bash

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"

# Load site configuration. Values already in the environment win, so CI and
# one-off overrides (BASALT_VERSION=0.0.2 make build) work without editing .env.
if [[ -f "$ENV_FILE" ]]; then
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
    key="${line%%=*}"
    [[ -n "${!key+x}" ]] && continue
    eval "export $line"
  done <"$ENV_FILE"
fi

: "${BASE_IMAGE:=quay.io/fedora/fedora-bootc:44}"
: "${BASALT_VERSION:=$(tr -d "[:space:]" <"$REPO_ROOT/VERSION")}"
: "${REGISTRY:=localhost}"
: "${IMAGE_REPO:=basalt/basalt-os}"
: "${CHANNEL_TAG:=stable}"
: "${PODMAN:=sudo podman}"
: "${LAB_DIR:=/srv/basalt-lab}"

# Used by the scripts that source this file.
# shellcheck disable=SC2034
IMAGE="${REGISTRY}/${IMAGE_REPO}"
# shellcheck disable=SC2034
LOCAL_IMAGE="localhost/basalt-os"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Print a command, then run it. Secrets are never passed on a command line.
run() { printf '  $ %s\n' "$*" >&2; "$@"; }

# Lab registry trust and credentials, when the lab registry is in use. A public
# registry needs neither: tools fall back to their normal configuration.
lab_registry_opts() {
  LAB_CERT_DIR="$LAB_DIR/registry/client-certs"
  LAB_AUTH_FILE="$LAB_DIR/registry/auth.json"
  PODMAN_REG_OPTS=()
  SKOPEO_REG_OPTS=()   # skopeo inspect
  SKOPEO_COPY_OPTS=()  # skopeo copy (source and destination are the same registry)
  if [[ -d "$LAB_CERT_DIR" ]]; then
    PODMAN_REG_OPTS+=(--cert-dir "$LAB_CERT_DIR")
    SKOPEO_REG_OPTS+=(--cert-dir "$LAB_CERT_DIR")
    SKOPEO_COPY_OPTS+=(--src-cert-dir "$LAB_CERT_DIR" --dest-cert-dir "$LAB_CERT_DIR")
  fi
  if [[ -f "$LAB_AUTH_FILE" ]]; then
    PODMAN_REG_OPTS+=(--authfile "$LAB_AUTH_FILE")
    SKOPEO_REG_OPTS+=(--authfile "$LAB_AUTH_FILE")
    SKOPEO_COPY_OPTS+=(--authfile "$LAB_AUTH_FILE")
  fi
}
