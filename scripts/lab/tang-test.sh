#!/usr/bin/env bash
# Network-bound unlock (Clevis + Tang) on an installed lab VM.
#
#   VM_NAME=... VM_HOST=13 scripts/lab/tang-test.sh
#
# For a VM installed with basalt.unlock=tang or tpm2+tang:
# 1. Tang up: the boot unlocks without interaction (Clevis in the initramfs).
# 2. Tang down: the boot stops at the passphrase prompt and the VM is not
#    reachable; when Tang comes back, Clevis retries and the boot continues
#    without anyone typing anything.
# 3. Tang down again: the recovery key typed on the serial console unlocks.
# 4. tpm2+tang only: Secure Boot disabled in the firmware (PCR 7 changes)
#    with Tang up: the TPM half fails, the prompt stays, the recovery key
#    unlocks; the original firmware state restores the unattended unlock.
# The Tang server is the lab container (scripts/lab/tang-serve.sh).
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
VIRSH="virsh -c qemu:///system"
keyfile="$LAB_DIR/recovery/$VM_NAME.txt"
[[ -f "$keyfile" ]] || die "no recovery key at $keyfile"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
report() { log "$1: denials $(denials), failed units $(vm 'systemctl --failed --no-legend | wc -l'), $(vm 'mokutil --sb-state | head -1')"; }

"$L/tang-serve.sh" up >/dev/null
dev="$(vm 'blkid -t TYPE=crypto_LUKS -o device | head -1')"
vm "clevis luks list -d $dev; systemd-cryptenroll $dev; cat /etc/crypttab; grep -o 'rd.neednet=1' /proc/cmdline"
mode="$(vm "clevis luks list -d $dev" | grep -q '"tpm2"\|tpm2 ' && echo tpm2+tang || echo tang)"
log "unlock mode: $mode"
vm 'basalt-tpm status | sed -n "/Tokens:/,\$p"'

log "1. Tang up: unattended boot"
"$L/vm.sh" stop
t0=$(date +%s); $VIRSH start "$VM_NAME" >/dev/null; "$L/vm.sh" wait-ssh 300
log "booted unattended in $(($(date +%s) - t0))s"
report "Tang up"

log "2. Tang down: blocked; Tang back: the boot continues by itself"
"$L/vm.sh" stop
"$L/tang-serve.sh" down >/dev/null
$VIRSH start "$VM_NAME" >/dev/null
"$L/console-unlock.py" "$VM_NAME" --timeout 240 || die "no passphrase prompt with Tang down"
sleep 20
if "$L/vm.sh" ssh true 2>/dev/null; then die "reachable with Tang down"; fi
log "boot blocked at the prompt, not reachable"
t0=$(date +%s); "$L/tang-serve.sh" up >/dev/null
if "$L/vm.sh" wait-ssh 240 2>/dev/null; then
  log "Tang back: unlocked without interaction $(($(date +%s) - t0))s later"
else
  log "Tang back: NO retry within 240s; typing the recovery key"
  "$L/console-unlock.py" "$VM_NAME" --now --key-file "$keyfile" --timeout 240 || die "recovery key did not unlock"
  "$L/vm.sh" wait-ssh 300
fi
report "Tang back"

log "3. Tang down: recovery key on the console"
"$L/vm.sh" stop
"$L/tang-serve.sh" down >/dev/null
$VIRSH start "$VM_NAME" >/dev/null
"$L/console-unlock.py" "$VM_NAME" --timeout 240 || die "no passphrase prompt with Tang down"
"$L/console-unlock.py" "$VM_NAME" --now --key-file "$keyfile" --timeout 240 || die "recovery key did not unlock"
"$L/vm.sh" wait-ssh 300
report "recovery key"
"$L/tang-serve.sh" up >/dev/null

if [[ "$mode" == tpm2+tang ]]; then
  log "4. tpm2+tang: Secure Boot disabled (PCR 7 changed), Tang up"
  "$L/vm.sh" stop
  "$L/vm.sh" sb backup >/dev/null
  "$L/vm.sh" sb disable >/dev/null
  $VIRSH start "$VM_NAME" >/dev/null
  "$L/console-unlock.py" "$VM_NAME" --timeout 240 || die "no prompt with Secure Boot off"
  sleep 30
  if "$L/vm.sh" ssh true 2>/dev/null; then die "reachable with PCR 7 changed (Tang alone unlocked a tpm2+tang volume)"; fi
  log "Tang up but the TPM half refuses: blocked"
  "$L/console-unlock.py" "$VM_NAME" --now --key-file "$keyfile" --timeout 240 || die "recovery key did not unlock"
  "$L/vm.sh" wait-ssh 300
  report "Secure Boot off, recovery key"
  "$L/vm.sh" stop
  "$L/vm.sh" sb restore
  t0=$(date +%s); $VIRSH start "$VM_NAME" >/dev/null; "$L/vm.sh" wait-ssh 300
  log "firmware restored: unattended again in $(($(date +%s) - t0))s"
  report "restored"
fi
log "Tang test passed ($mode)"
