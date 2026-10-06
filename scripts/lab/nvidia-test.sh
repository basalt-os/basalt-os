#!/usr/bin/env bash
# The NVIDIA driver of basalt-nonfree on a lab VM (docs/nvidia.md). Lab VMs
# have no NVIDIA GPU, so the driver never binds; what this proves is the
# packaging, the signatures, nouveau off, the kernel guard, the trial and
# its fallback (the module refuses to load without a GPU: a real failure),
# the person being told why, and the rollback.
#
#   scripts/lab/nvidia-test.sh keys            issue a throwaway basalt-nonfree module signing
#                                              certificate from the lab module CA
#   scripts/lab/nvidia-test.sh repo DIR        sign the packages of DIR (build-nonfree.sh packages)
#                                              into the lab repository, REPO_DIR/nonfree
#   scripts/lab/nvidia-test.sh mok             enroll the lab module CA in the VM (MokManager)
#   scripts/lab/nvidia-test.sh install         the assistant's driver.install, applied like a person does
#   scripts/lab/nvidia-test.sh modules         signatures accepted, unsigned copy refused, nouveau
#                                              out of the initramfs, files byte-identical (rpm -V)
#   scripts/lab/nvidia-test.sh kernels         the kernel guard: dnf holds a newer kernel; a kernel
#                                              installed anyway never becomes the default without its module
#   scripts/lab/nvidia-test.sh trial           restart: the trial falls back to nouveau, with the reason
#   scripts/lab/nvidia-test.sh hang            a trial boot that never reaches its check: the next boot falls back
#   scripts/lab/nvidia-test.sh rollback        the rollback proposal, applied: the system from before the install
#   scripts/lab/nvidia-test.sh all             install modules kernels trial hang rollback
#
# Uses the lab settings of .env (VM_NAME, LAB_DIR, REPO_DIR) like the other
# lab scripts; the VM is a lab install with Secure Boot firmware and the lab
# basalt-security (lab module CA), with basalt-nonfree-release pointing at
# LAB_REPO_URL/nonfree (BASALT_DEFAULT_NONFREE_URL).
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
vm() { "$L/vm.sh" ssh "$@"; }
denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
expect() { # expect WHAT COMMAND...: run in the VM, fail the test on non-zero
  local what=$1; shift
  if vm "$@"; then log "ok: $what"; else die "FAILED: $what"; fi
}
reboot_vm() {
  vm 'systemctl reboot' || true
  sleep 15
  "$L/vm.sh" wait-ssh 600
}
# apply_proposal ID: what Settings does, without the window: the code from
# basalt show, then basalt apply --yes --confirm.
apply_proposal() {
  local id=$1 code
  code="$(vm "basalt show $id" | sed -n 's/.*--confirm \([0-9a-f]\{8\}\).*/\1/p' | head -1)"
  [[ -n "$code" ]] || die "no confirmation code for $id"
  vm "basalt apply $id --yes --confirm $code"
}

phase_keys() {
  local k="$LAB_DIR/sb-keys"
  [[ -s "$k/module-ca.key" ]] || die "no lab module CA (make lab-sb-keys)"
  umask 077
  openssl req -new -newkey rsa:4096 -sha256 -nodes -subj "/CN=Basalt OS lab nonfree module signing (THROWAWAY)/" \
    -keyout "$k/nonfree-module-signing.key" -out "$k/nonfree.req" 2>/dev/null
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\n' >"$k/nonfree.ext"
  openssl x509 -req -in "$k/nonfree.req" -CA "$k/module-ca.pem" -CAkey "$k/module-ca.key" -CAcreateserial -CAserial "$k/nonfree.srl" \
    -days 30 -sha256 -extfile "$k/nonfree.ext" -out "$k/nonfree-module-signing.pem" 2>/dev/null
  rm -f "$k/nonfree.req" "$k/nonfree.ext" "$k/nonfree.srl"
  chmod 0644 "$k/nonfree-module-signing.pem"
  openssl x509 -in "$k/nonfree-module-signing.pem" -noout -subject -issuer
  log "for sign-modules.sh: OB_MODULE_KEY_FILE (copy to a tmpfs, 0600) and OB_MODULE_CERT_FILE=$k/nonfree-module-signing.pem"
}

