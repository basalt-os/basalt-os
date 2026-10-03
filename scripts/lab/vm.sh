#!/usr/bin/env bash
# Lab VM for Basalt OS on libvirt/KVM: UEFI Secure Boot firmware (OVMF with
# enrolled keys), an emulated TPM 2.0 (swtpm), an isolated NAT network.
#
# The install runs inside the VM, so the disk is sealed to that VM's TPM:
#   1. an "installer" disk: Basalt OS written with bootc install --via-loopback
#      (plain btrfs, no encryption), booted first;
#   2. a blank "target" disk: the installer runs bootc install to-disk with
#      --block-setup tpm2-luks, then adds a recovery key (scripts/lab/install-target.sh).
#
#   vm.sh net-up               define and start the isolated network
#   vm.sh installer-disk       write the installer disk from the local image
#   vm.sh create               define and start the VM (installer + blank target)
#   vm.sh detach-installer     remove the installer disk; next boot is the target
#   vm.sh start | stop         boot (and wait for SSH) or shut down
#   vm.sh ssh [cmd]            ssh as root (uses the lab VM key)
#   vm.sh registry-auth        give the installed system pull credentials
#                              for the lab registry (/etc/ostree/auth.json)
#   vm.sh console              serial console (virsh console)
#   vm.sh sb show|disable|foreign-db|restore
#                              inspect or change the Secure Boot state in the
#                              VM's variable store (VM must be shut off)
#   vm.sh status | destroy
source "$(dirname "$0")/../lib.sh"

: "${VM_NAME:=basalt-lab-vm}"
: "${VM_NETWORK:=basalt-lab}"
: "${VM_SUBNET:=10.150.0}"
: "${VM_MAC:=52:54:00:15:00:10}"
: "${VM_MEMORY_MB:=4096}"
: "${VM_VCPUS:=2}"
: "${VM_DISK_GB:=20}"
: "${VM_DIR:=/var/lib/libvirt/images/basalt-lab}"
: "${VM_BRIDGE:=bsltlab0}"
: "${SSH_PUBKEY_FILE:?set SSH_PUBKEY_FILE}"
VIRSH="virsh -c qemu:///system"
VM_IP="${VM_SUBNET}.10"
SSH_KEY="$LAB_DIR/keys/vm_ed25519"
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5)

net_up() {
  if $VIRSH net-info "$VM_NETWORK" >/dev/null 2>&1; then
    $VIRSH net-start "$VM_NETWORK" >/dev/null 2>&1 || true
    log "network $VM_NETWORK exists"; return 0
  fi
  local xml; xml="$(mktemp)"
  cat >"$xml" <<EOF
<network>
  <name>${VM_NETWORK}</name>
  <forward mode='nat'/>
  <bridge name='${VM_BRIDGE}' stp='on' delay='0'/>
  <ip address='${VM_SUBNET}.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='${VM_SUBNET}.100' end='${VM_SUBNET}.199'/>
      <host mac='${VM_MAC}' name='${VM_NAME}' ip='${VM_IP}'/>
    </dhcp>
  </ip>
</network>
EOF
  run $VIRSH net-define "$xml"; rm -f "$xml"
  run $VIRSH net-autostart "$VM_NETWORK"
  run $VIRSH net-start "$VM_NETWORK"
}

installer_disk() {
  sudo install -d -m 0755 "$VM_DIR"
  local raw="$VM_DIR/installer.raw"
  sudo rm -f "$raw"
  sudo truncate -s 10G "$raw"
  log "writing the installer disk from $LOCAL_IMAGE:$BASALT_VERSION"
  run $PODMAN run --rm --privileged --pid=host \
    --security-opt label=type:unconfined_t \
    -v /var/lib/containers:/var/lib/containers -v /dev:/dev \
    -v "$VM_DIR:/output" -v "$SSH_PUBKEY_FILE:/run/basalt/authorized_keys:ro" \
    "$LOCAL_IMAGE:$BASALT_VERSION" \
    bootc install to-disk --via-loopback --generic-image --wipe --block-setup direct \
      --root-ssh-authorized-keys /run/basalt/authorized_keys \
      /output/installer.raw
}

