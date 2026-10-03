#!/usr/bin/env bash
# Fetch pinned lab tools into $LAB_DIR/bin, verified against the upstream
# checksums, without touching the host's package set.
#   cosign        image signing (v2 line: writes the sigstore attachment
#                 format that containers-policy.json "sigstoreSigned" verifies)
#   virt-fw-vars  edit OVMF variable stores (virt-firmware, packaged in a
#                 small tool image: localhost/basalt-lab-tools)
source "$(dirname "$0")/../lib.sh"

: "${COSIGN_VERSION:=v2.6.5}"
bin="$LAB_DIR/bin"
install -d -m 0755 "$bin"

if [[ ! -x "$bin/cosign" ]] || ! "$bin/cosign" version 2>/dev/null | grep -q "$COSIGN_VERSION"; then
  log "fetching cosign $COSIGN_VERSION"
  tmp="$(mktemp -d)"
  base="https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION"
  curl -fsSL -o "$tmp/cosign-linux-amd64" "$base/cosign-linux-amd64"
  curl -fsSL -o "$tmp/cosign_checksums.txt" "$base/cosign_checksums.txt"
  (cd "$tmp" && grep ' cosign-linux-amd64$' cosign_checksums.txt | sha256sum -c -)
  install -m 0755 "$tmp/cosign-linux-amd64" "$bin/cosign"
  rm -rf "$tmp"
fi

if ! $PODMAN image exists localhost/basalt-lab-tools; then
  log "building localhost/basalt-lab-tools (virt-firmware)"
  printf 'FROM registry.fedoraproject.org/fedora:44\nRUN dnf -y install python3-virt-firmware openssl && dnf clean all\n' |
    $PODMAN build --network=host -q -t localhost/basalt-lab-tools -f - >/dev/null
fi

"$bin/cosign" version 2>/dev/null | grep GitVersion
$PODMAN run --rm localhost/basalt-lab-tools virt-fw-vars --help >/dev/null && echo "virt-fw-vars ok"
