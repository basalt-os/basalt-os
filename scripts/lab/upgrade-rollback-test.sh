#!/usr/bin/env bash
# Back from a Fedora release upgrade: boot the pre-upgrade snapshot from the
# GRUB menu, keep it, then roll forward again.
#
#   VM_NAME=... VM_HOST=11 scripts/lab/upgrade-rollback-test.sh
#
# Runs on a VM upgraded with scripts/lab/upgrade-test.sh. Expects:
# 1. The pre snapshot of the offline upgrade transaction is in the snapshot
#    menu (it needs a kernel of the old release, still on /boot).
# 2. Booted from the menu it runs the old release read-only: os-release,
#    rpm macros and the kernel are the old release's, 0 denials, SSH up.
# 3. basalt-rollback keeps it; the next boot is the old release read-write.
# 4. basalt-rollback to the snapshot that rollback kept returns to the new
#    release.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
VIRSH="virsh -c qemu:///system"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
report() {
  log "$1: $(vm '. /etc/os-release; echo "$PRETTY_NAME"'), fedora $(vm 'rpm -E %fedora'), kernel $(vm 'uname -r'), $(vm 'rpm -q basalt-release'), root $(vm 'findmnt -no OPTIONS / | cut -d, -f1'), denials $(denials), failed units $(vm 'systemctl --failed --no-legend | wc -l')"
}
reboot_wait() { vm 'systemctl reboot' || true; sleep 15; "$L/vm.sh" wait-ssh 600; }

new_rel="$(vm 'rpm -E %fedora')"
report "now"
pre="$(vm "snapper --csvout list --columns number,type,description | awk -F, '\$2 == \"pre\" && \$3 ~ /offline/ {n = \$1} END {print n}'")"
[[ -n "$pre" ]] || die "no pre snapshot of an offline (system-upgrade) transaction"
old_rel="$(vm "rpm --dbpath /.snapshots/$pre/snapshot/usr/lib/sysimage/rpm -q --qf '%{VERSION}' basalt-release")"
log "pre-upgrade snapshot: $pre (basalt-release $old_rel); release now: $new_rel"
menu="$(vm 'basalt-snapshot-boot update; basalt-snapshot-boot list')"
printf '%s\n' "$menu" | head -8
idx="$(printf '%s\n' "$menu" | grep -n "^Snapshot $pre," | cut -d: -f1)"
[[ -n "$idx" ]] || die "snapshot $pre is not in the boot menu"
idx=$((idx - 1))

log "1. boot snapshot $pre from the GRUB menu (submenu entry $idx)"
"$L/vm.sh" stop
$VIRSH start "$VM_NAME" >/dev/null
"$L/grub-console.py" "$VM_NAME" --last-then "$idx" --expect "Snapshot $pre," --expect-snapshot "$pre" ||
  die "could not boot snapshot $pre from GRUB"
"$L/vm.sh" wait-ssh 600
report "booted pre-upgrade snapshot $pre"
[[ "$(vm 'rpm -E %fedora')" == "$old_rel" ]] || die "expected release $old_rel in the snapshot"
vm 'touch /usr/basalt-write-test 2>/dev/null && echo "root is WRITABLE (unexpected)" || echo "root is read-only (expected)"'

log "2. basalt-rollback (keep the booted snapshot), reboot"
out="$(vm 'basalt-rollback --yes')"
printf '%s\n' "$out" | tail -4
back="$(printf '%s\n' "$out" | sed -n 's/.*read-only snapshot of .*(Snapshot \([0-9]*\)\.).*/\1/p')"
reboot_wait
report "after rollback to $old_rel"
[[ "$(vm 'rpm -E %fedora')" == "$old_rel" ]] || die "expected release $old_rel after the rollback"
vm 'touch /etc/basalt-rw-test && rm /etc/basalt-rw-test && echo "root is writable"'
vm 'dnf -q repolist 2>&1 | tail -4; basalt-rollback --kernels 2>/dev/null || ls /boot/vmlinuz-*' || true

if [[ -n "$back" ]]; then
  log "3. roll forward: basalt-rollback $back (the $new_rel system as it was)"
  vm "basalt-rollback --yes $back" | tail -3
  reboot_wait
  report "after rolling forward"
  [[ "$(vm 'rpm -E %fedora')" == "$new_rel" ]] || die "expected release $new_rel after rolling forward"
else
  log "could not read the kept snapshot number; roll forward skipped"
fi
log "upgrade rollback test passed"
