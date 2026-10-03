#!/usr/bin/env bash
# Boot test, VM side. Runs as root inside a Fedora container with /dev/kvm
# (started by scripts/ci/boot-test.sh run); do not run it on a host.
#
# Mounts:   /ci (this directory, ro), /vm/installer.iso (ro), /repo (signed
#           test repository, ro), /keys (lab SSH key, ro), /work (state, logs)
#
# 1. Install: QEMU q35 + KVM, OVMF with Secure Boot on and the Microsoft and
#    Red Hat keys enrolled, swtpm TPM 2.0 (CRB), a blank virtio disk and the
#    ISO. The ISO's site file makes the kickstart install unattended and
#    power the VM off at the end.
# 2. First boot from the disk: the LUKS2 volume must unlock through the TPM
#    (nobody types anything; a passphrase prompt on the serial console fails
#    the test), then SSH.
# 3. Smoke checks over SSH: identity, Secure Boot on, SELinux enforcing with
#    0 AVC denials, 0 failed units, TPM2 and recovery key slots, snapper
#    pre/post snapshots around a dnf install, basalt-rollback dry run.
# 4. Reboot: TPM unlock again, package still installed, 0 denials.
# The guest reaches the test repository on 10.0.2.2 (QEMU user networking
# maps it to this container's loopback, where a small HTTP server runs).
set -euo pipefail

: "${SSH_PORT:=2222}"
: "${REPO_PORT:=8098}"
: "${VM_MEMORY_MB:=4096}"
: "${VM_VCPUS:=2}"
: "${VM_DISK_GB:=30}"
: "${INSTALL_TIMEOUT:=3600}"
: "${BOOT_TIMEOUT:=600}"
: "${TEST_PACKAGE:=tmux}"
: "${HOST_OWNER:=0:0}"

W=/work
S="$W/state"
LOGS="$W/logs"
ISO=/vm/installer.iso
OVMF_CODE=/usr/share/edk2/ovmf/OVMF_CODE_4M.secboot.qcow2
OVMF_VARS=/usr/share/edk2/ovmf/OVMF_VARS_4M.secboot.qcow2
SUMMARY="$LOGS/summary.txt"

