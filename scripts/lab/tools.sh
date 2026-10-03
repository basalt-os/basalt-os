#!/usr/bin/env bash
# Lab tools that are not installed on the host: a small Fedora tool image
# (localhost/basalt-lab-tools) with
#   virt-fw-vars (virt-firmware)   edit a VM's OVMF variable store (Secure Boot keys)
#   sbsign, sbverify (sbsigntools) sign and check EFI binaries
#   efitools, openssl              EFI signature lists, certificates
# The image is rebuilt when a tool is missing (older lab images had only
# virt-fw-vars).
source "$(dirname "$0")/../lib.sh"

IMAGE=localhost/basalt-lab-tools
need="virt-fw-vars sbsign sbverify cert-to-efi-sig-list openssl"

if ! $PODMAN image exists "$IMAGE" ||
   ! $PODMAN run --rm "$IMAGE" bash -c "for t in $need; do command -v \$t >/dev/null || exit 1; done"; then
  log "building $IMAGE"
  printf 'FROM registry.fedoraproject.org/fedora:44\nRUN dnf -y install python3-virt-firmware openssl sbsigntools efitools && dnf clean all\n' |
    $PODMAN build --network=host -q -t "$IMAGE" -f - >/dev/null
fi
$PODMAN run --rm "$IMAGE" bash -c "for t in $need; do command -v \$t >/dev/null || { echo \"missing \$t\"; exit 1; }; done" &&
  echo "lab tools ok ($need)"
