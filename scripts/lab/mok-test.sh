#!/usr/bin/env bash
# MOK mode: Basalt's kernel module CA enrolled through Fedora's shim, and
# which kernel modules the kernel then accepts.
#
#   scripts/lab/mok-test.sh [--sb-off]
#
# On an installed lab VM with the lab basalt-security build (module CA and
# signing certificate from scripts/lab/sb-keys.sh):
# 1. The test module (packages/lab/kmodtest) is built for the VM's kernel
#    and signed four ways (scripts/lab/kmod-build.sh); with Secure Boot on
#    and before enrollment every variant is refused.
# 2. `basalt-secureboot enroll-mok` queues the CA; at the next boot
#    MokManager is driven over the serial console (mok-console.py) with the
#    one-time password. PCRs 4, 7 and 14 are recorded before and after; the
#    disk must still unlock without interaction.
# 3. After enrollment: unsigned and foreign-signed modules are refused; the
#    module signed by the CA and the one signed by the signing certificate
#    (loaded at boot by basalt-module-keys.service) load.
# --sb-off also boots once with Secure Boot disabled in the firmware (the
# TPM refuses, the recovery key is typed on the console) to show that
# module.sig_enforce=1 and lockdown=integrity on the command line keep
# unsigned modules out without Secure Boot. The firmware is restored after.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
VIRSH="virsh -c qemu:///system"
sboff=0; [[ "${1:-}" == --sb-off ]] && sboff=1
vm() { "$L/vm.sh" ssh "$@"; }
tpm_failures() { vm 'journalctl -b --no-pager | grep -c "TPM policy does not match\|Failed to unseal" || true'; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
pcrs() { vm 'systemd-analyze pcrs 4 7 14' | awk 'NR > 1 {printf "PCR %s %s  ", $1, substr($3, 1, 16)}'; }
matrix() {
  vm 'for m in unsigned foreign ca signed; do
        if out=$(insmod /root/kmod/$m.ko 2>&1); then echo "  $m.ko: LOADED"; rmmod basalt_kmodtest
        else echo "  $m.ko: refused (${out##*: })"; fi
      done'
}
# check_matrix "EXPECTED": four words, LOADED or refused, for unsigned foreign ca signed.
check_matrix() {
  local out got; out="$(matrix)"; printf '%s\n' "$out"
  got="$(awk '{print $2}' <<<"$out" | paste -sd' ' -)"
  [[ "$got" == "$1" ]] || die "module matrix: expected '$1', got '$got'"
}

kver="$(vm 'uname -r')"
log "kernel $kver; $(vm 'mokutil --sb-state | head -1'); lockdown $(vm 'cat /sys/kernel/security/lockdown'); sig_enforce $(vm 'cat /sys/module/module/parameters/sig_enforce')"
"$L/kmod-build.sh" "$kver" >/dev/null
vm 'mkdir -p /root/kmod'
for m in unsigned foreign ca signed; do "$L/vm.sh" scp-to "$LAB_DIR/kmod/$kver/$m.ko" "/root/kmod/$m.ko"; done
vm 'basalt-secureboot status'

log "1. before enrollment (Secure Boot on)"
check_matrix "refused refused refused refused"
before="$(pcrs)"

log "2. basalt-secureboot enroll-mok, MokManager at the next boot"
umask 077
pw="$(mktemp "$LAB_DIR/sb-keys/mok-onetime.XXXXXX")"
openssl rand -hex 8 >"$pw"
"$L/vm.sh" scp-to "$pw" /run/basalt-mok-pw >/dev/null
vm 'basalt-secureboot enroll-mok --yes --password-file=/run/basalt-mok-pw; shred -u /run/basalt-mok-pw' | tail -2
vm 'systemctl reboot' || true
sleep 2
"$L/mok-console.py" "$VM_NAME" --password-file "$pw" --timeout 240 || die "MokManager confirmation failed"
shred -u "$pw"
sleep 5
"$L/vm.sh" wait-ssh 300
after="$(pcrs)"
log "PCRs before: $before"
log "PCRs after:  $after"
log "TPM failures this boot: $(tpm_failures); denials: $(denials)"
[[ "$(tpm_failures)" == 0 ]] || die "the TPM did not unlock after the MOK enrollment"
vm 'basalt-secureboot status | sed -n "/MOKs enrolled/,\$p"'
vm 'journalctl -b -u basalt-module-keys.service --no-pager -o cat | tail -3'

log "3. after enrollment (Secure Boot on)"
check_matrix "refused refused LOADED LOADED"

if [[ $sboff == 1 ]]; then
  log "4. Secure Boot disabled in the firmware (lockdown and sig_enforce from the command line)"
  keyfile="$LAB_DIR/recovery/$VM_NAME.txt"
  "$L/vm.sh" stop
  "$L/vm.sh" sb backup >/dev/null
  "$L/vm.sh" sb disable >/dev/null
  $VIRSH start "$VM_NAME" >/dev/null
  "$L/console-unlock.py" "$VM_NAME" --timeout 240 || die "no recovery prompt with Secure Boot off"
  "$L/console-unlock.py" "$VM_NAME" --now --key-file "$keyfile" --timeout 240 || die "recovery key did not unlock"
  "$L/vm.sh" wait-ssh 300
  log "$(vm 'mokutil --sb-state | head -1'); lockdown $(vm 'cat /sys/kernel/security/lockdown'); sig_enforce $(vm 'cat /sys/module/module/parameters/sig_enforce'); denials $(denials)"
  vm 'journalctl -b -u basalt-module-keys.service --no-pager -o cat | tail -2'
  out="$(matrix)"; printf '%s\n' "$out"
  grep -q 'unsigned.ko: refused' <<<"$out" || die "an unsigned module loaded with Secure Boot off"
  "$L/vm.sh" stop
  "$L/vm.sh" sb restore
  $VIRSH start "$VM_NAME" >/dev/null
  "$L/vm.sh" wait-ssh 300
  log "firmware restored: TPM failures $(tpm_failures), $(vm 'mokutil --sb-state | head -1')"
fi
log "denials this boot: $(denials); failed units: $(vm 'systemctl --failed --no-legend | wc -l')"
log "MOK test passed"
