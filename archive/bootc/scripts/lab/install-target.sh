#!/usr/bin/env bash
# Install Basalt OS onto the lab VM's blank target disk, from inside the VM
# (booted from the installer disk), with basalt-install: btrfs on LUKS2, TPM2
# bound to PCR 7, recovery key. The image is pulled from the registry, so the
# VM's signature policy is enforced on that pull, and the installed system is
# set to follow ${IMAGE}:${CHANNEL_TAG} with the policy enforced on updates.
#
# The recovery key is saved on the build host, never printed:
#   $LAB_DIR/recovery/$VM_NAME.txt (0600)
source "$(dirname "$0")/../lib.sh"
lab_registry_opts

: "${VM_NAME:=basalt-lab-vm}"
: "${TARGET_DISK:=/dev/disk/by-id/virtio-basalt-target}"
vm() { "$REPO_ROOT/scripts/lab/vm.sh" ssh "$@"; }
ref="$IMAGE:$CHANNEL_TAG"

install -d -m 0700 "$LAB_DIR/recovery"
keyfile="$LAB_DIR/recovery/$VM_NAME.txt"

# Registry credentials go to tmpfs in the VM, through stdin.
if [[ -f "${LAB_AUTH_FILE:-}" ]]; then
  vm 'umask 077; install -d /run/basalt; cat > /run/basalt/auth.json' <"$LAB_AUTH_FILE"
fi
vm 'install -d /run/basalt; cat > /run/basalt/authorized_keys' <"$SSH_PUBKEY_FILE"

log "pulling $ref inside the VM (signature policy enforced)"
start=$(date +%s)
vm "podman pull --quiet --authfile /run/basalt/auth.json $ref"
log "pull took $(($(date +%s) - start))s"

log "running basalt-install on $TARGET_DISK"
start=$(date +%s)
umask 077
vm "podman run --rm --privileged --pid=host --security-opt label=type:unconfined_t \
  -v /var/lib/containers:/var/lib/containers -v /dev:/dev -v /run/basalt:/run/basalt:ro \
  $ref basalt-install $TARGET_DISK -- \
  --target-imgref $ref --enforce-container-sigpolicy \
  --root-ssh-authorized-keys /run/basalt/authorized_keys" >"$keyfile"
log "install took $(($(date +%s) - start))s; recovery key saved to $keyfile ($(wc -c <"$keyfile") bytes)"
[[ "$(tr -d '[:space:]' <"$keyfile")" =~ ^([a-z]{8}-){7}[a-z]{8}$ ]] || die "recovery key file looks wrong"
