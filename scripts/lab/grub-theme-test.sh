#!/usr/bin/env bash
# Render the Basalt GRUB theme with Fedora's signed shim and GRUB, Secure
# Boot on, without installing a system, and take screenshots.
#
#   scripts/lab/grub-theme-test.sh OUT_DIR [MODE...]
#
# Each MODE gets one boot. WxH: GRUB_GFXMODE=WxH and a screen of that size.
# default@WxH: GRUB_GFXMODE unset (Basalt's list of preferred modes) on a
# screen whose preferred mode is WxH, e.g. default@3840x2160 for a HiDPI
# panel. Default: default@3840x2160 1920x1080 1280x800 1024x768. Each boot:
# a small EFI system partition (QEMU's vvfat on a directory) with shim,
# GRUB and a grub.cfg like an installed system's: the serial console first
# (/etc/default/grub of Basalt), the block /etc/grub.d/06_basalt_theme
# writes for that mode, kernel
# entries and a snapshot submenu shaped like basalt-snapshot-boot's. The
# theme files come from packages/basalt-logos/tree, so this tests the tree
# as committed. QMP takes the screenshots: the menu with the countdown, the
# countdown stopped, the snapshot submenu (key s), the entry editor (e) and
# the command line (c). The serial log of each boot shows the text menu
# the serial console gets at the same time.
#
# Needs qemu-system-x86_64, edk2-ovmf (Secure Boot variables with the
# Microsoft keys), python3 and podman (Fedora container for the RPMs).
source "$(dirname "$0")/../lib.sh"

