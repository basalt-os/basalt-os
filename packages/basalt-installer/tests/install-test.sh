#!/usr/bin/env bash
# Installer lab test: build a lab live ISO, install it in a QEMU/KVM VM with
# Secure Boot and an emulated TPM through the text or the graphical
# installer, then run the kickstart boot test's checks on the result.
#
#   packages/basalt-installer/tests/install-test.sh iso        lab live ISO (plan on the media)
#   packages/basalt-installer/tests/install-test.sh tui        install via the text installer (serial)
#   packages/basalt-installer/tests/install-test.sh gui        install via the graphical installer (screenshots)
#   packages/basalt-installer/tests/install-test.sh hold       boot the live ISO and keep it running (debugging)
#   packages/basalt-installer/tests/install-test.sh kickstart  the same checks on a kickstart install of the same
#                                                            repository (scripts/iso.sh build with SITE_DIR), for comparison
#
# Needs: the signed repository in REPO_DIR with basalt-installer (build.sh,
# make repo), lab keys in LAB_DIR (make lab-keys; the SSH key for root on
# the VM). The lab ISO carries a plan (basalt/plans/default.yaml: the VM's
# disk, TPM unlock, root's lab SSH key, install from the media, power off)
# and a root shell on the second serial port (systemd.debug_shell=ttyS1)
# that the GUI driver uses to read the installer's status; neither is in a
# release ISO. Outputs: $BUILD_DIR/installer-test/<mode>/logs (serial
# consoles, screenshots, summary). Loopback ports: SSH_PORT (2291), REPO_PORT
# (8191).
source "$(dirname "$0")/../../../scripts/lib.sh"
tests="$REPO_ROOT/packages/basalt-installer/tests"
: "${SSH_PORT:=2291}"
: "${REPO_PORT:=8191}"
iso="$BUILD_DIR/live/basalt-os-$BASALT_VERSION-$ARCH-live-lab.iso"

build_iso() {
  [[ -f "$LAB_DIR/keys/vm_ed25519.pub" ]] || die "no lab SSH key (make lab-keys)"
  local plans
  plans="$(mktemp -d)"
  cat >"$plans/default.yaml" <<EOF
# Lab plan (install-test.sh): the person still reviews it and types the
# disk name; the recovery key is still shown once and acknowledged.
apiVersion: basalt-install-plan/v1
target: {disk: /dev/vda, wipe: true}
encryption: {enabled: true, unlock: tpm2}
hostname: basalt-inst
accounts:
  root:
    ssh_keys: ["$(cat "$LAB_DIR/keys/vm_ed25519.pub")"]
repos:
  basalt:
    url: media
    installed_url: http://10.0.2.2:$REPO_PORT
finish: poweroff
EOF
  LIVE_PLANS="$plans" LIVE_NAME=lab LIVE_TIMEOUT=3 LIVE_CMDLINE="systemd.debug_shell=ttyS1" \
    "$REPO_ROOT/packages/basalt-installer/live/build-live.sh"
  rm -rf "$plans"
}

run_vm() {
  local mode="$1" vmdir="$BUILD_DIR/installer-test/$1" rc=0
  [[ -f "$iso" ]] || die "no lab ISO at $iso (install-test.sh iso)"
  [[ -c /dev/kvm ]] || die "no /dev/kvm"
  sudo rm -rf "$vmdir"
  mkdir -p "$vmdir"
  log "installer test ($mode) in $FEDORA_IMAGE (logs: $vmdir/logs)"
  $PODMAN run --rm --name "basalt-inst-test-$mode" --network=host --security-opt label=disable --device /dev/kvm \
    -v "$tests:/t:ro" -v "$REPO_ROOT/scripts/ci:/ci:ro" -v "$iso:/vm/live.iso:ro" -v "$REPO_DIR:/repo:ro" \
    -v "$LAB_DIR/keys:/keys:ro" -v "$vmdir:/work" \
    -e MODE="$mode" -e SSH_PORT="$SSH_PORT" -e REPO_PORT="$REPO_PORT" -e HOST_OWNER="$(id -u):$(id -g)" \
    -e VM_MEMORY_MB="${VM_MEMORY_MB:-4096}" -e VM_VCPUS="${VM_VCPUS:-4}" -e INSTALL_TIMEOUT="${INSTALL_TIMEOUT:-3600}" \
    -e EXPECT_FEDORA_RELEASE="$FEDORA_RELEASE" -e EXPECT_BASALT_VERSION="$BASALT_VERSION" \
    "$FEDORA_IMAGE" /t/vm-install-test.sh || rc=$?
  # The disk and the TPM state hold the lab install and its recovery key.
  sudo rm -rf "$vmdir/state"
  [[ -f "$vmdir/logs/summary.txt" ]] && cat "$vmdir/logs/summary.txt" >&2
  return "$rc"
}

run_kickstart() {
  local vmdir="$BUILD_DIR/installer-test/kickstart" ks_iso="$BUILD_DIR/iso/basalt-os-$BASALT_VERSION-$ARCH-$SITE_NAME.iso" rc=0
  [[ -f "$ks_iso" ]] || die "no kickstart ISO at $ks_iso (scripts/iso.sh build with SITE_DIR set)"
  sudo rm -rf "$vmdir"
  mkdir -p "$vmdir"
  log "kickstart comparison in $FEDORA_IMAGE (logs: $vmdir/logs)"
  $PODMAN run --rm --name basalt-inst-test-kickstart --network=host --security-opt label=disable --device /dev/kvm \
    -v "$REPO_ROOT/scripts/ci:/ci:ro" -v "$ks_iso:/vm/installer.iso:ro" -v "$REPO_DIR:/repo:ro" \
    -v "$LAB_DIR/keys:/keys:ro" -v "$vmdir:/work" \
    -e SSH_PORT="$SSH_PORT" -e REPO_PORT="$REPO_PORT" -e HOST_OWNER="$(id -u):$(id -g)" \
    -e VM_MEMORY_MB="${VM_MEMORY_MB:-4096}" -e VM_VCPUS="${VM_VCPUS:-4}" \
    -e EXPECT_FEDORA_RELEASE="$FEDORA_RELEASE" -e EXPECT_BASALT_VERSION="$BASALT_VERSION" \
    "$FEDORA_IMAGE" /ci/vm-test.sh || rc=$?
  sudo rm -rf "$vmdir/state"
  [[ -f "$vmdir/logs/summary.txt" ]] && cat "$vmdir/logs/summary.txt" >&2
  return "$rc"
}

case "${1:-}" in
  kickstart) run_kickstart ;;
  iso) build_iso ;;
  tui | gui | hold) run_vm "$1" ;;
  *) sed -n '2,24p' "$0"; exit 2 ;;
esac
