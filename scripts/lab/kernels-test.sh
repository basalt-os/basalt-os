#!/usr/bin/env bash
# Kernels left on /boot by a rollback: detection and safe cleanup.
#
#   scripts/lab/kernels-test.sh
#
# On a lab VM that went through a kernel update (snapshot-test.sh):
# 1. basalt-rollback to the initial snapshot (older kernel only), reboot.
#    The newer kernel stays on /boot. While snapshots hold its modules it is
#    "kept", and --clean-kernels removes nothing.
# 2. The snapshots holding it are deleted (as the retention policy would in
#    time): it is now "orphaned"; the next dnf transaction reports it.
# 3. basalt-rollback --clean-kernels --yes removes it (kernel image,
#    initramfs, boot entry); the default boot entry is a kernel the package
#    database owns; the reboot is healthy.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
reboot_wait() { vm 'systemctl reboot' || true; sleep 15; "$L/vm.sh" wait-ssh 300; }

init="$(vm "snapper --csvout list --columns number,description | awk -F, '\$2 == \"initial state\" {print \$1; exit}'")"
[[ -n "$init" ]] || die "no initial snapshot"
kinit="$(vm "ls /.snapshots/$init/snapshot/usr/lib/modules")"
knew="$(vm 'uname -r')"
[[ "$kinit" != *"$knew"* ]] || die "the running kernel $knew is also in the initial snapshot; run snapshot-test.sh first"
log "running kernel $knew; initial snapshot $init has $kinit"

log "1. basalt-rollback $init, reboot"
vm "basalt-rollback --yes $init" | tail -4
reboot_wait
log "kernel now $(vm 'uname -r'), denials $(denials)"
vm 'basalt-rollback --kernels'
vm 'basalt-rollback --clean-kernels --yes'
vm "test -e /boot/vmlinuz-$knew" || die "a kept kernel was removed"

log "2. delete the snapshots that hold $knew"
holders="$(vm "ls -d /.snapshots/*/snapshot/usr/lib/modules/$knew 2>/dev/null | cut -d/ -f3 | sort -n | paste -sd' ' -")"
log "snapshots with its modules: $holders"
for n in $holders; do vm "snapper delete $n" || die "could not delete snapshot $n"; done
vm 'basalt-rollback --kernels'
[[ "$(vm 'basalt-rollback --list-orphans')" == "$knew" ]] || die "$knew is not reported as orphaned"
log "a dnf transaction reports it:"
vm 'dnf -y install tree 2>&1 | grep -i "orphaned" || true'

log "3. basalt-rollback --clean-kernels --yes"
vm 'basalt-rollback --clean-kernels --yes'
vm "test ! -e /boot/vmlinuz-$knew && ! ls /boot/loader/entries/*$knew.conf >/dev/null 2>&1" || die "$knew still on /boot"
log "default boot entry: $(vm 'grubby --default-kernel')"
vm 'basalt-snapshot-boot list | head -3'
reboot_wait
log "after reboot: kernel $(vm 'uname -r'), denials $(denials), failed units $(vm 'systemctl --failed --no-legend | wc -l'), orphans: $(vm 'basalt-rollback --list-orphans | wc -l')"
log "kernels test passed"