create() {
  net_up
  [[ -f "$VM_DIR/installer.raw" ]] || die "no installer disk (vm.sh installer-disk)"
  sudo rm -f "$VM_DIR/target.qcow2"
  sudo qemu-img create -q -f qcow2 "$VM_DIR/target.qcow2" "${VM_DISK_GB}G"
  local xml; xml="$(mktemp)"
  virt-install --connect qemu:///system --name "$VM_NAME" \
    --memory "$VM_MEMORY_MB" --vcpus "$VM_VCPUS" --cpu host-passthrough \
    --machine q35 --features smm.state=on \
    --boot firmware=efi,firmware.feature0.name=secure-boot,firmware.feature0.enabled=yes,firmware.feature1.name=enrolled-keys,firmware.feature1.enabled=yes \
    --tpm backend.type=emulator,backend.version=2.0,model=tpm-crb \
    --disk "path=$VM_DIR/installer.raw,format=raw,bus=virtio,boot.order=1,serial=basalt-installer" \
    --disk "path=$VM_DIR/target.qcow2,format=qcow2,bus=virtio,boot.order=2,serial=basalt-target" \
    --network "network=$VM_NETWORK,mac=$VM_MAC,model=virtio" \
    --serial "pty,log.file=/var/log/libvirt/qemu/$VM_NAME-serial.log,log.append=on" \
    --osinfo linux2024 --graphics none \
    --noautoconsole --import --print-xml >"$xml"
  run $VIRSH define "$xml"; rm -f "$xml"
  tpm_manufacture
  run $VIRSH start "$VM_NAME"
  wait_ssh
}

# Create the vTPM state before the first start. libvirt normally runs
# swtpm_setup itself, but on the lab host (Fedora 44, selinux-policy of
# 2026-10) swtpm_t is denied access to swtpm_setup's pidfile in virtqemud's
# private /tmp (virtqemud_tmpfs_t), so manufacturing fails. With the state
# already present libvirt skips swtpm_setup and only runs swtpm, which works.
tpm_manufacture() {
  local uuid dir
  uuid="$($VIRSH domuuid "$VM_NAME")"
  dir="/var/lib/libvirt/swtpm/$uuid/tpm2"
  if sudo test -f "$dir/tpm2-00.permall"; then return 0; fi
  log "manufacturing vTPM state for $VM_NAME ($uuid)"
  sudo install -d -o tss -g tss -m 0711 "/var/lib/libvirt/swtpm/$uuid"
  sudo install -d -o tss -g tss -m 0700 "$dir"
  sudo swtpm_setup --tpm2 --tpmstate "$dir" --runas tss \
    --createek --create-ek-cert --create-platform-cert --lock-nvram \
    --pcr-banks sha256 --not-overwrite --vmid "$VM_NAME:$uuid" >/dev/null
  sudo restorecon -R "/var/lib/libvirt/swtpm/$uuid"
}

wait_ssh() {
  log "waiting for SSH on $VM_IP"
  local i
  for i in $(seq 1 120); do
    if ssh "${SSH_OPTS[@]}" "root@$VM_IP" true 2>/dev/null; then log "SSH up after ~$((i * 5))s"; return 0; fi
    sleep 5
  done
  die "no SSH on $VM_IP"
}

stop_vm() {
  if [[ "$($VIRSH domstate "$VM_NAME")" != "shut off" ]]; then
    $VIRSH shutdown "$VM_NAME" >/dev/null
    for _ in $(seq 1 60); do [[ "$($VIRSH domstate "$VM_NAME")" == "shut off" ]] && return 0; sleep 2; done
    die "$VM_NAME did not shut down"
  fi
}

