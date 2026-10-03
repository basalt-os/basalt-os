#!/usr/bin/env bash
# Custom db mode: the firmware trusts only Basalt's own Secure Boot keys.
#
#   VM_NAME=... VM_HOST=12 scripts/lab/sb-custom-test.sh
#
# On an installed, encrypted lab VM (TPM2 unlock bound to PCR 7):
# 1. shim (EFI/fedora/shimx64.efi and the fallback EFI/BOOT/BOOTX64.EFI) is
#    signed with the lab db key on the lab host; sbsign appends, so the
#    Microsoft signatures stay. GRUB and the kernel keep Fedora's signatures:
#    shim verifies them with its built-in Fedora certificate.
# 2. `basalt-tpm suspend` adds a TPM2 key bound to no PCR for one boot.
# 3. PK, KEK and db in the VM's variable store are replaced with the lab
#    keys (Microsoft and Red Hat certificates removed, dbx kept).
# 4. Boot: unattended unlock through the suspended key; basalt-tpm-resume
#    re-seals to the new PCR 7 and removes it. Next boot: unattended again.
# 5. Negative control: a boot loader signed only by Microsoft (the stock
#    Fedora netinst ISO) is refused by the firmware.
# 6. Back to the stock keys without suspending first: the TPM refuses (PCR 7
#    changed), the recovery key unlocks on the console, `basalt-tpm reenroll`
#    re-seals, and the next boot is unattended. Then custom keys again with
#    the same recovery flow, so the VM ends in custom db mode.
# PCR 7 is recorded at every step. No key material is printed.
# START_STEP=N resumes at step N (5, 6 or 7) on a VM already in custom db mode.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
: "${ISO_CACHE:=$BUILD_DIR/iso-cache}"
: "${FEDORA_ISO:=$ISO_CACHE/$(cat "$ISO_CACHE/latest" 2>/dev/null)}"
VIRSH="virsh -c qemu:///system"
keys="$LAB_DIR/sb-keys"
keyfile="$LAB_DIR/recovery/$VM_NAME.txt"
[[ -s "$keys/db.key" ]] || die "no lab keys (scripts/lab/sb-keys.sh)"
[[ -f "$keyfile" ]] || die "no recovery key at $keyfile"
vm() { "$L/vm.sh" ssh "$@"; }
pcr7() { vm "systemd-analyze pcrs 7 | awk 'NR == 2 {print \$3}'"; }
tpm_failures() { vm 'journalctl -b --no-pager | grep -c "TPM policy does not match\|Failed to unseal" || true'; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
report() {
  log "$1: PCR 7 $(pcr7 | cut -c1-16), $(vm 'mokutil --sb-state | head -1'), PK: $(vm "mokutil --pk 2>/dev/null | sed -n 's/.*Subject:.*CN=\([^,/]*\).*/\1/p' | head -1"), TPM failures $(tpm_failures), denials $(denials), failed units $(vm 'systemctl --failed --no-legend | wc -l')"
}
auto_boot() {
  local t0; t0=$(date +%s)
  $VIRSH start "$VM_NAME" >/dev/null
  "$L/vm.sh" wait-ssh 300
  log "booted unattended in $(($(date +%s) - t0))s"
}
recovery_boot() {
  $VIRSH start "$VM_NAME" >/dev/null
  "$L/console-unlock.py" "$VM_NAME" --timeout 240 || die "$1: no recovery prompt"
  if "$L/vm.sh" ssh true 2>/dev/null; then die "$1: reachable without the recovery key"; fi
  "$L/console-unlock.py" "$VM_NAME" --now --key-file "$keyfile" --timeout 240 || die "$1: recovery key did not unlock"
  "$L/vm.sh" wait-ssh 300
  log "$1: TPM refused, recovery key typed on the serial console"
}
reenroll_with_recovery_key() {
  # The key goes to a tmpfs file on the VM and is shredded right after.
  "$L/vm.sh" scp-to "$keyfile" /run/basalt-rk >/dev/null
  vm 'chmod 0600 /run/basalt-rk; basalt-tpm reenroll --recovery-key-file=/run/basalt-rk --yes; shred -u /run/basalt-rk' | grep -v '^  \$' | tail -4
}

: "${START_STEP:=1}"
serial_log="/var/log/libvirt/qemu/$VM_NAME-serial.log"
if [[ "$START_STEP" -le 4 ]]; then
report "start (stock keys)"
vm 'basalt-tpm status | sed -n "/Tokens:/,\$p"'

log "1. sign shim with the lab db key"
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
"$L/vm.sh" scp-from /boot/efi/EFI/fedora/shimx64.efi "$work/shimx64.efi"
$PODMAN run --rm --security-opt label=disable -v "$work:/w" -v "$keys:/k:ro" localhost/basalt-lab-tools bash -c '
  sbsign --key /k/db.key --cert /k/db.pem --output /w/shimx64.signed.efi /w/shimx64.efi 2>/dev/null
  sbverify --list /w/shimx64.signed.efi | grep -E "^signature|CN=" | sed "s/^/  /"
  sbverify --cert /k/db.pem /w/shimx64.signed.efi'
sudo chown "$(id -u):$(id -g)" "$work"/*
"$L/vm.sh" scp-to "$work/shimx64.signed.efi" /root/shimx64.signed.efi
vm 'set -e; cd /boot/efi/EFI
for f in fedora/shimx64.efi BOOT/BOOTX64.EFI; do
  [ -f "$f.stock" ] || cp -p "$f" "$f.stock"
  cmp -s fedora/shimx64.efi.stock "$f.stock" || echo "note: $f differs from fedora/shimx64.efi"
  cp /root/shimx64.signed.efi "$f"
done; sync; ls -l fedora/shimx64.efi* BOOT/BOOTX64.EFI*'

log "2. basalt-tpm suspend"
vm 'basalt-tpm suspend --yes' | tail -2
"$L/vm.sh" stop
"$L/vm.sh" sb backup >/dev/null

log "3. custom keys: PK, KEK and db replaced"
"$L/vm.sh" sb custom
"$L/vm.sh" sb show | grep -E 'CN=' | head -6 || true

log "4. boot in custom db mode"
auto_boot
report "custom keys, suspended boot"
vm 'systemctl status basalt-tpm-resume.service --no-pager 2>&1 | sed -n "1,3p"; journalctl -b -u basalt-tpm-resume.service --no-pager -o cat | tail -4'
vm 'basalt-tpm status | sed -n "/Tokens:/,\$p"'
vm 'basalt-secureboot status | sed -n "1,12p"'
vm 'systemctl reboot' || true; sleep 15; "$L/vm.sh" wait-ssh 300
report "custom keys, next boot (re-sealed)"
[[ "$(tpm_failures)" == 0 ]] || die "TPM did not unlock after re-sealing"
fi
if ! "$L/vm.sh" ssh true 2>/dev/null; then $VIRSH start "$VM_NAME" >/dev/null 2>&1 || true; "$L/vm.sh" wait-ssh 300; fi
custom_pcr7="$(pcr7)"

if [[ "$START_STEP" -le 5 ]]; then
log "5. negative control: the stock Fedora netinst ISO (Microsoft-signed shim only)"
"$L/vm.sh" stop
"$L/vm.sh" boot-iso "$FEDORA_ISO"
# The firmware prints its refusal before a console could attach: read the
# serial log libvirt keeps instead.
lines="$(sudo wc -l "$serial_log" | cut -d" " -f1)"
$VIRSH start "$VM_NAME" >/dev/null
"$L/vm.sh" wait-ssh 300
refusal="$(sudo tail -n +"$((lines + 1))" "$serial_log" | tr -d '\r' | grep -a 'DVD-ROM.*Access Denied' | head -1 || true)"
[[ -n "$refusal" ]] || die "no refusal of the stock ISO in the serial log"
log "firmware: ${refusal#*: }"
log "the firmware fell back to the disk: $(vm '. /etc/os-release; echo $PRETTY_NAME')"
"$L/vm.sh" stop
"$L/vm.sh" eject
auto_boot
report "custom keys, after the refused ISO"
fi

log "6. stock keys again, no suspend: recovery key, basalt-tpm reenroll"
"$L/vm.sh" stop
nv="$($VIRSH dumpxml "$VM_NAME" | sed -n 's#.*<nvram[^>]*>\(.*\)</nvram>.*#\1#p')"
sudo cp --preserve=all "$nv" "$LAB_DIR/sb/$VM_NAME/nvram.custom"
"$L/vm.sh" sb restore
recovery_boot "stock keys"
report "stock keys, unlocked with the recovery key"
reenroll_with_recovery_key
vm 'systemctl reboot' || true; sleep 15; "$L/vm.sh" wait-ssh 300
report "stock keys, after reenroll"
[[ "$(tpm_failures)" == 0 ]] || die "TPM did not unlock after reenroll"

log "7. custom keys again (recovery flow), VM stays in custom db mode"
"$L/vm.sh" stop
sudo cp --preserve=all "$LAB_DIR/sb/$VM_NAME/nvram.custom" "$nv"
recovery_boot "custom keys"
reenroll_with_recovery_key
vm 'systemctl reboot' || true; sleep 15; "$L/vm.sh" wait-ssh 300
report "custom keys, final"
[[ "$(tpm_failures)" == 0 ]] || die "TPM did not unlock after reenroll"
[[ "$(pcr7)" == "$custom_pcr7" ]] && log "PCR 7 in custom db mode is stable across boots" || log "note: PCR 7 differs from the earlier custom boot"
log "custom db test passed"
