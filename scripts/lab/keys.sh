#!/usr/bin/env bash
# Generate lab-only key material under $LAB_DIR/keys (mode 0700):
#   cosign.key / cosign.pub   development image-signing key pair (cosign)
#   cosign.password           its passphrase (0600)
#   vm_ed25519[.pub]          SSH key for root on lab VMs
#   authorized_keys           what lab installs inject for root (vm key plus
#                             any extra public keys listed in EXTRA_SSH_PUBKEYS)
#
# These are development keys. Release signing keys are never generated on a
# build host: they live offline in the project's vault (see docs/signing.md).
# Nothing here prints private material.
source "$(dirname "$0")/../lib.sh"

: "${COSIGN:=$LAB_DIR/bin/cosign}"
keys="$LAB_DIR/keys"
install -d -m 0700 "$keys"
umask 077

if [[ ! -f "$keys/cosign.key" ]]; then
  [[ -x "$COSIGN" ]] || die "cosign not found at $COSIGN (scripts/lab/tools.sh)"
  log "generating development cosign key pair in $keys"
  openssl rand -hex 32 >"$keys/cosign.password"
  (cd "$keys" && COSIGN_PASSWORD="$(cat cosign.password)" "$COSIGN" generate-key-pair >/dev/null 2>&1)
  chmod 0600 "$keys/cosign.key" "$keys/cosign.password"
  chmod 0644 "$keys/cosign.pub"
fi

if [[ ! -f "$keys/vm_ed25519" ]]; then
  log "generating lab VM SSH key"
  ssh-keygen -q -t ed25519 -N '' -C basalt-lab-vm -f "$keys/vm_ed25519"
fi

{
  cat "$keys/vm_ed25519.pub"
  if [[ -n "${EXTRA_SSH_PUBKEYS:-}" ]]; then
    for f in $EXTRA_SSH_PUBKEYS; do cat "$f"; done
  fi
} >"$keys/authorized_keys"
chmod 0644 "$keys/authorized_keys"

ls -l "$keys" | sed 1d
