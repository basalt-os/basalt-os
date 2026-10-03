#!/usr/bin/env bash
# Push a built image, sign it by digest with cosign, then point the channel tag
# at that digest. Registry-agnostic: any OCI registry the engine can log in to.
#
#   BASALT_VERSION=0.0.1 scripts/publish.sh
#
# Signing (key-based, for lab and private registries):
#   COSIGN_KEY             private key  (default $LAB_DIR/keys/cosign.key)
#   COSIGN_PASSWORD_FILE   its passphrase file (default $LAB_DIR/keys/cosign.password)
#   SIGN=0                 skip signing (an image policy that requires a signature
#                          will then refuse the image, which is the point)
# Public CI signs keyless instead (.github/workflows/image.yml).
source "$(dirname "$0")/lib.sh"
lab_registry_opts

: "${COSIGN:=$LAB_DIR/bin/cosign}"
: "${COSIGN_KEY:=$LAB_DIR/keys/cosign.key}"
: "${COSIGN_PASSWORD_FILE:=$LAB_DIR/keys/cosign.password}"
: "${SIGN:=1}"
: "${SKOPEO:=skopeo}"

install -d "$REPO_ROOT/build"
digestfile="$REPO_ROOT/build/digest-$BASALT_VERSION"

start=$(date +%s)
log "pushing $IMAGE:$BASALT_VERSION"
run $PODMAN push "${PODMAN_REG_OPTS[@]}" --digestfile "$digestfile" \
  "$IMAGE:$BASALT_VERSION"
digest="$(cat "$digestfile")"
ref="$IMAGE@$digest"
log "pushed $ref in $(($(date +%s) - start))s"

# cosign reads registry credentials from DOCKER_CONFIG and trusts the system
# CA bundle plus, for a lab registry, the lab CA.
cosign_env=()
if [[ -f "${LAB_AUTH_FILE:-}" ]]; then
  dc="$LAB_DIR/registry/docker"
  install -d -m 0700 "$dc"
  ln -sf "$LAB_AUTH_FILE" "$dc/config.json"
  cosign_env+=(DOCKER_CONFIG="$dc")
fi
if [[ -f "${LAB_CERT_DIR:-}/ca.crt" ]]; then
  bundle="$LAB_DIR/registry/ca-bundle.pem"
  sys_bundle=""
  for f in /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/ssl/certs/ca-certificates.crt; do
    [[ -f "$f" ]] && { sys_bundle="$f"; break; }
  done
  cat $sys_bundle "$LAB_CERT_DIR/ca.crt" >"$bundle"
  cosign_env+=(SSL_CERT_FILE="$bundle")
fi

if [[ "$SIGN" == 1 ]]; then
  [[ -f "$COSIGN_KEY" ]] || die "signing key not found: $COSIGN_KEY (make lab-keys)"
  log "signing $ref with $(basename "$COSIGN_KEY") (no transparency log upload: private registry)"
  # Exported in a subshell (not passed through env(1)) so the passphrase never
  # appears in a process argument list.
  (
    for kv in "${cosign_env[@]}"; do export "${kv?}"; done
    COSIGN_PASSWORD="$(cat "$COSIGN_PASSWORD_FILE")"; export COSIGN_PASSWORD
    "$COSIGN" sign --yes --key "$COSIGN_KEY" --tlog-upload=false "$ref"
  )
else
  log "SIGN=0: $ref is NOT signed"
fi

log "moving tag $CHANNEL_TAG to $digest"
run $SKOPEO copy --preserve-digests --all "${SKOPEO_COPY_OPTS[@]}" \
  "docker://$ref" "docker://$IMAGE:$CHANNEL_TAG"

log "published $IMAGE:$BASALT_VERSION and $IMAGE:$CHANNEL_TAG = $digest in $(($(date +%s) - start))s"
