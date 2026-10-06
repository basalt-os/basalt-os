#!/usr/bin/env bash
# Tests of basalt-nvidia (packages/nvidia/basalt-nvidia/basalt-nvidia): the
# trial, the boot-time choice, the check, the fallback, retry, the kernel
# guard and status, against a fake system (module tree, firmware, snapshot
# metadata, boot id) and stub commands (modprobe, modinfo, nvidia-smi,
# systemctl, grubby, uname, basalt-ledger). Runs as any user; no root, no
# GPU, nothing outside a temporary directory.
#
#   packages/nvidia/tests/basalt-nvidia-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
bn="$here/../basalt-nvidia/basalt-nvidia"
T="$(mktemp -d)"
trap 'kill "${sockpid:-0}" 2>/dev/null || true; rm -rf "$T"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$*"; }
bad() { printf 'FAIL %s\n' "$*" >&2; fail=1; }
check() { local what=$1; shift; if "$@"; then ok "$what"; else bad "$what"; fi; }

# --- the fake system -----------------------------------------------------------
V=595.104.02
mkdir -p "$T"/{bin,state,run,etcmod,runmod,modules,firmware/$V,snapshots/41,sys/module,egl,ledger}
export BASALT_NVIDIA_TEST_ROOT=1 BASALT_NVIDIA_PATH="$T/bin:/usr/bin:/bin"
export BASALT_NVIDIA_STATE_DIR="$T/state" BASALT_NVIDIA_RUN_DIR="$T/run"
export BASALT_NVIDIA_ETC_MODPROBE="$T/etcmod/basalt-nvidia.conf" BASALT_NVIDIA_RUN_MODPROBE="$T/runmod/basalt-nvidia.conf"
export BASALT_NVIDIA_MODULES="$T/modules" BASALT_NVIDIA_FIRMWARE="$T/firmware" BASALT_NVIDIA_SNAPSHOTS="$T/snapshots"
export BASALT_NVIDIA_BOOT_ID="$T/boot_id" BASALT_NVIDIA_SYS="$T/sys" BASALT_NVIDIA_EGL_VENDOR="$T/egl/10_nvidia.json"
export BASALT_NVIDIA_LEDGER_SOCK="$T/ledger/ledger.sock" BASALT_NVIDIA_DM_WAIT=0
export FAKE_KVER=7.2.8-200.fc44.x86_64 FAKE_MODPROBE=ok FAKE_SMI=ok T

