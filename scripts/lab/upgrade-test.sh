#!/usr/bin/env bash
# Fedora release upgrade of a lab VM with dnf system-upgrade.
#
#   VM_NAME=... VM_HOST=11 scripts/lab/upgrade-test.sh [RELEASE]   default: FEDORA_RELEASE + 1
#
# Needs the Basalt packages for RELEASE in the repository
# (FEDORA_RELEASE=RELEASE make rpms repo). Downloads the new release, reboots
# into the offline upgrade, and checks: the system identifies as Basalt OS on
# the new base (basalt-release for RELEASE, $releasever), the offline
# transaction was bracketed by snapshots, packages installed before the
# upgrade are still there, SELinux denials and failed units after the
# upgrade, and the snapshot boot menu.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
to="${1:-$((FEDORA_RELEASE + 1))}"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }

log "before: $(vm '. /etc/os-release; echo "$PRETTY_NAME"'), $(vm 'rpm -q basalt-release kernel | tr "\n" " "')"
vm 'rpm -q htop >/dev/null 2>&1 || dnf -y -q install htop'
log "test package installed before the upgrade: $(vm 'rpm -q htop')"
vm 'snapper list | tail -2'

log "dnf system-upgrade download --releasever=$to"
t0=$(date +%s)
vm "dnf -y system-upgrade download --releasever=$to" | grep -vE '^\[|^ ' | tail -8
t1=$(date +%s)
log "TIMING download and test transaction: $((t1 - t0))s"

log "dnf offline reboot (offline upgrade, then reboot)"
vm 'dnf -y offline reboot' || true
sleep 30
"$L/vm.sh" wait-ssh 3600
t2=$(date +%s)
log "TIMING reboot, offline upgrade, reboot to SSH: $((t2 - t1))s"

vm 'set -e
. /etc/os-release; echo "os: $PRETTY_NAME"
echo "base: fedora macro $(rpm -E %fedora), releasever $(dnf --dump-variables 2>/dev/null | sed -n "s/^releasever = //p")"
rpm -q basalt-release basalt-release-server basalt-snapshots basalt-logos htop
echo "kernel: $(uname -r)"
echo "fedora-release installed: $(rpm -q fedora-release-common >/dev/null 2>&1 && echo yes || echo no)"
snapper list | tail -4
basalt-snapshot-boot list | head -3
dnf -q repolist
echo "failed units: $(systemctl --failed --no-legend | wc -l)"; systemctl --failed --no-legend || true
'
log "denials after the upgrade (this boot): $(denials)"
log "upgrade test finished"