phase_repo() {
  local dir="${1:?usage: nvidia-test.sh repo DIR}" rpms
  mapfile -t rpms < <(find "$dir" -maxdepth 1 -name '*.rpm' ! -name '*.src.rpm' | sort)
  [[ ${#rpms[@]} -gt 0 ]] || die "no binary RPMs in $dir"
  REPO_DIR="$REPO_DIR/nonfree" "$REPO_ROOT/scripts/repo.sh" publish "${rpms[@]}"
}

phase_mok() {
  local pw
  if vm 'mokutil --test-key /usr/share/basalt/secureboot/basalt-module-ca.der 2>&1 | grep -q "already enrolled"'; then
    log "the lab module CA is enrolled already"; return 0
  fi
  umask 077
  pw="$(mktemp "$LAB_DIR/sb-keys/mok-onetime.XXXXXX")"
  openssl rand -hex 8 >"$pw"
  "$L/vm.sh" scp-to "$pw" /run/basalt-mok-pw >/dev/null
  vm 'basalt-secureboot enroll-mok --yes --password-file=/run/basalt-mok-pw; shred -u /run/basalt-mok-pw' | tail -2
  vm 'systemctl reboot' || true
  sleep 2
  "$L/mok-console.py" "$VM_NAME" --password-file "$pw" --timeout 240 || die "MokManager confirmation failed"
  shred -u "$pw"
  sleep 5
  "$L/vm.sh" wait-ssh 600
  expect "the lab module CA is enrolled" 'mokutil --test-key /usr/share/basalt/secureboot/basalt-module-ca.der 2>&1 | grep -q "already enrolled"'
}

phase_install() {
  local kver id
  kver="$(vm 'uname -r')"
  log "VM: kernel $kver, $(vm 'mokutil --sb-state | head -1'), lockdown $(vm 'cat /sys/kernel/security/lockdown')"
  vm 'basalt drivers' || true
  vm 'basalt drivers --json' | python3 -c 'import json,sys; d=json.load(sys.stdin); print("detected:", [(g["vendor_id"]+":"+g["device_id"], g["driver"]) for g in (d["gpus"] or [])], "action:", d["recommendation"]["action"])'
  # No NVIDIA GPU in a VM, so `basalt drivers install` refuses; the
  # proposal it would store for one is written here with the same typed
  # action (the closed set validates it again when it is shown and applied).
  id="p-$(openssl rand -hex 3)"
  vm "install -d -m 0700 /var/lib/basalt-assistant/proposals && cat >/var/lib/basalt-assistant/proposals/$id.json" <<EOF
{"id":"$id","created":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","updated":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","source":"cli","kind":"driver","subject":"nvidia",
 "key":"driver:nvidia:install:display","title":"install the NVIDIA driver (lab: no NVIDIA GPU in this VM)",
 "facts":{"kind":"driver","cause":"install","subject":"nvidia","values":{"gpu":"the lab VM","version":"lab","variant":"display","kernel":"$kver"}},
 "actions":[{"kind":"driver.install","params":{"driver":"nvidia","variant":"display","kernel":"$kver","license":"c692fb14ad4f499c6aee1b9e144e6905148c420e6225b17cb2937366c1e507ad"}}],
 "needs_review":false,"severity":1,"status":"pending","seen":1,"last_seen":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
  vm "restorecon -R /var/lib/basalt-assistant/proposals"
  vm "basalt show $id"
  apply_proposal "$id"
  expect "the next start is a trial" 'basalt-nvidia status --json | grep -q "\"mode\":\"trial\""'
  expect "the snapshot of the apply is recorded" "grep -q '^proposal=$id' /var/lib/basalt-nvidia/state"
  vm 'basalt-nvidia status; basalt drivers' || true
  log "SELinux denials this boot: $(denials)"
}

phase_modules() {
  local kver; kver="$(vm 'uname -r')"
  # modinfo names the issuer and the serial of the signing certificate.
  expect "the module is signed with the basalt-nonfree certificate basalt-nvidia ships" \
    "m=/usr/lib/modules/$kver/extra/nvidia-open/nvidia.ko; c=/usr/lib/basalt/module-keys/basalt-nonfree-module-signing.der; \
     modinfo \$m | grep -E '^(signer|sig_key|sig_hashalgo):'; \
     [ \"\$(modinfo -F sig_key \$m | tr -d ':' | tr A-F a-f)\" = \"\$(openssl x509 -inform DER -in \$c -noout -serial | cut -d= -f2 | tr A-F a-f)\" ]"
  vm 'basalt-secureboot load-module-keys; keyctl show %:.secondary_trusted_keys | grep -i "module signing"'
  # The kernel checks the signature before the module runs: without a GPU
  # a module it accepted fails with "No such device"; one it refused fails
  # with "Key was rejected by service".
  expect "signature accepted (the module then finds no GPU)" \
    'out=$(modprobe nvidia 2>&1); echo "$out"; echo "$out" | grep -q "No such device" && ! echo "$out" | grep -qi "key was rejected"'
  vm 'dmesg | grep -i -E "NVRM|nvidia" | tail -5' || true
  # The same module without its signature (the trailer cut off) is refused.
  vm "python3 - /usr/lib/modules/$kver/extra/nvidia-open/nvidia.ko /root/nvidia-unsigned.ko" <<'PY'
import sys
b = open(sys.argv[1], "rb").read()
m = b"~Module signature appended~\n"
assert b.endswith(m), "not signed"
n = int.from_bytes(b[-len(m) - 4:-len(m)], "big")
open(sys.argv[2], "wb").write(b[:-(len(m) + 12 + n)])
PY
  expect "the same module without its signature is refused" \
    'out=$(insmod /root/nvidia-unsigned.ko 2>&1); rm -f /root/nvidia-unsigned.ko; echo "$out"; echo "$out" | grep -q -E "Key was rejected|Required key not available|Operation not permitted"'
  expect "nouveau is off (modprobe.d)" 'modprobe -c | grep -qx "blacklist nouveau"'
  expect "neither nouveau nor nvidia in the initramfs" "! lsinitrd /boot/initramfs-$kver.img 2>/dev/null | grep -E '/(nouveau|nvidia[-a-z]*)\\.ko'"
  expect "installed files match the packages (rpm -V)" 'rpm -V nvidia-driver-libs nvidia-driver-cuda-libs nvidia-driver-cuda nvidia-driver-firmware nvidia-driver-power'
  expect "the Agreement is in every NVIDIA package" \
    'for p in $(rpm -qa "nvidia-driver*" --qf "%{NAME}\n"); do rpm -ql $p | grep -q "/usr/share/licenses/$p/LICENSE" || { echo "no LICENSE in $p"; exit 1; }; done'
  vm 'sha256sum /usr/share/licenses/nvidia-driver/LICENSE'
}

phase_kernels() {
  local cur newest
  cur="$(vm 'uname -r')"
  vm 'rpm -q kmod-nvidia-open --qf "%{NAME} %{VERSION}-%{RELEASE}\n"; rpm -q --conflicts kmod-nvidia-open'
  log "1. dnf holds a kernel newer than the one kmod-nvidia-open follows"
  avail="$(vm "dnf -q repoquery --refresh --enablerepo=updates-testing --latest-limit=1 --qf '%{version}-%{release}.%{arch}\n' kernel-core | tail -1")"
  log "newest kernel-core available (updates-testing on): $avail"
  vm 'dnf -y upgrade --enablerepo=updates-testing kernel kernel-core kernel-modules kernel-modules-core 2>&1 | grep -i -E "conflict|skip|problem|nothing|Complete" | head -8' || true
  newest="$(vm 'rpm -q kernel-core --qf "%{VERSION}-%{RELEASE}.%{ARCH}\n" | sort -V | tail -1')"
  log "newest kernel-core installed: $newest (running $cur)"
  expect "no kernel without its module was installed" "rpm -q kmod-nvidia-open-$newest >/dev/null"
  [[ "$avail" != "$newest" ]] || { log "no newer kernel available; skipping the boot entry check"; return 0; }
  log "2. a newer kernel installed anyway (removing kmod-nvidia-open): the default stays"
  vm "dnf -y install --allowerasing --enablerepo=updates-testing kernel-${avail%.x86_64} 2>&1 | grep -E 'Installing|Removing|kmod' | head -12" || true
  newest="$(vm 'rpm -q kernel-core --qf "%{VERSION}-%{RELEASE}.%{ARCH}\n" | sort -V | tail -1')"
  [[ "$newest" == "$avail" ]] || die "kernel $avail was not installed"
  vm 'grubby --default-kernel; basalt-nvidia status | grep -i -E "held|default"'
  expect "the default boot entry is not the kernel without a module" "[ \"\$(grubby --default-kernel)\" != /boot/vmlinuz-$newest ]"
  log "3. its module arrives: that kernel becomes the default"
  vm "dnf -y install kmod-nvidia-open-$newest 2>&1 | tail -4"
  expect "the default boot entry is the new kernel" "[ \"\$(grubby --default-kernel)\" = /boot/vmlinuz-$newest ]"
  vm 'basalt-ledger --event driver. 2>/dev/null | tail -5 || journalctl -t basalt-nvidia --no-pager -o cat | tail -5'
}

phase_trial() {
  reboot_vm
  vm 'systemctl status basalt-nvidia-boot.service basalt-nvidia-check.service --no-pager -l | grep -E "Active|basalt-nvidia" | head -8' || true
  vm 'basalt-nvidia status; cat /run/basalt-nvidia/boot' || true
  expect "this boot uses nouveau (no NVIDIA GPU)" 'grep -q "^driver=nouveau" /run/basalt-nvidia/boot'
  expect "the trial fell back for good" 'grep -q "^mode=fallback" /var/lib/basalt-nvidia/state && grep -q "^# basalt-nvidia fallback" /etc/modprobe.d/basalt-nvidia.conf'
  vm 'basalt drivers' || true
  vm 'journalctl -b -t basalt-nvidia --no-pager -o cat | tail -6' || true
  log "SELinux denials this boot: $(denials)"
}

phase_hang() {
  # A trial boot that never reached its check: the state says a trial ran
  # on an earlier boot. The next boot must fall back before udev.
  vm 'basalt-nvidia retry'
  vm 'sed -i "s/^trial_boot=.*//" /var/lib/basalt-nvidia/state; echo trial_boot=00000000000000000000000000000000 >>/var/lib/basalt-nvidia/state'
  reboot_vm
  expect "an unfinished trial makes the next boot fall back" 'grep -q "did not finish" /var/lib/basalt-nvidia/state && grep -q "^driver=nouveau" /run/basalt-nvidia/boot'
}

phase_rollback() {
  local id
  id="$(vm 'basalt drivers rollback --json' | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
  vm "basalt show $id | head -20"
  apply_proposal "$id"
  reboot_vm
  expect "after the rollback: no NVIDIA packages" '! rpm -q nvidia-driver basalt-nvidia >/dev/null'
  expect "after the rollback: nouveau allowed again" '! modprobe -c | grep -qx "blacklist nouveau"'
  expect "after the rollback: basalt-nonfree off" '! dnf repo list --enabled 2>/dev/null | grep -q basalt-nonfree'
  vm 'basalt drivers' || true
  log "SELinux denials this boot: $(denials)"
}

cmd="${1:-all}"; shift || true
case "$cmd" in
  keys) phase_keys ;;
  repo) phase_repo "$@" ;;
  mok) phase_mok ;;
  install) phase_install ;;
  modules) phase_modules ;;
  kernels) phase_kernels ;;
  trial) phase_trial ;;
  hang) phase_hang ;;
  rollback) phase_rollback ;;
  all) phase_install; phase_modules; phase_kernels; phase_trial; phase_hang; phase_rollback ;;
  *) sed -n '2,25p' "$0"; exit 2 ;;
esac
