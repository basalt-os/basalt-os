#!/usr/bin/env bash
# Boot test: unattended install of a test ISO in QEMU/KVM with Secure Boot
# and an emulated TPM, then smoke checks (scripts/ci/vm-test.sh).
#
#   scripts/ci/boot-test.sh prepare   throwaway keys, packages, signed test
#                                     repository, test ISO with site files
#   scripts/ci/boot-test.sh run       the install and the checks, in a Fedora
#                                     container with /dev/kvm
#   scripts/ci/boot-test.sh all       both (default)
#
# The test needs a repository dnf accepts, so `prepare` creates throwaway
# keys with scripts/lab/keys.sh in CI_LAB_DIR (default: a fresh directory
# under RUNNER_TEMP or /tmp): a repository signing key that exists for this
# run only and is never published, an SSH key for root on the VM, and site
# files for an unattended install. It never touches a lab's LAB_DIR. These
# are test keys, not release keys; CI holds no release keys.
#
# Outputs: $BUILD_DIR/ci-boot/ (packages, repository, ISO, VM state) and
# $BUILD_DIR/ci-boot/vm/logs/ (serial consoles, summary). Ports on the
# host's loopback: CI_SSH_PORT (2222) for SSH into the VM, CI_REPO_PORT
# (8098) for the repository.
source "$(dirname "$0")/../lib.sh"

: "${CI_SSH_PORT:=2222}"
: "${CI_REPO_PORT:=8098}"
: "${CI_LAB_DIR:=${RUNNER_TEMP:-/tmp}/basalt-ci-lab}"

# Everything below is test-only and kept apart from a lab's state.
export ISO_CACHE="${ISO_CACHE:-$BUILD_DIR/iso-cache}"
export BUILD_DIR="$BUILD_DIR/ci-boot"
export LAB_DIR="$CI_LAB_DIR"
export GNUPGHOME_LAB="$LAB_DIR/gpg"
export BASALT_GPG_PUBKEY="$LAB_DIR/gpg/RPM-GPG-KEY-basalt-lab"
# The OpenBasalt module certificates (no lab override): CI signs no modules.
export BASALT_MODULE_CA_CERT="" BASALT_MODULE_SIGNING_CERT="" EXTRA_SSH_PUBKEYS=""
export LAB_REPO_URL="http://10.0.2.2:$CI_REPO_PORT"
export REPO_DIR="$BUILD_DIR/repo"
export SITE_DIR="$LAB_DIR/site"
export SITE_NAME=ci
iso="$BUILD_DIR/iso/$(iso_name server "netinst-$SITE_NAME").iso"
vmdir="$BUILD_DIR/vm"

prepare() {
  [[ "$LAB_DIR" != / ]] || die "CI_LAB_DIR must not be /"
  log "throwaway test keys and site files in $LAB_DIR"
  "$REPO_ROOT/scripts/lab/keys.sh"
  "$REPO_ROOT/scripts/build-rpms.sh"
  rm -rf "$REPO_DIR"
  "$REPO_ROOT/scripts/repo.sh" publish
  "$REPO_ROOT/scripts/repo.sh" verify
  "$REPO_ROOT/scripts/iso.sh" fetch
  "$REPO_ROOT/scripts/iso.sh" build
}

run_vm() {
  [[ -f "$iso" ]] || die "no test ISO at $iso (scripts/ci/boot-test.sh prepare)"
  [[ -c /dev/kvm ]] || die "no /dev/kvm"
  sudo rm -rf "$vmdir"
  mkdir -p "$vmdir"
  log "boot test in $FEDORA_IMAGE (logs: $vmdir/logs)"
  local rc=0
  $PODMAN run --rm --network=host --security-opt label=disable --device /dev/kvm \
    -v "$REPO_ROOT/scripts/ci:/ci:ro" -v "$iso:/vm/installer.iso:ro" -v "$REPO_DIR:/repo:ro" \
    -v "$LAB_DIR/keys:/keys:ro" -v "$vmdir:/work" \
    -e SSH_PORT="$CI_SSH_PORT" -e REPO_PORT="$CI_REPO_PORT" -e HOST_OWNER="$(id -u):$(id -g)" \
    -e VM_MEMORY_MB="${VM_MEMORY_MB:-4096}" -e VM_VCPUS="${CI_VM_VCPUS:-2}" \
    -e INSTALL_TIMEOUT="${INSTALL_TIMEOUT:-3600}" \
    -e EXPECT_FEDORA_RELEASE="$FEDORA_RELEASE" -e EXPECT_BASALT_VERSION="$BASALT_VERSION" -e EXPECT_BASALT_STAGE="$BASALT_STAGE" \
    "$FEDORA_IMAGE" /ci/vm-test.sh || rc=$?
  # VM disk and firmware state are large and hold the throwaway keys' results;
  # only the logs are kept.
  sudo rm -rf "$vmdir/state"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" && -f "$vmdir/logs/summary.txt" ]]; then
    { echo "### Boot test"; echo; echo '```'; cat "$vmdir/logs/summary.txt"; echo '```'; } >>"$GITHUB_STEP_SUMMARY"
  fi
  return "$rc"
}

case "${1:-all}" in
  prepare) prepare ;;
  run) run_vm ;;
  all) prepare; run_vm ;;
  *) sed -n '2,21p' "$0"; exit 2 ;;
esac
