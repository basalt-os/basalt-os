#!/usr/bin/env bash
# Installer lab test, VM side. Runs as root inside a Fedora container with
# /dev/kvm (started by tests/install-test.sh run); do not run it on a host.
#
# Mounts:   /t (this directory, ro), /ci (scripts/ci, ro), /vm/live.iso (the
#           live installer ISO, ro), /repo (signed repository, ro), /keys
#           (lab SSH key, ro), /work (state, logs)
#
# 1. QEMU q35 + KVM, OVMF with Secure Boot on (Microsoft and Red Hat keys
#    enrolled), swtpm TPM 2.0, a blank virtio disk and the live ISO. The live
#    system boots through Fedora's signed shim, GRUB and kernel.
# 2. MODE=tui: tests/drive-tui.py walks the text installer on the serial
#    console like a person: every screen, the review, the typed disk name,
#    the recovery key read off the screen and acknowledged, power off.
#    MODE=gui: tests/drive-gui.py clicks through the graphical installer
#    (QMP pointer and keyboard), taking a screenshot of every page.
# 3. The checks of the kickstart boot test (scripts/ci/vm-test.sh) run on
#    the installed disk: TPM unlock, os-release, profile, Secure Boot,
#    SELinux with 0 denials, LUKS slots and PCR 7, snapper pre/post around
#    dnf, rollback dry run, the assistant enabled and confined, sealed
#    audit rotation, a reboot.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1

: "${MODE:=tui}"
: "${SSH_PORT:=2291}"
: "${REPO_PORT:=8191}"
: "${VM_MEMORY_MB:=4096}"
: "${VM_VCPUS:=4}"
: "${VM_DISK_GB:=30}"
: "${INSTALL_TIMEOUT:=3600}"
: "${HOST_OWNER:=0:0}"

W=/work
S="$W/state"
LOGS="$W/logs"
OVMF_CODE=/usr/share/edk2/ovmf/OVMF_CODE_4M.secboot.qcow2
OVMF_VARS=/usr/share/edk2/ovmf/OVMF_VARS_4M.secboot.qcow2

