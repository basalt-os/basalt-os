#!/usr/bin/env bash
# Broken updates and rollback, with the lab canary package
# (packages/lab/basalt-canary, published to the separate lab repository).
#
#   scripts/lab/rollback-test.sh
#
# A. From the running system:
#    canary 1 installed and healthy; data written to /home, /var/lib/containers,
#    /var/lib/pgsql, /var/log; canary 2 (a broken update) installed; more data
#    written; `basalt-rollback <pre snapshot of that update>`; reboot. Expect
#    canary 1 back and healthy, all data from before and after the update kept.
# B. From the GRUB menu:
#    canary 3 (breaks sshd at the next boot) installed; data written; reboot:
#    SSH must stay down. Then power-cycle and pick the update's pre snapshot in
#    the "Basalt OS snapshots" GRUB submenu over the serial console. The
#    snapshot boots read-only with SSH working and the data present;
#    `basalt-rollback` keeps it; a normal reboot comes back healthy.
# C. Across a kernel update:
#    roll back to the initial snapshot (first boot, older kernel than the one
#    running after the snapshot test's upgrade). basalt-rollback must make the
#    older kernel the default boot entry; the system boots read-write with SSH.
#    Then roll forward to the read-only copy that rollback kept.
# Every boot unlocks the disk through the TPM (no recovery key typed), and
# SELinux denials are counted after each boot.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
: "${LAB_REPO_URL:?set LAB_REPO_URL}"
VIRSH="virsh -c qemu:///system"
lab_rpms="$RPM_DIR/lab"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
tpm_failures() { vm 'journalctl -b --no-pager | grep -c "TPM policy does not match\|Failed to unseal" || true'; }
newest_pre() { vm "snapper --csvout list --columns number,type | awk -F, '\$2 == \"pre\" {n = \$1} END {print n}'"; }
canary() { vm 'rpm -q basalt-canary; basalt-canary' 2>&1 | tr '\n' ' '; }

publish_canary() {
  "$REPO_ROOT/scripts/repo.sh" --lab remove basalt-canary >/dev/null 2>&1 || true
  "$REPO_ROOT/scripts/repo.sh" --lab publish "$lab_rpms/basalt-canary-$1.0-1.fc$FEDORA_RELEASE.noarch.rpm" 2>&1 | grep -E 'added|signed' || true
}

