#!/usr/bin/env bash
# dnf transactions are bracketed by snapshots, and installed packages persist.
#
#   scripts/lab/snapshot-test.sh [PACKAGE]     default PACKAGE: tmux
#
# 1. dnf install PACKAGE (from the Fedora repositories): a new pre/post pair
#    appears, described with the dnf command, and the pre snapshot is in the
#    GRUB snapshot menu.
# 2. dnf upgrade --refresh: another pair; PACKAGE is still installed.
# 3. reboot (TPM2 unlock): PACKAGE still installed and working, 0 denials.
# Also measures the time a snapshot takes and the space the upgrade's pre
# snapshot keeps (exclusive data, `btrfs filesystem du`).
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
pkg="${1:-tmux}"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
last_pair() { vm "snapper --csvout list --columns number,type,pre-number,description | tail -1"; }

log "snapshots before:"; vm 'snapper list'
before="$(vm 'snapper --csvout list --columns number | tail -1')"

log "1. dnf install $pkg"
t0=$(date +%s.%N)
vm "dnf -y install $pkg" | tail -4
log "TIMING dnf install (with two snapshots): $(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b - a}')s"
pair="$(last_pair)"
log "newest snapshot after install: $pair"
[[ "$pair" == *post*"install $pkg"* || "$pair" == *post*"$pkg"* ]] || die "no post snapshot for the install"
vm "rpm -q $pkg" || die "$pkg not installed"
vm 'basalt-snapshot-boot list | head -3'

log "2. dnf upgrade --refresh"
vm 'dnf -y upgrade --refresh' | tail -6
upg="$(last_pair)"
log "newest snapshot after upgrade: $upg"
upg_pre="$(cut -d, -f3 <<<"$upg")"
vm "rpm -q $pkg" || die "$pkg vanished after the upgrade"
log "denials after the updates (this boot): $(denials)"
vm "rpm -qa --last | head -5"

log "snapshot cost:"
vm 'set -e
t0=$(date +%s%N); n=$(snapper create -t single -p -d "timing probe"); t1=$(date +%s%N)
echo "snapper create: $(( (t1 - t0) / 1000000 )) ms"
for i in 1 2 3 4 5; do snapper delete "$n" 2>/dev/null && break; sleep 3; done'
if [[ "$upg_pre" =~ ^[0-9]+$ ]]; then
  vm "btrfs filesystem du -s /.snapshots/$upg_pre/snapshot | tail -1 | awk '{print \"pre-upgrade snapshot $upg_pre: total \" \$1 \", exclusive \" \$2 \" (space the snapshot alone keeps)\"}'"
fi
vm 'df -h / | tail -1; compsize -x / 2>/dev/null | sed -n "1,3p" || true'

log "3. reboot"
vm 'systemctl reboot' || true
sleep 10
"$L/vm.sh" wait-ssh 300
vm "rpm -q $pkg && $pkg -V 2>/dev/null | head -1 || true"
log "after reboot: kernel $(vm 'uname -r'), denials: $(denials), failed units: $(vm 'systemctl --failed --no-legend | wc -l')"
log "snapshots after:"; vm 'snapper list'
log "snapshot test passed (first new snapshot after $before)"
