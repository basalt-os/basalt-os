#!/usr/bin/env bash
# Unattended install of the lab ISO into a new VM, then the first boot.
#
#   scripts/lab/install.sh [ISO]     default: $BUILD_DIR/iso/basalt-os-<version>-<arch>-<site>.iso
#
# Steps, each timed: network and repository server up, VM created with
# Secure Boot firmware and a fresh vTPM, kickstart install from the ISO (the
# site file powers the VM off at the end), ISO removed, first boot (TPM2
# unlock) until SSH answers. The recovery key is then moved off the VM into
# $LAB_DIR/recovery/<vm>.txt (0600); only its length is printed.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
: "${SITE_NAME:=site}"
iso="${1:-$BUILD_DIR/iso/basalt-os-$BASALT_VERSION-$ARCH-$SITE_NAME.iso}"
[[ -f "$iso" ]] || die "no ISO at $iso (make iso)"
vm() { "$L/vm.sh" ssh "$@"; }

"$L/vm.sh" net-up
"$L/repo-serve.sh" up

t0=$(date +%s)
"$L/vm.sh" install "$iso"
t1=$(date +%s)
log "TIMING install: $((t1 - t0))s (VM creation to power off)"

virsh -c qemu:///system start "$VM_NAME" >/dev/null
"$L/vm.sh" wait-ssh 600
t2=$(date +%s)
log "TIMING first boot to SSH: $((t2 - t1))s"

install -d -m 0700 "$LAB_DIR/recovery"
if vm test -f /root/basalt-recovery-key.txt; then
  umask 077
  vm cat /root/basalt-recovery-key.txt >"$LAB_DIR/recovery/$VM_NAME.txt"
  vm rm -f /root/basalt-recovery-key.txt /etc/motd.d/basalt-recovery-key
  log "recovery key saved to $LAB_DIR/recovery/$VM_NAME.txt ($(tr -d '\n' <"$LAB_DIR/recovery/$VM_NAME.txt" | wc -c) characters) and removed from the VM"
else
  log "no recovery key on the VM (unencrypted install?)"
fi

vm 'set -e
. /etc/os-release; echo "os: $PRETTY_NAME"
echo "selinux: $(getenforce)"
echo "secure boot: $(mokutil --sb-state 2>/dev/null | head -1)"
echo "release package: $(rpm -q basalt-release) (fedora-release: $(rpm -q fedora-release-common >/dev/null 2>&1 && echo installed || echo absent))"
echo "root: $(findmnt -no SOURCE,OPTIONS /)"
echo "default subvolume: $(btrfs subvolume get-default /)"
lsblk -o NAME,TYPE,FSTYPE,SIZE,MOUNTPOINTS | sed -n "1,20p"
if command -v cryptsetup >/dev/null && blkid -t TYPE=crypto_LUKS -o device >/dev/null; then
  dev=$(blkid -t TYPE=crypto_LUKS -o device | head -1)
  systemd-cryptenroll "$dev"
  cryptsetup luksDump "$dev" | grep -E "Version|tpm2-hash-pcrs|tpm2-pcr-bank|Cipher:" | sort -u
  echo "crypttab: $(cat /etc/crypttab)"
fi
snapper list
echo "denials since boot: $(ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true)"
echo "failed units: $(systemctl --failed --no-legend | wc -l)"
'