out="${1:?usage: grub-theme-test.sh OUT_DIR [MODE...]}"
shift
modes=("$@")
[[ ${#modes[@]} -gt 0 ]] || modes=(default@3840x2160 1920x1080 1280x800 1024x768)
: "${OVMF_CODE:=/usr/share/edk2/ovmf/OVMF_CODE_4M.secboot.qcow2}"
: "${OVMF_VARS:=/usr/share/edk2/ovmf/OVMF_VARS_4M.secboot.qcow2}"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
[[ -n "${KEEP_WORK:-}" ]] || trap 'rm -rf "$work"' EXIT

log "Fedora $FEDORA_RELEASE shim and GRUB, and the theme block for each mode"
mkdir -p "$work/esp/EFI/BOOT" "$work/esp/grub2/themes" "$work/cfg"
cp -r "$REPO_ROOT/packages/basalt-logos/tree/usr/share/basalt/grub2/themes/basalt" "$work/esp/grub2/themes/"
$PODMAN run --rm --network=host --security-opt label=disable \
  -v "$REPO_ROOT/packages/basalt-logos:/pkg:ro" -v "$work:/work" -e MODES="${modes[*]}" \
  "$FEDORA_IMAGE" bash -euc '
    cd /tmp
    dnf -q -y download shim-x64 grub2-efi-x64 >/dev/null 2>&1
    dnf -q -y install cpio >/dev/null 2>&1
    for r in shim-x64-*.rpm grub2-efi-x64-*.rpm; do rpm2cpio "$r" | cpio -idm --quiet; done
    # Fedora 44 keeps the EFI binaries under /usr/lib/efi (older: /boot/efi).
    cp "$(find . -path "*/EFI/BOOT/BOOTX64.EFI" | head -1)" "$(find . -path "*/EFI/fedora/grubx64.efi" | head -1)" /work/esp/EFI/BOOT/
    rpm -qp --qf "%{NAME}-%{VERSION}-%{RELEASE}\n" shim-x64-*.rpm grub2-efi-x64-*.rpm >/work/versions.txt
    mkdir -p /usr/share/basalt/grub2
    cp /pkg/basalt-theme.cfg /usr/share/basalt/grub2/
    cp -r /pkg/tree/usr/share/basalt/grub2/themes /usr/share/basalt/grub2/
    for m in $MODES; do
      case "$m" in
        default@*) GRUB_TERMINAL_OUTPUT="serial console" sh /pkg/06_basalt_theme >"/work/cfg/$m.cfg" ;;
        *) GRUB_TERMINAL_OUTPUT="serial console" GRUB_GFXMODE="$m" sh /pkg/06_basalt_theme >"/work/cfg/$m.cfg" ;;
      esac
    done
    chmod -R a+rwX /work'

# Fedora's EFI stub: find the partition, set prefix, read the real config.
cat >"$work/esp/EFI/BOOT/grub.cfg" <<'EOF'
search --no-floppy --file --set=dev /grub2/grub.cfg
set prefix=($dev)/grub2
export prefix
configfile $prefix/grub.cfg
EOF

menu() { # menu MODE: grub.cfg of an installed Basalt system
  cat <<'EOF'
serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
terminal_input serial console
terminal_output serial console
function load_video {
  insmod all_video
}
set timeout_style=menu
set timeout=60
EOF
  cat "$work/cfg/$1.cfg"
  cat <<'EOF'
menuentry 'Basalt OS (7.2.8-200.fc44.x86_64) 44 (Basalt 0.0.1)' --class basalt --class kernel --id k1 {
  echo 'kernel entry (test)'
}
menuentry 'Basalt OS (6.19.10-300.fc44.x86_64) 44 (Basalt 0.0.1)' --class basalt --class kernel --id k2 {
  echo 'kernel entry (test)'
}
submenu 'Basalt OS snapshots (read-only, key s)' --class basalt-snapshot --class basalt --hotkey=s --id basalt-snapshots {
  menuentry 'Snapshot 14, 2026-10-04 21:12:40 UTC: after dnf upgrade' --class basalt-snapshot --class basalt { true; }
  menuentry 'Snapshot 13, 2026-10-04 21:10:02 UTC, before: dnf upgrade' --class basalt-snapshot --class basalt { true; }
  menuentry 'Snapshot 12, 2026-10-04 18:44:51 UTC: after dnf install tmux nginx' --class basalt-snapshot --class basalt { true; }
  menuentry 'Snapshot 11, 2026-10-04 18:44:20 UTC, before: dnf install tmux nginx' --class basalt-snapshot --class basalt { true; }
  menuentry 'Snapshot 2, 2026-10-03 09:02:11 UTC: after dnf install basalt-assistant' --class basalt-snapshot --class basalt { true; }
  menuentry 'Snapshot 1, 2026-10-03 08:58:37 UTC: first boot' --class basalt-snapshot --class basalt { true; }
}
menuentry 'UEFI Firmware Settings' --id uefi-firmware {
  fwsetup
}
EOF
}

qmp() { # qmp SOCK JSON...: run QMP commands in order
  python3 - "$@" <<'EOF'
import json, socket, sys
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
f = s.makefile("rw")
f.readline()
def cmd(c):
    f.write(json.dumps(c) + "\n"); f.flush()
    while True:
        r = json.loads(f.readline())
        if "return" in r or "error" in r:
            return r
cmd({"execute": "qmp_capabilities"})
for a in sys.argv[2:]:
    r = cmd(json.loads(a))
    if "error" in r:
        print(r, file=sys.stderr)
EOF
}

shot() { qmp "$1" "{\"execute\":\"screendump\",\"arguments\":{\"filename\":\"$2\",\"format\":\"png\"}}"; }
key() { qmp "$1" "{\"execute\":\"human-monitor-command\",\"arguments\":{\"command-line\":\"sendkey $2\"}}"; }

for m in "${modes[@]}"; do
  log "mode $m"
  menu "$m" >"$work/esp/grub2/grub.cfg"
  cp "$OVMF_VARS" "$work/vars.qcow2"
  sock="$work/qmp.sock"
  res="${m#default@}"
  w="${res%x*}" h="${res#*x}"
  # QEMU options are comma separated by design.
  # shellcheck disable=SC2054
  qemu-system-x86_64 -machine q35,smm=on,accel=kvm -cpu host -m 1024 \
    -global driver=cfi.pflash01,property=secure,value=on \
    -blockdev "node-name=code,driver=qcow2,read-only=on,file.driver=file,file.filename=$OVMF_CODE" \
    -blockdev "node-name=vars,driver=qcow2,file.driver=file,file.filename=$work/vars.qcow2" \
    -machine pflash0=code,pflash1=vars \
    -drive "format=raw,file=fat:$work/esp,if=virtio,readonly=on" \
    -device "VGA,xres=$w,yres=$h,vgamem_mb=64" -display none -net none \
    -serial "file:$out/serial-$m.log" -qmp "unix:$sock,server=on,wait=off" &
  pid=$!
  for _ in $(seq 1 120); do grep -q "UEFI Firmware Settings" "$out/serial-$m.log" 2>/dev/null && break; sleep 0.5; done
  sleep 3
  shot "$sock" "$out/$m-1-menu.png"
  key "$sock" down; sleep 1; key "$sock" up; sleep 1
  shot "$sock" "$out/$m-2-stopped.png"
  key "$sock" s; sleep 2
  shot "$sock" "$out/$m-3-snapshots.png"
  key "$sock" esc; sleep 1; key "$sock" e; sleep 2
  shot "$sock" "$out/$m-4-edit.png"
  key "$sock" esc; sleep 1; key "$sock" c; sleep 2
  shot "$sock" "$out/$m-5-command.png"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
done
cp "$work/versions.txt" "$out/versions.txt"
mokutil_note="Secure Boot: OVMF $(basename "$OVMF_CODE") with $(basename "$OVMF_VARS")"
echo "$mokutil_note" >>"$out/versions.txt"
ls -l "$out"
