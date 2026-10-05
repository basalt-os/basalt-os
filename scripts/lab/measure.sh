#!/usr/bin/env bash
# Collect milestone measurements from the build host and a lab VM.
#   scripts/lab/measure.sh [label]     prints a plain-text report to stdout
# ISO size, installed size (packages, file system use, compression), boot
# time, idle memory, SELinux denials, snapshots and the space they keep.
source "$(dirname "$0")/../lib.sh"
: "${SITE_NAME:=site}"
: "${IDLE_SECONDS:=120}"
label="${1:-snapshot}"
vm() { "$REPO_ROOT/scripts/lab/vm.sh" ssh "$@"; }

echo "== measurements: $label ($(date -u +%Y-%m-%dT%H:%M:%SZ))"
iso="$BUILD_DIR/iso/$(iso_name server "netinst-$SITE_NAME").iso"
if [[ -f "$iso" ]]; then
  echo "-- ISO"
  echo "$(basename "$iso"): $(( $(stat -c %s "$iso") / 1048576 )) MiB"
  base="$(cat "${ISO_CACHE:-$BUILD_DIR/iso-cache}/latest" 2>/dev/null || true)"
  [[ -n "$base" ]] && echo "base $base: $(( $(stat -c %s "${ISO_CACHE:-$BUILD_DIR/iso-cache}/$base") / 1048576 )) MiB"
  echo "Basalt repository on the media: $(du -sk "$REPO_DIR/$FEDORA_RELEASE/$ARCH" | cut -f1) KiB"
fi

echo "-- VM"
vm 'set -e
. /etc/os-release; echo "os: $PRETTY_NAME (ID=$ID, ID_LIKE=$ID_LIKE)"
echo "kernel: $(uname -r)"
echo "selinux: $(getenforce)"
echo "secure boot: $(mokutil --sb-state 2>/dev/null | head -1)"
echo "boot: $(systemd-analyze | head -1)"
echo "uptime: $(cut -d" " -f1 /proc/uptime)s"
echo "packages: $(rpm -qa | wc -l), installed size $(rpm -qa --qf "%{SIZE}\n" | awk "{s+=\$1} END {printf \"%d MiB\", s/1048576}")"
'
up=$(vm "cut -d. -f1 /proc/uptime")
if [[ "$up" -lt "$IDLE_SECONDS" ]]; then
  sleep $((IDLE_SECONDS - up))
fi
vm 'set -e
echo "memory after $(cut -d. -f1 /proc/uptime)s idle (MiB):"
free -m | sed -n "1,2p"
echo "largest resident processes (MiB):"
ps -eo rss=,comm= --sort=-rss | head -5 | awk "{printf \"  %s %d\n\", \$2, \$1/1024}"
echo "denials since boot (AVC, USER_AVC, SELINUX_ERR): $(ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true)"
echo "failed units: $(systemctl --failed --no-legend | wc -l)"
echo "root: $(findmnt -no SOURCE,FSTYPE,OPTIONS /)"
echo "default subvolume: $(btrfs subvolume get-default /)"
echo "luks: $(lsblk -lno NAME,FSTYPE | awk "\$2==\"crypto_LUKS\"{print \$1}")"
df -h / /boot /boot/efi | sed 1d
echo "btrfs (whole file system):"; btrfs filesystem df / | sed "s/^/  /"
echo "compression of the root subvolume:"; compsize -x / 2>/dev/null | sed -n "1,4p" | sed "s/^/  /" || true
echo "snapshots: $(snapper --csvout list --columns number | sed 1d | grep -vcx 0)"
# Space the snapshots keep: disk usage of the live root plus all snapshots
# (shared extents counted once) minus the live root alone. Per-snapshot
# "exclusive" numbers undercount, because snapshots share old extents.
live=$(compsize -b -x / 2>/dev/null | awk "/^TOTAL/{print \$3}")
all=$(compsize -b -x / /.snapshots/*/snapshot 2>/dev/null | awk "/^TOTAL/{print \$3}")
if [ -n "$live" ] && [ -n "$all" ]; then
  echo "space kept by snapshots: $(( (all - live) / 1048576 )) MiB on disk (live root $(( live / 1048576 )) MiB)"
fi
echo "listening sockets:"; ss -Htlnu | awk "{print \"  \" \$1, \$5}" | sort -u
'
