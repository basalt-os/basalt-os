#!/usr/bin/env bash
# TPM2 / Secure Boot unlock test on the installed lab VM.
#
#   scripts/lab/sb-test.sh
#
# For each change of the Secure Boot state (SecureBootEnable=false, then an
# extra certificate in db), expect: the TPM refuses to unseal (PCR 7 changed),
# the boot stops at the recovery prompt, the recovery key unlocks it. After
# each case the original variable store is restored and an unattended unlock
# is expected again.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
keyfile="$LAB_DIR/recovery/$VM_NAME.txt"
[[ -f "$keyfile" ]] || die "no recovery key at $keyfile"
VIRSH="virsh -c qemu:///system"
vm() { "$L/vm.sh" ssh "$@"; }

tpm_failures() { vm 'journalctl -b --no-pager | grep -c "TPM policy does not match" || true'; }

expect_auto_unlock() {
  local t0; t0=$(date +%s)
  run $VIRSH start "$VM_NAME" >/dev/null
  "$L/vm.sh" wait-ssh
  log "booted unattended in $(($(date +%s) - t0))s; $(vm 'mokutil --sb-state | head -1'); TPM failures this boot: $(tpm_failures)"
  [[ "$(tpm_failures)" == 0 ]] || die "TPM2 unlock was expected to succeed"
}

expect_recovery_prompt() {
  local case="$1"
  run $VIRSH start "$VM_NAME" >/dev/null
  "$L/console-unlock.py" "$VM_NAME" --timeout 180 || die "$case: no recovery prompt"
  if "$L/vm.sh" ssh true 2>/dev/null; then die "$case: system is reachable without the recovery key"; fi
  log "$case: boot is blocked at the recovery prompt; typing the recovery key"
  "$L/console-unlock.py" "$VM_NAME" --now --key-file "$keyfile" --timeout 180 || die "$case: recovery key did not unlock"
  "$L/vm.sh" wait-ssh
  log "$case: booted with the recovery key; $(vm 'mokutil --sb-state | head -1'); TPM failures this boot: $(tpm_failures)"
}

"$L/vm.sh" stop
"$L/vm.sh" sb backup

log "case 1: Secure Boot disabled in the firmware"
"$L/vm.sh" sb disable >/dev/null
expect_recovery_prompt "Secure Boot disabled"
"$L/vm.sh" stop
"$L/vm.sh" sb restore
expect_auto_unlock

log "case 2: a foreign certificate enrolled in db"
"$L/vm.sh" stop
"$L/vm.sh" sb foreign-db >/dev/null
expect_recovery_prompt "foreign db certificate"
"$L/vm.sh" stop
"$L/vm.sh" sb restore
expect_auto_unlock
log "Secure Boot state test passed"
