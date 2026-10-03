#!/usr/bin/env bash
# Lab tools that are not installed on the host: a small Fedora tool image
# (localhost/basalt-lab-tools) with virt-fw-vars (virt-firmware), used to
# change the Secure Boot state in a VM's OVMF variable store.
source "$(dirname "$0")/../lib.sh"

if ! $PODMAN image exists localhost/basalt-lab-tools; then
  log "building localhost/basalt-lab-tools (virt-firmware)"
  printf 'FROM registry.fedoraproject.org/fedora:44\nRUN dnf -y install python3-virt-firmware openssl && dnf clean all\n' |
    $PODMAN build --network=host -q -t localhost/basalt-lab-tools -f - >/dev/null
fi
$PODMAN run --rm localhost/basalt-lab-tools virt-fw-vars --help >/dev/null && echo "virt-fw-vars ok"
