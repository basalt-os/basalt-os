#!/usr/bin/env bash
# End-to-end lab install, from a published image to an encrypted, TPM2-unlocked
# system, with timings. Destroys and recreates the lab VM.
#   scripts/lab/e2e-install.sh
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"

step() {
  local name="$1"; shift
  local t0; t0=$(date +%s)
  log "step: $name"
  "$@"
  log "step: $name done in $(($(date +%s) - t0))s"
}

step "destroy old VM" "$L/vm.sh" destroy
sudo rm -f "/var/log/libvirt/qemu/${VM_NAME:-basalt-lab-vm}-serial.log" "$LAB_DIR/sb/nvram.orig"
step "installer disk" "$L/vm.sh" installer-disk
step "create VM and boot installer" "$L/vm.sh" create
step "basalt-install to target" "$L/install-target.sh"
step "detach installer" "$L/vm.sh" detach-installer
step "first boot of the installed system" "$L/vm.sh" start
step "registry credentials" "$L/vm.sh" registry-auth
