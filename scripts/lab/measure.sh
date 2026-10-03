#!/usr/bin/env bash
# Collect milestone measurements from the build host and the lab VM.
#   scripts/lab/measure.sh [label]     prints a plain-text report to stdout
# Image size: uncompressed (local storage) and compressed (sum of registry
# layers). VM: boot time, idle memory, SELinux denials since boot, storage.
source "$(dirname "$0")/../lib.sh"
lab_registry_opts

label="${1:-snapshot}"
vm() { "$REPO_ROOT/scripts/lab/vm.sh" ssh "$@"; }
: "${IDLE_SECONDS:=120}"

echo "== measurements: $label ($(date -u +%Y-%m-%dT%H:%M:%SZ))"

echo "-- image $IMAGE:$BASALT_VERSION"
size=$($PODMAN image inspect "$LOCAL_IMAGE:$BASALT_VERSION" --format '{{.Size}}' 2>/dev/null || echo 0)
echo "uncompressed: $((size / 1024 / 1024)) MiB"
base=$($PODMAN image inspect "$BASE_IMAGE" --format '{{.Size}}' 2>/dev/null || echo 0)
echo "base ($BASE_IMAGE) uncompressed: $((base / 1024 / 1024)) MiB"
skopeo inspect --raw "${SKOPEO_REG_OPTS[@]}" "docker://$IMAGE:$BASALT_VERSION" |
  python3 -c 'import json,sys; m=json.load(sys.stdin); n=len(m["layers"]); s=sum(l["size"] for l in m["layers"]); print("compressed (registry layers): %d MiB in %d layers" % (s/1048576, n))'

echo "-- VM"
vm 'set -e
. /etc/os-release; echo "os: $PRETTY_NAME (ID=$ID, ID_LIKE=$ID_LIKE)"
echo "kernel: $(uname -r)"
echo "selinux: $(getenforce)"
echo "secure boot: $(mokutil --sb-state 2>/dev/null | head -1)"
bootc status --format=humanreadable 2>/dev/null | sed -n "1,3p"
echo "boot: $(systemd-analyze | head -1)"
echo "uptime: $(cut -d" " -f1 /proc/uptime)s"
'
up=$(vm "cut -d. -f1 /proc/uptime")
if [[ "$up" -lt "$IDLE_SECONDS" ]]; then
  sleep $((IDLE_SECONDS - up))
fi
vm 'set -e
echo "memory after $(cut -d. -f1 /proc/uptime)s idle (MiB):"
free -m | sed -n "1,2p"
echo "denials since boot (AVC, USER_AVC, SELINUX_ERR): $(ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true)"
echo "failed units: $(systemctl --failed --no-legend | wc -l)"
echo "root: $(findmnt -no SOURCE,FSTYPE,OPTIONS /sysroot)"
echo "luks: $(lsblk -lno NAME,FSTYPE | awk "\$2==\"crypto_LUKS\"{print \$1}")"
compsize -x /sysroot 2>/dev/null | sed -n "1,3p" || true
df -h /sysroot | tail -1
'