log() { printf '==> [%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

failures=0
result() {
  # result PASS|FAIL name detail
  printf '%-4s  %-34s %s\n' "$1" "$2" "${3:-}" | tee -a "$SUMMARY" >&2
  [[ "$1" == PASS ]] || failures=$((failures + 1))
}

qemu_pid=""
http_pid=""
cleanup() {
  [[ -n "$qemu_pid" ]] && kill "$qemu_pid" 2>/dev/null || true
  [[ -n "$http_pid" ]] && kill "$http_pid" 2>/dev/null || true
  pkill -x swtpm 2>/dev/null || true
  chown -R "$HOST_OWNER" "$W" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$S" "$LOGS"
: >"$SUMMARY"

log "installing QEMU, OVMF, swtpm"
dnf -q -y install qemu-system-x86-core qemu-img edk2-ovmf swtpm swtpm-tools \
  openssh-clients python3 procps-ng >/dev/null 2>&1 || die "dnf install failed"
[[ -f "$OVMF_CODE" && -f "$OVMF_VARS" ]] || die "Secure Boot OVMF images not found"
[[ -c /dev/kvm ]] || die "no /dev/kvm in the container"

# The SSH key stays inside the container (the work directory is uploaded
# as a CI artifact).
install -d -m 0700 /root/.ssh
install -m 0600 /keys/vm_ed25519 /root/.ssh/vm
SSH_OPTS=(-p "$SSH_PORT" -i /root/.ssh/vm -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
  -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes -o ServerAliveInterval=5 -o ServerAliveCountMax=3)
vm() { ssh "${SSH_OPTS[@]}" root@127.0.0.1 "$@"; }

# --- emulated hardware ------------------------------------------------------------

cp "$OVMF_VARS" "$S/vars.qcow2"
rm -f "$S/disk.qcow2"
qemu-img create -q -f qcow2 "$S/disk.qcow2" "${VM_DISK_GB}G"
rm -rf "$S/tpm"
mkdir -p "$S/tpm"
swtpm_setup --tpm2 --tpmstate "$S/tpm" --createek --create-ek-cert --create-platform-cert \
  --lock-nvram --pcr-banks sha256 --overwrite >"$LOGS/swtpm-setup.log" 2>&1 ||
  { cat "$LOGS/swtpm-setup.log" >&2; die "swtpm_setup failed"; }

# swtpm ends when QEMU exits, so each QEMU process gets a fresh one (same state).
tpm_start() {
  pkill -x swtpm 2>/dev/null || true
  rm -f "$S/tpm.sock"
  swtpm socket --tpm2 --tpmstate dir="$S/tpm" --ctrl type=unixio,path="$S/tpm.sock" \
    --log file="$LOGS/swtpm.log",level=1 --terminate --daemon
  for _ in $(seq 1 50); do [[ -S "$S/tpm.sock" ]] && return 0; sleep 0.1; done
  die "swtpm did not start"
}

# qemu_start PHASE WITH_ISO(0|1): start QEMU in the background, serial
# console written to $LOGS/serial-PHASE.log.
qemu_start() {
  local phase="$1" with_iso="$2"
  tpm_start
  # QEMU options are comma separated by design.
  # shellcheck disable=SC2054
  local args=(
    -name basalt-ci -machine q35,smm=on,accel=kvm -cpu host -smp "$VM_VCPUS" -m "$VM_MEMORY_MB"
    -global driver=cfi.pflash01,property=secure,value=on
    -global ICH9-LPC.disable_s3=1
    -blockdev "node-name=fw-code,driver=qcow2,read-only=on,file.driver=file,file.filename=$OVMF_CODE"
    -blockdev "node-name=fw-vars,driver=qcow2,file.driver=file,file.filename=$S/vars.qcow2"
    -machine pflash0=fw-code,pflash1=fw-vars
    -chardev "socket,id=chrtpm,path=$S/tpm.sock" -tpmdev emulator,id=tpm0,chardev=chrtpm -device tpm-crb,tpmdev=tpm0
    -drive "if=none,id=disk0,file=$S/disk.qcow2,format=qcow2,cache=unsafe,discard=unmap"
    -device virtio-blk-pci,drive=disk0,serial=basalt-disk,bootindex=2
    -netdev "user,id=net0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22" -device virtio-net-pci,netdev=net0
    -object rng-random,id=rng0,filename=/dev/urandom -device virtio-rng-pci,rng=rng0
    -display none -monitor none -serial "file:$LOGS/serial-$phase.log"
  )
  if [[ "$with_iso" == 1 ]]; then
    # shellcheck disable=SC2054
    args+=(-drive "if=none,id=cd0,media=cdrom,readonly=on,file=$ISO" -device ide-cd,bus=ide.0,drive=cd0,bootindex=1)
  fi
  qemu-system-x86_64 "${args[@]}" >"$LOGS/qemu-$phase.log" 2>&1 &
  qemu_pid=$!
  sleep 2
  kill -0 "$qemu_pid" 2>/dev/null || { cat "$LOGS/qemu-$phase.log" >&2; die "QEMU did not start"; }
}

# Last readable line of a serial log, for progress output.
serial_tail() {
  sed -e 's/\x1b\[[0-9;?]*[A-Za-z]//g' -e 's/\r/\n/g' "$1" 2>/dev/null | grep -a '[[:alnum:]]' | tail -1 | cut -c1-150
}

PROMPT_RE='Please enter passphrase for disk|Enter passphrase for|recovery key for disk'

wait_qemu_exit() {
  local timeout="$1" logf="$2" t0 last=0 now
  t0=$(date +%s)
  while kill -0 "$qemu_pid" 2>/dev/null; do
    now=$(date +%s)
    if (( now - t0 > timeout )); then
      kill "$qemu_pid" 2>/dev/null || true
      log "QEMU still running after ${timeout}s; last serial lines:"
      sed -e 's/\x1b\[[0-9;?]*[A-Za-z]//g' "$logf" | tail -40 >&2
      qemu_pid=""
      return 1
    fi
    if (( now - last >= 120 )); then
      log "  $(( now - t0 ))s: $(serial_tail "$logf")"
      last=$now
    fi
    sleep 5
  done
  wait "$qemu_pid" 2>/dev/null || true
  qemu_pid=""
}

wait_ssh() {
  local timeout="$1" logf="$2" t0
  t0=$(date +%s)
  while (( $(date +%s) - t0 < timeout )); do
    if vm true 2>/dev/null; then
      log "SSH up after $(( $(date +%s) - t0 ))s"; return 0
    fi
    if grep -aqE "$PROMPT_RE" "$logf" 2>/dev/null; then
      log "last serial lines:"; sed -e 's/\x1b\[[0-9;?]*[A-Za-z]//g' "$logf" | tail -30 >&2
      return 2
    fi
    kill -0 "$qemu_pid" 2>/dev/null || { log "QEMU exited"; return 1; }
    sleep 3
  done
  log "last serial lines:"; sed -e 's/\x1b\[[0-9;?]*[A-Za-z]//g' "$logf" | tail -30 >&2
  return 1
}

denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
failed_units() { vm 'systemctl --failed --no-legend --plain | wc -l'; }

# Checks that every boot must pass.
boot_checks() {
  local tag="$1" n
  local enf; enf="$(vm getenforce)"
  [[ "$enf" == Enforcing ]] && result PASS "$tag selinux" "$enf" || result FAIL "$tag selinux" "$enf"
  n="$(denials)"
  if [[ "$n" == 0 ]]; then result PASS "$tag avc denials" "0"; else
    result FAIL "$tag avc denials" "$n"
    vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | tail -20' >&2 || true
  fi
  n="$(failed_units)"
  if [[ "$n" == 0 ]]; then result PASS "$tag failed units" "0"; else
    result FAIL "$tag failed units" "$n: $(vm 'systemctl --failed --no-legend --plain' | awk '{print $1}' | paste -sd' ')"
  fi
  if grep -aqE "$PROMPT_RE" "$LOGS/serial-boot.log"; then
    result FAIL "$tag tpm unlock" "passphrase prompt on the console"
  else
    result PASS "$tag tpm unlock" "no prompt, unlocked unattended"
  fi
}

# --- 1. install -----------------------------------------------------------------

log "serving the test repository on 127.0.0.1:$REPO_PORT (guest: 10.0.2.2)"
python3 -m http.server "$REPO_PORT" --bind 127.0.0.1 --directory /repo >"$LOGS/http.log" 2>&1 &
http_pid=$!

log "install: ${VM_VCPUS} vCPU, ${VM_MEMORY_MB} MiB, ${VM_DISK_GB} GiB disk, timeout ${INSTALL_TIMEOUT}s"
t0=$(date +%s)
qemu_start install 1
wait_qemu_exit "$INSTALL_TIMEOUT" "$LOGS/serial-install.log" || { result FAIL "install" "not finished in ${INSTALL_TIMEOUT}s"; die "install timed out"; }
t_install=$(( $(date +%s) - t0 ))
log "TIMING install: ${t_install}s"
grep -aq 'basalt-post: done' "$LOGS/serial-install.log" ||
  log "note: the %post completion line is not on the serial console (Anaconda logs it in the target)"
result PASS "install" "${t_install}s, VM powered off by the kickstart"

# --- 2. first boot ----------------------------------------------------------------

t0=$(date +%s)
qemu_start boot 0
rc=0; wait_ssh "$BOOT_TIMEOUT" "$LOGS/serial-boot.log" || rc=$?
if [[ $rc == 2 ]]; then result FAIL "first boot" "LUKS passphrase prompt: TPM unlock failed"; die "first boot stopped at the passphrase prompt"; fi
[[ $rc == 0 ]] || { result FAIL "first boot" "no SSH within ${BOOT_TIMEOUT}s"; die "first boot failed"; }
t_boot=$(( $(date +%s) - t0 ))
result PASS "first boot to ssh" "${t_boot}s"

# Give first-boot units (initial snapshot, theme) time to settle.
vm 'systemctl is-system-running --wait >/dev/null 2>&1 || true'

# --- 3. smoke checks ------------------------------------------------------------------

id="$(vm '. /etc/os-release; echo "$ID $VERSION_ID"')"
[[ "$id" == "basalt "* ]] && result PASS "os-release" "$id" || result FAIL "os-release" "$id"
if vm 'rpm -q basalt-release basalt-snapshots basalt-security >/dev/null && ! rpm -q fedora-release-common >/dev/null 2>&1'; then
  result PASS "basalt packages" "$(vm 'rpm -q basalt-release basalt-snapshots basalt-security' | paste -sd' ')"
else
  result FAIL "basalt packages" "missing basalt packages or fedora-release present"
fi
sb="$(vm 'mokutil --sb-state 2>&1 | head -1')"
[[ "$sb" == "SecureBoot enabled" ]] && result PASS "secure boot" "$sb" || result FAIL "secure boot" "$sb"
slots="$(vm 'dev=$(blkid -t TYPE=crypto_LUKS -o device | head -1); [ -n "$dev" ] && systemd-cryptenroll "$dev" | awk "NR > 1 {print \$2}" | sort | paste -sd" "' || true)"
[[ "$slots" == *tpm2* && "$slots" == *recovery* ]] && result PASS "luks key slots" "$slots" || result FAIL "luks key slots" "${slots:-no LUKS device}"
pcrs="$(vm 'dev=$(blkid -t TYPE=crypto_LUKS -o device | head -1); cryptsetup luksDump "$dev" | sed -n "s/.*tpm2-hash-pcrs: *//p" | head -1' || true)"
[[ "$pcrs" == 7 ]] && result PASS "tpm2 policy" "PCR $pcrs" || result FAIL "tpm2 policy" "PCRs: ${pcrs:-none}"
boot_checks "boot 1:"

log "snapshots before the dnf transaction:"; vm 'snapper list' >&2 || true
before="$(vm 'snapper --csvout list --columns number | tail -1')"
t0=$(date +%s)
if vm "dnf -y install $TEST_PACKAGE" >"$LOGS/dnf-install.log" 2>&1; then
  result PASS "dnf install $TEST_PACKAGE" "$(( $(date +%s) - t0 ))s"
else
  tail -20 "$LOGS/dnf-install.log" >&2
  result FAIL "dnf install $TEST_PACKAGE" "see dnf-install.log"
fi
pair="$(vm 'snapper --csvout list --columns number,type,pre-number,description | tail -1')"
post_num="${pair%%,*}"
pre_num="$(cut -d, -f3 <<<"$pair")"
pre_type="$(vm "snapper --csvout list --columns number,type | awk -F, -v n='$pre_num' '\$1 == n {print \$2}'" || true)"
if [[ "$pair" == *,post,* && "$pair" == *"$TEST_PACKAGE"* && "$pre_type" == pre && "$post_num" -gt "${before:-0}" ]]; then
  result PASS "snapper pre/post" "pre $pre_num, post $post_num ($(cut -d, -f4- <<<"$pair"))"
else
  result FAIL "snapper pre/post" "newest: '$pair', before: $before"
  pre_num=""
fi
if [[ -n "$pre_num" ]] && vm "basalt-snapshot-boot list" | grep -qw "$pre_num"; then
  result PASS "snapshot in boot menu" "snapshot $pre_num listed"
else
  result FAIL "snapshot in boot menu" "$(vm 'basalt-snapshot-boot list 2>&1 | head -3' | paste -sd' ')"
fi

default_before="$(vm 'btrfs subvolume get-default /')"
if [[ -n "$pre_num" ]]; then
  out="$(vm "printf 'n\n' | basalt-rollback $pre_num" 2>&1)" && rc=0 || rc=$?
  printf '%s\n' "$out" >"$LOGS/rollback-dry-run.log"
  default_after="$(vm 'btrfs subvolume get-default /')"
  if [[ $rc == 1 && "$out" == *"Command: snap"*"rollback"* && "$out" == *Cancelled.* && "$default_before" == "$default_after" ]]; then
    result PASS "basalt-rollback dry run" "snapshot $pre_num: command shown, declined, default subvolume unchanged"
  else
    result FAIL "basalt-rollback dry run" "exit $rc, see rollback-dry-run.log"
  fi
else
  result FAIL "basalt-rollback dry run" "no pre snapshot to roll back to"
fi
if vm 'basalt-rollback --kernels' >"$LOGS/rollback-kernels.log" 2>&1; then
  result PASS "basalt-rollback --kernels" "kernel table shown"
else
  result FAIL "basalt-rollback --kernels" "see rollback-kernels.log"
fi

# --- 4. reboot ------------------------------------------------------------------------

boot_id="$(vm 'cat /proc/sys/kernel/random/boot_id')"
vm 'systemctl reboot' >/dev/null 2>&1 || true
sleep 15
t0=$(date +%s)
rebooted=0
while (( $(date +%s) - t0 < BOOT_TIMEOUT )); do
  if grep -aqE "$PROMPT_RE" "$LOGS/serial-boot.log"; then break; fi
  new_id="$(vm 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true)"
  if [[ -n "$new_id" && "$new_id" != "$boot_id" ]]; then rebooted=1; break; fi
  sleep 3
done
if [[ $rebooted == 1 ]]; then
  result PASS "reboot to ssh" "$(( $(date +%s) - t0 + 15 ))s"
  vm 'systemctl is-system-running --wait >/dev/null 2>&1 || true'
  boot_checks "boot 2:"
  if vm "rpm -q $TEST_PACKAGE" >/dev/null; then
    result PASS "package persists" "$(vm "rpm -q $TEST_PACKAGE")"
  else
    result FAIL "package persists" "$TEST_PACKAGE missing after reboot"
  fi
else
  result FAIL "reboot to ssh" "no SSH on a new boot within ${BOOT_TIMEOUT}s (prompt seen: $(grep -acE "$PROMPT_RE" "$LOGS/serial-boot.log"))"
fi

vm 'snapper list' >"$LOGS/snapper-list.txt" 2>&1 || true
vm 'systemctl poweroff' >/dev/null 2>&1 || true
wait_qemu_exit 180 "$LOGS/serial-boot.log" || log "the VM did not power off in 180s (killed)"

echo >&2
log "summary ($failures failed):"
cat "$SUMMARY" >&2
[[ $failures == 0 ]] || die "$failures check(s) failed"
log "boot test passed"