detach_installer() {
  stop_vm
  run $VIRSH detach-disk "$VM_NAME" "$VM_DIR/installer.raw" --config
}

nvram_path() { $VIRSH dumpxml "$VM_NAME" | sed -n 's#.*<nvram[^>]*>\(.*\)</nvram>.*#\1#p'; }

require_off() {
  [[ "$($VIRSH domstate "$VM_NAME")" == "shut off" ]] || die "$VM_NAME must be shut off"
}

# Run virt-fw-vars from the lab tool image against the VM's variable store.
fwvars() {
  local nv; nv="$(nvram_path)"
  $PODMAN run --rm --security-opt label=disable -v "$(dirname "$nv"):/nv" \
    -v "$LAB_DIR/sb:/sb:ro" localhost/basalt-lab-tools \
    virt-fw-vars "$@" 2>&1
}

sb() {
  local nv; nv="$(nvram_path)"; local base; base="$(basename "$nv")"
  install -d -m 0700 "$LAB_DIR/sb"
  case "${1:-show}" in
    show)
      fwvars -i "/nv/$base" -p | grep -E 'name=(SecureBoot|SecureBootEnable|SetupMode|PK|KEK|db) |^ *(SecureBoot|SecureBootEnable|SetupMode)|subject|CN=' | head -40 ;;
    backup)
      require_off
      sudo cp --preserve=all "$nv" "$LAB_DIR/sb/nvram.orig" && log "saved $nv" ;;
    disable)
      require_off
      [[ -f "$LAB_DIR/sb/nvram.orig" ]] || sb backup
      fwvars --inplace "/nv/$base" --set-false SecureBootEnable ;;
    foreign-db)
      require_off
      [[ -f "$LAB_DIR/sb/nvram.orig" ]] || sb backup
      if [[ ! -f "$LAB_DIR/sb/foreign-db.pem" ]]; then
        openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=Basalt lab foreign db key" \
          -keyout "$LAB_DIR/sb/foreign-db.key" -out "$LAB_DIR/sb/foreign-db.pem" 2>/dev/null
      fi
      fwvars --inplace "/nv/$base" --add-db "$(uuidgen)" /sb/foreign-db.pem ;;
    restore)
      require_off
      [[ -f "$LAB_DIR/sb/nvram.orig" ]] || die "no saved variable store"
      sudo cp --preserve=all "$LAB_DIR/sb/nvram.orig" "$nv" && log "restored $nv" ;;
    *) die "usage: vm.sh sb show|backup|disable|foreign-db|restore" ;;
  esac
}

case "${1:-}" in
  net-up) net_up ;;
  installer-disk) installer_disk ;;
  create) create ;;
  detach-installer) detach_installer ;;
  wait-ssh) wait_ssh ;;
  start) run $VIRSH start "$VM_NAME"; wait_ssh ;;
  stop) stop_vm ;;
  ssh) shift; ssh "${SSH_OPTS[@]}" "root@$VM_IP" "$@" ;;
  registry-auth)
    # Pull credentials for bootc on the installed system (lab registry only).
    ssh "${SSH_OPTS[@]}" "root@$VM_IP" 'umask 077; install -d /etc/ostree; cat > /etc/ostree/auth.json' \
      <"$LAB_DIR/registry/auth.json" && log "installed /etc/ostree/auth.json" ;;
  console) exec $VIRSH console "$VM_NAME" ;;
  sb) shift; sb "$@" ;;
  status) $VIRSH dominfo "$VM_NAME"; $VIRSH domblklist "$VM_NAME"; $VIRSH net-dhcp-leases "$VM_NETWORK" ;;
  destroy)
    $VIRSH destroy "$VM_NAME" 2>/dev/null || true
    $VIRSH undefine "$VM_NAME" --nvram --tpm 2>/dev/null || true ;;
  *) sed -n '2,24p' "$0"; exit 2 ;;
esac