log() { printf '==> [%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

qemu_pid="" http_pid=""
cleanup() {
  [[ -n "$qemu_pid" ]] && kill "$qemu_pid" 2>/dev/null || true
  [[ -n "$http_pid" ]] && kill "$http_pid" 2>/dev/null || true
  pkill -x swtpm 2>/dev/null || true
  chown -R "$HOST_OWNER" "$W" 2>/dev/null || true
}
trap cleanup EXIT
mkdir -p "$S" "$LOGS"

log "installing QEMU, OVMF, swtpm"
dnf -q -y install qemu-system-x86-core qemu-img edk2-ovmf swtpm swtpm-tools \
  openssh-clients python3 procps-ng >/dev/null 2>&1 || die "dnf install failed"
[[ -c /dev/kvm ]] || die "no /dev/kvm in the container"

cp "$OVMF_VARS" "$S/vars.qcow2"
rm -f "$S/disk.qcow2"
qemu-img create -q -f qcow2 "$S/disk.qcow2" "${VM_DISK_GB}G"
rm -rf "$S/tpm" && mkdir -p "$S/tpm"
swtpm_setup --tpm2 --tpmstate "$S/tpm" --createek --create-ek-cert --create-platform-cert \
  --lock-nvram --pcr-banks sha256 --overwrite >"$LOGS/swtpm-setup.log" 2>&1 ||
  { cat "$LOGS/swtpm-setup.log" >&2; die "swtpm_setup failed"; }
rm -f "$S/tpm.sock"
swtpm socket --tpm2 --tpmstate dir="$S/tpm" --ctrl type=unixio,path="$S/tpm.sock" \
  --log file="$LOGS/swtpm.log",level=1 --terminate --daemon
for _ in $(seq 1 50); do [[ -S "$S/tpm.sock" ]] && break; sleep 0.1; done

log "serving the repository on 127.0.0.1:$REPO_PORT (guest: 10.0.2.2)"
python3 -m http.server "$REPO_PORT" --bind 127.0.0.1 --directory /repo >"$LOGS/http-install.log" 2>&1 &
http_pid=$!

# QEMU options are comma separated by design.
# shellcheck disable=SC2054
args=(
  -name basalt-inst-"$MODE" -machine q35,smm=on,accel=kvm -cpu host -smp "$VM_VCPUS" -m "$VM_MEMORY_MB"
  -global driver=cfi.pflash01,property=secure,value=on -global ICH9-LPC.disable_s3=1
  -blockdev "node-name=fw-code,driver=qcow2,read-only=on,file.driver=file,file.filename=$OVMF_CODE"
  -blockdev "node-name=fw-vars,driver=qcow2,file.driver=file,file.filename=$S/vars.qcow2"
  -machine pflash0=fw-code,pflash1=fw-vars
  -chardev "socket,id=chrtpm,path=$S/tpm.sock" -tpmdev emulator,id=tpm0,chardev=chrtpm -device tpm-crb,tpmdev=tpm0
  -drive "if=none,id=disk0,file=$S/disk.qcow2,format=qcow2,cache=unsafe,discard=unmap"
  -device virtio-blk-pci,drive=disk0,serial=basalt-disk,bootindex=2
  -drive "if=none,id=cd0,media=cdrom,readonly=on,file=/vm/live.iso" -device ide-cd,bus=ide.0,drive=cd0,bootindex=1
  -netdev user,id=net0 -device virtio-net-pci,netdev=net0
  -object rng-random,id=rng0,filename=/dev/urandom -device virtio-rng-pci,rng=rng0
  -chardev "socket,id=ser0,path=$S/serial.sock,server=on,wait=off,logfile=$LOGS/serial-install.log"
  -serial chardev:ser0
  -chardev "socket,id=ser1,path=$S/shell.sock,server=on,wait=off,logfile=$LOGS/serial-shell.log"
  -serial chardev:ser1
  -qmp "unix:$S/qmp.sock,server=on,wait=off"
  -display none -monitor none
)
if [[ "$MODE" == gui || "$MODE" == hold ]]; then
  args+=(-vga std -device qemu-xhci -device usb-tablet)
else
  args+=(-vga none)
fi

log "booting the live installer ($MODE): ${VM_VCPUS} vCPU, ${VM_MEMORY_MB} MiB, ${VM_DISK_GB} GiB disk"
t0=$(date +%s)
qemu-system-x86_64 "${args[@]}" >"$LOGS/qemu-install.log" 2>&1 &
qemu_pid=$!
sleep 2
kill -0 "$qemu_pid" 2>/dev/null || { cat "$LOGS/qemu-install.log" >&2; die "QEMU did not start"; }

rc=0
if [[ "$MODE" == hold ]]; then
  # Debugging: keep the live system running; talk to it through
  # $S/serial.sock and $S/shell.sock (podman exec), stop the container to end.
  log "holding the live system (MODE=hold)"
  wait "$qemu_pid" || true
  exit 0
elif [[ "$MODE" == gui ]]; then
  python3 /t/drive-gui.py --qmp "$S/qmp.sock" --shell "$S/shell.sock" --disk vda --shots "$LOGS/screenshots" \
    --key-out "$S/recovery-key.txt" --install-timeout "$INSTALL_TIMEOUT" --selinux-out "$LOGS/live-selinux.txt" \
    2>&1 | tee "$LOGS/driver.log" || rc=$?
else
  python3 /t/drive-tui.py --socket "$S/serial.sock" --disk vda --key-out "$S/recovery-key.txt" \
    --shell "$S/shell.sock" --selinux-out "$LOGS/live-selinux.txt" \
    --install-timeout "$INSTALL_TIMEOUT" 2>&1 | tee "$LOGS/driver.log" || rc=$?
fi
[[ $rc == 0 ]] || die "the $MODE driver failed (exit $rc)"

log "waiting for the live system to power off"
for _ in $(seq 1 120); do kill -0 "$qemu_pid" 2>/dev/null || break; sleep 1; done
if kill -0 "$qemu_pid" 2>/dev/null; then kill "$qemu_pid"; die "the live system did not power off"; fi
qemu_pid=""
t_install=$(( $(date +%s) - t0 ))
log "TIMING live boot + install: ${t_install}s"
kill "$http_pid" 2>/dev/null || true
http_pid=""
pkill -x swtpm 2>/dev/null || true
sleep 1

# The recovery key stays in the state directory (removed by the host script).
[[ -s "$S/recovery-key.txt" ]] || die "no recovery key captured"

# The live system itself runs SELinux enforcing; the installer must not
# have needed a single exception (read before the end action, see liveshell.py).
live_mode="$(sed -n 's/^mode //p' "$LOGS/live-selinux.txt" 2>/dev/null)"
live_avc="$(sed -n 's/^denials //p' "$LOGS/live-selinux.txt" 2>/dev/null)"
[[ "$live_mode" == Enforcing ]] || die "live system SELinux: '${live_mode:-unknown}', expected Enforcing ($LOGS/live-selinux.txt)"
[[ "$live_avc" == 0 ]] || { grep '^avc ' "$LOGS/live-selinux.txt" >&2 || true; die "live system: ${live_avc:-?} AVC denial(s)"; }
log "live system: SELinux Enforcing, 0 AVC denials during the installation"

log "running the boot test checks on the installed disk"
export INSTALL_MODE=external INSTALL_NOTE="basalt-installer ($MODE frontend), live boot + install ${t_install}s, live SELinux Enforcing 0 AVC, VM powered off"
export SSH_PORT REPO_PORT VM_MEMORY_MB VM_VCPUS HOST_OWNER
exec /ci/vm-test.sh
