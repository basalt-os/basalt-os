#!/usr/bin/env bash
# Generate build/site/, the per-deployment files baked into the image:
#
#   /usr/share/basalt/keys/image-signing.pub   public key that signs images
#   /etc/containers/policy.json                 require that key for ${REGISTRY}/${IMAGE_REPO},
#                                               default reject, listed registries allowed
#   /etc/containers/registries.d/50-basalt.yaml read sigstore signatures for it
#   /etc/containers/certs.d/<registry>/ca.crt   lab registry CA (lab only)
#
# With SIGNING_PUBKEY empty no policy is written and the image keeps the base
# policy (accept anything). Keyless (Fulcio) signatures from CI cannot be
# expressed in policy.json for a workflow identity yet; see docs/signing.md.
source "$(dirname "$0")/lib.sh"

out="$REPO_ROOT/build/site"
rm -rf "$out"
install -d "$out"

scope="${REGISTRY}/${IMAGE_REPO}"
: "${POLICY_ALLOW_REGISTRIES:=docker.io quay.io ghcr.io registry.fedoraproject.org registry.access.redhat.com registry.redhat.io}"

if [[ -n "${SIGNING_PUBKEY:-}" ]]; then
  [[ -f "$SIGNING_PUBKEY" ]] || die "SIGNING_PUBKEY not found: $SIGNING_PUBKEY"
  grep -q 'BEGIN PUBLIC KEY' "$SIGNING_PUBKEY" || die "SIGNING_PUBKEY is not a public key"
  log "signature policy: $scope must be signed by $(basename "$SIGNING_PUBKEY")"
  install -d "$out/usr/share/basalt/keys" "$out/etc/containers/registries.d"
  install -m 0644 "$SIGNING_PUBKEY" "$out/usr/share/basalt/keys/image-signing.pub"
  # bootc refuses to verify signatures under a policy whose default is
  # insecureAcceptAnything, so the default is reject. Registries in
  # POLICY_ALLOW_REGISTRIES stay usable without signatures (podman pulls of
  # ordinary images), local transports are unchanged, and this image's own
  # repository requires the signing key.
  python3 - "$scope" "${POLICY_ALLOW_REGISTRIES}" >"$out/etc/containers/policy.json" <<'PY'
import json, sys
scope, allow = sys.argv[1], sys.argv[2].split()
accept = [{"type": "insecureAcceptAnything"}]
docker = {r: accept for r in allow}
docker[scope] = [{
    "type": "sigstoreSigned",
    "keyPath": "/usr/share/basalt/keys/image-signing.pub",
    "signedIdentity": {"type": "matchRepository"},
}]
policy = {
    "default": [{"type": "reject"}],
    "transports": {
        "docker": docker,
        "docker-daemon": {"": accept},
        "containers-storage": {"": accept},
        "oci": {"": accept},
        "oci-archive": {"": accept},
        "dir": {"": accept},
        "docker-archive": {"": accept},
    },
}
json.dump(policy, sys.stdout, indent=4)
print()
PY
  cat >"$out/etc/containers/registries.d/50-basalt.yaml" <<EOF
# Basalt OS images carry sigstore (cosign) signatures as attachments.
docker:
  ${scope}:
    use-sigstore-attachments: true
EOF
fi

lab_ca="$LAB_DIR/registry/client-certs/ca.crt"
if [[ "${LAB_REGISTRY_CA:-auto}" != no && -f "$lab_ca" ]]; then
  log "trusting the lab registry CA for ${REGISTRY} only"
  install -d "$out/etc/containers/certs.d/${REGISTRY}"
  install -m 0644 "$lab_ca" "$out/etc/containers/certs.d/${REGISTRY}/ca.crt"
fi

(cd "$out" && find . -type f | sort)