# Data markers in subvolumes that a rollback must not touch.
write_marker() {
  vm "set -e; for d in /home/basalt-lab /var/lib/containers/basalt-lab /var/lib/pgsql/basalt-lab /var/log/basalt-lab; do
        mkdir -p \$d; echo \"$1 \$(date -u +%FT%TZ)\" > \$d/$1; done"
}
check_markers() {
  vm "set -e; for m in $*; do for d in /home/basalt-lab /var/lib/containers/basalt-lab /var/lib/pgsql/basalt-lab /var/log/basalt-lab; do
        test -s \$d/\$m || { echo \"MISSING \$d/\$m\"; exit 1; }; done; done; echo \"markers present: $*\""
}
boot_report() {
  log "$1: $(vm '. /etc/os-release; echo $PRETTY_NAME'), root $(vm 'findmnt -no OPTIONS / | tr , "\n" | grep -E "^(ro|rw|subvol=)" | tr "\n" " "'), TPM failures $(tpm_failures), denials $(denials), failed units $(vm 'systemctl --failed --no-legend | wc -l')"
}

log "lab repository in the VM"
vm "cat >/etc/yum.repos.d/basalt-lab.repo <<EOF
# Lab-only test fixtures (basalt-canary). Same signing key as the Basalt repository.
[basalt-lab]
name=Basalt OS lab fixtures \\\$releasever
baseurl=$LAB_REPO_URL/lab/\\\$releasever/\\\$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-basalt
metadata_expire=0
EOF"

# --- A: rollback from the running system ------------------------------------------
log "A1. canary 1"
publish_canary 1
vm 'dnf -y install basalt-canary' | tail -2
log "canary: $(canary)"
write_marker A

log "A2. canary 2: a broken update"
publish_canary 2
vm 'dnf -y upgrade --refresh basalt-canary' | tail -2
pre_a="$(newest_pre)"
log "canary: $(canary)  (pre snapshot of this update: $pre_a)"
vm 'basalt-canary' >/dev/null 2>&1 && die "canary 2 should be broken"
write_marker B
log "denials after the broken update: $(denials)"

log "A3. basalt-rollback $pre_a, reboot"
vm "basalt-rollback --yes $pre_a"
publish_canary 1   # the lab repository stops offering the broken version
vm 'systemctl reboot' || true
sleep 10
"$L/vm.sh" wait-ssh 300
boot_report "after rollback A"
log "canary: $(canary)"
vm 'basalt-canary' || die "canary not healthy after rollback A"
check_markers A B
vm 'btrfs subvolume get-default /; snapper list | tail -5'

# --- B: rollback from the GRUB menu ---------------------------------------------------
log "B1. canary 3: breaks sshd at the next boot"
publish_canary 3
vm 'dnf -y upgrade --refresh basalt-canary' | tail -2
pre_b="$(newest_pre)"
log "canary: $(canary)  (pre snapshot of this update: $pre_b)"
write_marker C
menu="$(vm 'basalt-snapshot-boot list')"
printf '%s\n' "$menu" | head -5
idx="$(printf '%s\n' "$menu" | grep -n "^Snapshot $pre_b," | cut -d: -f1)"
[[ -n "$idx" ]] || die "snapshot $pre_b is not in the boot menu"
idx=$((idx - 1))

log "B2. reboot into the broken system: SSH must not come back"
vm 'systemctl reboot' || true
sleep 20
if "$L/vm.sh" wait-ssh 120 2>/dev/null; then die "SSH came back; canary 3 was supposed to break it"; fi
log "SSH is down as expected"

log "B3. power-cycle and boot snapshot $pre_b from the GRUB menu (entry $idx of the submenu)"
$VIRSH shutdown "$VM_NAME" >/dev/null || true
"$L/vm.sh" wait-off 180 || $VIRSH destroy "$VM_NAME"
$VIRSH start "$VM_NAME" >/dev/null
"$L/grub-console.py" "$VM_NAME" --last-then "$idx" --expect "Snapshot $pre_b," || die "could not pick the snapshot in GRUB"
"$L/vm.sh" wait-ssh 300
boot_report "booted snapshot $pre_b from GRUB"
vm "grep -o 'basalt.snapshot=[0-9]*' /proc/cmdline; basalt-snapshot-boot status"
vm 'touch /usr/basalt-write-test 2>&1 && echo "root is WRITABLE (unexpected)" || echo "root is read-only (expected)"'
log "canary: $(canary)"
check_markers A B C
vm 'systemctl --failed --no-legend' || true

log "B4. basalt-rollback (keep the booted snapshot), normal reboot"
vm 'basalt-rollback --yes'
publish_canary 1
vm 'systemctl reboot' || true
sleep 10
"$L/vm.sh" wait-ssh 300
boot_report "after rollback B"
log "canary: $(canary)"
vm 'basalt-canary' || die "canary not healthy after rollback B"
vm 'test ! -e /etc/ssh/sshd_config.d/01-basalt-canary-broken.conf' || die "broken sshd drop-in still present"
check_markers A B C
vm 'btrfs subvolume get-default /; snapper list | tail -4'

# --- C: rollback across a kernel update -------------------------------------------------
init="$(vm "snapper --csvout list --columns number,description | awk -F, '\$2 == \"initial state\" {print \$1; exit}'")"
[[ -n "$init" ]] || die "no initial snapshot"
kinit="$(vm "ls /.snapshots/$init/snapshot/usr/lib/modules | tr '\n' ' '")"
kcur="$(vm 'uname -r')"
log "C1. basalt-rollback $init (initial state, kernel(s) $kinit); running kernel $kcur"
out="$(vm "basalt-rollback --yes $init")"
printf '%s\n' "$out" | tail -5
back="$(printf '%s\n' "$out" | sed -n 's/.*read-only snapshot of .*(Snapshot \([0-9]*\)).*/\1/p')"
vm 'systemctl reboot' || true
sleep 10
"$L/vm.sh" wait-ssh 300
boot_report "after rollback C to the initial state"
kc="$(vm 'uname -r')"
log "kernel now $kc; canary: $(canary)"
[[ " $kinit " == *" $kc "* ]] || die "booted kernel $kc has no modules in the rolled-back root"
vm 'touch /etc/basalt-rw-test && rm /etc/basalt-rw-test && echo "root is writable"'
check_markers A B C
log "C2. roll forward: basalt-rollback $back (the state before C1)"
vm "basalt-rollback --yes $back" | tail -3
vm 'systemctl reboot' || true
sleep 10
"$L/vm.sh" wait-ssh 300
boot_report "after rolling forward"
log "kernel now $(vm 'uname -r'); canary: $(canary)"
[[ "$(vm 'uname -r')" == "$kcur" ]] || die "expected kernel $kcur after rolling forward"
vm 'basalt-canary' || die "canary not healthy after rolling forward"
check_markers A B C
log "rollback test passed"
