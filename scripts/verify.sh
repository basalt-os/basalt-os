#!/usr/bin/env bash
# Verify a published image the way a client does.
#
#   scripts/verify.sh [tag]            default: $CHANNEL_TAG
#   EXPECT=reject scripts/verify.sh t  succeed only if the policy REFUSES the tag
#
# 1. cosign verify against the public key (no transparency log: private registry).
# 2. containers-policy.json: copy the image with the same policy and
#    registries.d files that the image ships (build/site), so the check is the
#    one bootc performs on the installed system.
source "$(dirname "$0")/lib.sh"
lab_registry_opts

tag="${1:-$CHANNEL_TAG}"
: "${EXPECT:=accept}"
: "${COSIGN:=$LAB_DIR/bin/cosign}"
: "${SIGNING_PUBKEY:?SIGNING_PUBKEY is required to verify}"
ref="$IMAGE:$tag"
site="$REPO_ROOT/build/site"
[[ -f "$site/etc/containers/policy.json" ]] || die "no policy in $site (scripts/site-overlay.sh)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cosign_ok=0
(
  if [[ -f "${LAB_AUTH_FILE:-}" ]]; then export DOCKER_CONFIG="$LAB_DIR/registry/docker"; fi
  if [[ -f "$LAB_DIR/registry/ca-bundle.pem" ]]; then export SSL_CERT_FILE="$LAB_DIR/registry/ca-bundle.pem"; fi
  "$COSIGN" verify --key "$SIGNING_PUBKEY" --insecure-ignore-tlog=true "$ref" >"$tmp/cosign.json" 2>"$tmp/cosign.err"
) && cosign_ok=1
if [[ $cosign_ok == 1 ]]; then
  log "cosign: signature valid for $ref ($(grep -o '"docker-manifest-digest":"[^"]*"' "$tmp/cosign.json" | head -1))"
else
  log "cosign: verification FAILED for $ref: $(tail -1 "$tmp/cosign.err")"
fi

# Same policy as the image, with the key path pointed at the host copy.
sed "s#/usr/share/basalt/keys/image-signing.pub#${SIGNING_PUBKEY}#" \
  "$site/etc/containers/policy.json" >"$tmp/policy.json"
policy_ok=0
if skopeo copy --quiet --policy "$tmp/policy.json" \
     --registries.d "$site/etc/containers/registries.d" \
     "${SKOPEO_COPY_OPTS[@]}" "docker://$ref" "dir:$tmp/img" 2>"$tmp/policy.err"; then
  policy_ok=1
  log "policy.json: $ref accepted"
else
  log "policy.json: $ref REJECTED: $(tail -1 "$tmp/policy.err")"
fi

case "$EXPECT" in
  accept) [[ $cosign_ok == 1 && $policy_ok == 1 ]] || die "expected $ref to be accepted" ;;
  reject) [[ $cosign_ok == 0 && $policy_ok == 0 ]] || die "expected $ref to be rejected" ;;
  *) die "EXPECT must be accept or reject" ;;
esac
log "verify: $ref behaved as expected ($EXPECT)"