kernel() { # kernel KVER [with-module VERSION]
  mkdir -p "$T/modules/$1"
  : >"$T/modules/$1/vmlinuz"
  if [[ "${2:-}" == with-module ]]; then
    mkdir -p "$T/modules/$1/extra/nvidia-open"
    echo "${3:-$V}" >"$T/modules/$1/extra/nvidia-open/nvidia.ko"
  fi
}
newboot() { echo "$1" >"$T/boot_id"; rm -rf "$T/run"/* "$T/runmod"/* "$T/sys/module"/*; }
cat >"$T/snapshots/41/info.xml" <<'EOF'
<?xml version="1.0"?>
<snapshot>
  <type>pre</type>
  <num>41</num>
  <date>2026-10-05 20:00:00</date>
  <description>basalt apply p-1a2b3c</description>
</snapshot>
EOF

# Stubs.
cat >"$T/bin/uname" <<'EOF'
#!/bin/sh
echo "$FAKE_KVER"
EOF
cat >"$T/bin/modinfo" <<'EOF'
#!/bin/sh
# modinfo -F version FILE: the fake module file holds its version.
[ "$1" = -F ] && [ -f "$3" ] && cat "$3"
EOF
cat >"$T/bin/modprobe" <<'EOF'
#!/bin/sh
echo "modprobe $*" >>"$T/modprobe.log"
[ "$1" = -r ] && exit 0
case "$FAKE_MODPROBE" in
  ok) mkdir -p "$T/sys/module/nvidia" "$T/sys/module/nvidia_drm/parameters"; echo Y >"$T/sys/module/nvidia_drm/parameters/modeset" ;;
  key) echo "modprobe: ERROR: could not insert 'nvidia': Key was rejected by service" >&2; exit 1 ;;
  nodev) echo "modprobe: ERROR: could not insert 'nvidia': No such device" >&2; exit 1 ;;
esac
EOF
cat >"$T/bin/nvidia-smi" <<'EOF'
#!/bin/sh
[ "$FAKE_SMI" = ok ] && { echo "GPU 0: NVIDIA GeForce RTX 4060 Laptop GPU (UUID: GPU-0)"; exit 0; }
echo "NVIDIA-SMI has failed" >&2; exit 9
EOF
cat >"$T/bin/systemctl" <<'EOF'
#!/bin/sh
case "$1" in
  get-default) echo graphical.target ;;
  is-active) exit 0 ;;
esac
EOF
cat >"$T/bin/grubby" <<'EOF'
#!/bin/sh
case "$1" in
  --default-kernel) cat "$T/grub-default" ;;
  --set-default) echo "$2" >"$T/grub-default" ;;
esac
EOF
cat >"$T/bin/basalt-ledger" <<'EOF'
#!/bin/sh
cat >>"$T/ledger.jsonl"
EOF
chmod +x "$T"/bin/*
# A listening socket for the ledger check (-S).
python3 -c 'import socket,sys,time; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); s.listen(); time.sleep(600)' "$T/ledger/ledger.sock" &
sockpid=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do [[ -S "$T/ledger/ledger.sock" ]] && break; sleep 0.1; done

state() { sed -n "s/^$1=//p" "$T/state/state" 2>/dev/null; }
bootdrv() { sed -n 's/^driver=//p' "$T/run/boot" 2>/dev/null; }
last_event() { tail -1 "$T/ledger.jsonl" | python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["record"]["event"])'; }

# --- 1. install, trial, check ok -------------------------------------------------
kernel 6.19.10-300.fc44.x86_64
kernel 7.2.8-200.fc44.x86_64 with-module
: >"$T/egl/10_nvidia.json"
echo /boot/vmlinuz-7.2.8-200.fc44.x86_64 >"$T/grub-default"
"$bn" arm >/dev/null
check "arm: trial with the apply's snapshot" test "$(state mode) $(state snapshot) $(state proposal)" = "trial 41 p-1a2b3c"
check "arm: recorded in the ledger" test "$(last_event)" = driver.install
newboot aaaa
"$bn" boot
check "trial boot: NVIDIA chosen, trial recorded before the module loads" test "$(bootdrv) $(state trial_boot)" = "nvidia aaaa"
check "trial boot: nouveau still off (no override)" test ! -e "$T/runmod/basalt-nvidia.conf"
"$bn" check >/dev/null
check "check ok: the driver is active" test "$(state mode)" = active
check "check ok: recorded" test "$(last_event)" = driver.check

# --- 2. a trial that hangs: the next boot falls back --------------------------------
"$bn" retry >/dev/null
newboot bbbb
"$bn" boot
check "second trial boot recorded" test "$(state trial_boot)" = bbbb
newboot cccc          # bbbb never reached its check (hang, power off)
"$bn" boot
check "unfinished trial: this boot uses nouveau" test "$(bootdrv)" = nouveau
check "unfinished trial: /run override for this boot" grep -q '^blacklist nvidia$' "$T/runmod/basalt-nvidia.conf"
check "unfinished trial: persistent fallback in /etc" grep -q '^install nvidia /bin/false$' "$T/etcmod/basalt-nvidia.conf"
check "unfinished trial: state fallback with the reason" test "$(state mode)" = fallback
grep -q "did not finish" "$T/state/state" && ok "fallback reason names the unfinished start" || bad "fallback reason"
check "fallback recorded" test "$(last_event)" = driver.fallback
newboot dddd
"$bn" boot
check "after a fallback: nouveau again, no modprobe" test "$(bootdrv)" = nouveau
"$bn" check >/dev/null
check "check leaves a fallback alone" test "$(state mode)" = fallback

# --- 2b. records made while the ledger is not running wait in a queue ------------------
mv "$T/ledger/ledger.sock" "$T/ledger/ledger.sock.off"
"$bn" fallback "test while the ledger is down" >/dev/null
check "no ledger: the record waits in the queue" grep -q '"driver.fallback"' "$T/state/ledger-queue"
mv "$T/ledger/ledger.sock.off" "$T/ledger/ledger.sock"
"$bn" check >/dev/null
check "the check sends the queued record" test ! -e "$T/state/ledger-queue"
grep -q "test while the ledger is down" "$T/ledger.jsonl" && ok "queued record reached the ledger" || bad "queued record lost"

# --- 3. retry, then a refused signature -------------------------------------------
"$bn" retry >/dev/null
check "retry removes the /etc override" test ! -e "$T/etcmod/basalt-nvidia.conf"
check "retry arms a new trial" test "$(state mode)" = trial
newboot eeee
FAKE_MODPROBE=key "$bn" boot
check "refused signature: nouveau this boot and fallback" test "$(bootdrv) $(state mode)" = "nouveau fallback"
grep -q "refused the signature" "$T/state/state" && ok "reason: the signature" || bad "reason: the signature"

# --- 4. check fails in a trial --------------------------------------------------------
"$bn" retry >/dev/null
newboot ffff
"$bn" boot
FAKE_SMI=fail "$bn" check >/dev/null
check "trial check failure: fallback" test "$(state mode)" = fallback
grep -q "nvidia-smi does not answer" "$T/state/state" && ok "reason: nvidia-smi" || bad "reason: nvidia-smi"

# --- 5. active driver, a kernel without its module -------------------------------------
"$bn" retry >/dev/null; newboot gggg; "$bn" boot; "$bn" check >/dev/null
check "active again" test "$(state mode)" = active
newboot hhhh
FAKE_KVER=6.19.10-300.fc44.x86_64 "$bn" boot
check "kernel without module: nouveau this boot only" test "$(bootdrv)" = nouveau
check "kernel without module: no persistent fallback" test "$(state mode)" = active
check "kernel without module: no /etc override" test ! -e "$T/etcmod/basalt-nvidia.conf"
# A module of another driver version.
kernel 7.2.8-200.fc44.x86_64 with-module 590.48.01
newboot iiii
"$bn" boot
check "module of another version: nouveau this boot" test "$(bootdrv)" = nouveau
grep -q "does not match" "$T/run/boot" && ok "reason: version mismatch" || bad "reason: version mismatch"
kernel 7.2.8-200.fc44.x86_64 with-module

# --- 6. the kernel guard ------------------------------------------------------------------
kernel 7.2.9-200.fc44.x86_64     # a newer kernel without its module; Fedora made it the default
echo /boot/vmlinuz-7.2.9-200.fc44.x86_64 >"$T/grub-default"
"$bn" select-default >/dev/null
check "new kernel without module: default stays on 7.2.8" test "$(cat "$T/grub-default")" = /boot/vmlinuz-7.2.8-200.fc44.x86_64
check "the hold is recorded" test "$(state held_kernel) $(last_event)" = "7.2.9-200.fc44.x86_64 driver.kernel_hold"
kernel 7.2.9-200.fc44.x86_64 with-module   # its kmod arrives
"$bn" select-default >/dev/null
check "module arrives: 7.2.9 is the default again" test "$(cat "$T/grub-default")" = /boot/vmlinuz-7.2.9-200.fc44.x86_64
check "the release is recorded" test "$(state held_kernel)|$(last_event)" = "|driver.kernel_release"
echo /boot/vmlinuz-7.2.8-200.fc44.x86_64 >"$T/grub-default"   # the person picks an older kernel that has its module
"$bn" select-default >/dev/null
check "a default the person chose is left alone" test "$(cat "$T/grub-default")" = /boot/vmlinuz-7.2.8-200.fc44.x86_64

# --- 7. status ---------------------------------------------------------------------------
"$bn" status --json | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["mode"]=="active" and "7.2.9" in d["kernels_with_module"], d' &&
  ok "status --json" || bad "status --json"
out="$("$bn" status)"
grep -q "NVIDIA driver: in use" <<<"$out" && ok "status text" || { bad "status text"; printf '%s\n' "$out" >&2; }
python3 -c '
import json,sys
for l in open(sys.argv[1]):
    r=json.loads(l)
    assert r["op"]=="append" and r["record"]["producer"]=="basalt-nvidia" and r["record"]["v"]==1, r
' "$T/ledger.jsonl" && ok "every ledger request is a valid append" || bad "ledger requests"

[[ $fail == 0 ]] || { echo "basalt-nvidia tests failed" >&2; exit 1; }
echo "basalt-nvidia: all tests passed"
