#!/usr/bin/env bash
# Lab VMs for Basalt OS on libvirt/KVM: UEFI Secure Boot firmware (OVMF with
# the Microsoft and Red Hat keys enrolled), an emulated TPM 2.0 (swtpm, never
# the host's TPM), an isolated NAT network.
#
#   vm.sh net-up               define and start the isolated network
#   vm.sh install ISO          create the VM with a blank disk and the ISO,
#                              run the unattended install, wait until it
#                              powers off, then remove the ISO
#   vm.sh start | stop         boot (and wait for SSH) or shut down
#   vm.sh wait-ssh | wait-off
#   vm.sh ssh [cmd]            ssh as root (lab key)
#   vm.sh console              serial console (virsh console)
#   vm.sh sb show|backup|disable|foreign-db|restore|custom
#                              inspect or change the Secure Boot state in the
#                              VM's variable store (VM must be shut off);
#                              custom: replace PK, KEK and db with the lab's
#                              own keys (scripts/lab/sb-keys.sh), dbx kept
#   vm.sh status | destroy
#
# VM_NAME and VM_HOST (last octet of its address, default 10) select the VM,
# so several lab VMs can share the network.
source "$(dirname "$0")/../lib.sh"

: "${VM_NAME:=basalt-lab-vm}"
: "${VM_NETWORK:=basalt-lab}"
: "${VM_SUBNET:=10.150.0}"
: "${VM_MAC_PREFIX:=52:54:00:15:00}"
: "${VM_HOST:=10}"
: "${VM_MEMORY_MB:=8192}"
: "${VM_VCPUS:=4}"
: "${VM_DISK_GB:=30}"
: "${VM_DIR:=/var/lib/libvirt/images/basalt-lab}"
: "${VM_BRIDGE:=bsltlab0}"
: "${INSTALL_TIMEOUT:=3600}"
VIRSH="virsh -c qemu:///system"
VM_IP="${VM_SUBNET}.${VM_HOST}"
VM_MAC="${VM_MAC_PREFIX}:$(printf '%02x' "$VM_HOST")"
SSH_KEY="$LAB_DIR/keys/vm_ed25519"
SSH_OPTS=(-i "$SSH_KEY" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 -o BatchMode=yes)

state() {
  local s
  if s="$($VIRSH domstate "$VM_NAME" 2>/dev/null)"; then echo "${s%%$'\n'*}"; else echo undefined; fi
}

# Docker sets the iptables FORWARD policy to DROP, which also drops the
# libvirt network's NAT traffic (both rule sets must accept a packet). Allow
# forwarding for this lab bridge only, through Docker's DOCKER-USER chain.
# Not persistent: Docker recreates its chains on restart, so net-up re-adds it.
docker_forward_allow() {
  sudo iptables -n -L DOCKER-USER >/dev/null 2>&1 || return 0
  sudo iptables -C DOCKER-USER -i "$VM_BRIDGE" -j ACCEPT 2>/dev/null ||
    sudo iptables -I DOCKER-USER -i "$VM_BRIDGE" -j ACCEPT
  sudo iptables -C DOCKER-USER -o "$VM_BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
    sudo iptables -I DOCKER-USER -o "$VM_BRIDGE" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
}

net_up() {
  local xml h hosts=""
  if $VIRSH net-info "$VM_NETWORK" >/dev/null 2>&1; then
    $VIRSH net-start "$VM_NETWORK" >/dev/null 2>&1 || true
    # Fixed addresses .10 to .19 (older lab networks had .10 to .13).
    for h in $(seq 10 19); do
      $VIRSH net-dumpxml "$VM_NETWORK" | grep -q "ip='${VM_SUBNET}.${h}'" && continue
      $VIRSH net-update "$VM_NETWORK" add ip-dhcp-host \
        "<host mac='${VM_MAC_PREFIX}:$(printf '%02x' "$h")' ip='${VM_SUBNET}.${h}'/>" --live --config >/dev/null
    done
    docker_forward_allow
    log "network $VM_NETWORK exists"; return 0
  fi
  for h in $(seq 10 19); do
    hosts+="      <host mac='${VM_MAC_PREFIX}:$(printf '%02x' "$h")' ip='${VM_SUBNET}.${h}'/>"$'\n'
  done
  xml="$(mktemp)"
  cat >"$xml" <<EOF
<network>
  <name>${VM_NETWORK}</name>
  <forward mode='nat'/>
  <bridge name='${VM_BRIDGE}' stp='on' delay='0'/>
  <ip address='${VM_SUBNET}.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='${VM_SUBNET}.100' end='${VM_SUBNET}.199'/>
${hosts}    </dhcp>
  </ip>
</network>
EOF
  run $VIRSH net-define "$xml"; rm -f "$xml"
  run $VIRSH net-autostart "$VM_NETWORK"
  run $VIRSH net-start "$VM_NETWORK"
  docker_forward_allow
}

# Create the vTPM state before the first start. libvirt normally runs
# swtpm_setup itself, but on Fedora 44 hosts (selinux-policy of 2026-10)
# swtpm_t is denied access to swtpm_setup's pidfile in virtqemud's private
# /tmp, so manufacturing fails. With the state already present libvirt only
# runs swtpm, which works. The state is per VM and emulated: the host's own
# TPM is never used.
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

install_vm() {
  local iso="${1:?ISO path}"
  [[ "$(state)" == undefined ]] || die "$VM_NAME exists (vm.sh destroy first)"
  net_up
  sudo install -d -m 0755 "$VM_DIR"
  sudo cp "$iso" "$VM_DIR/installer-$VM_NAME.iso"
  sudo rm -f "$VM_DIR/$VM_NAME.qcow2"
  sudo qemu-img create -q -f qcow2 "$VM_DIR/$VM_NAME.qcow2" "${VM_DISK_GB}G"
  local xml; xml="$(mktemp)"
  virt-install --connect qemu:///system --name "$VM_NAME" \
    --memory "$VM_MEMORY_MB" --vcpus "$VM_VCPUS" --cpu host-passthrough \
    --machine q35 --features smm.state=on \
    --boot firmware=efi,firmware.feature0.name=secure-boot,firmware.feature0.enabled=yes,firmware.feature1.name=enrolled-keys,firmware.feature1.enabled=yes \
    --tpm backend.type=emulator,backend.version=2.0,model=tpm-crb \
    --disk "path=$VM_DIR/$VM_NAME.qcow2,format=qcow2,bus=virtio,boot.order=2,serial=basalt-disk" \
    --disk "path=$VM_DIR/installer-$VM_NAME.iso,device=cdrom,bus=sata,readonly=on,boot.order=1" \
    --network "network=$VM_NETWORK,mac=$VM_MAC,model=virtio" \
    --serial "pty,log.file=/var/log/libvirt/qemu/$VM_NAME-serial.log,log.append=on" \
    --rng /dev/urandom \
    --osinfo fedora-unknown --graphics none \
    --noautoconsole --import --print-xml >"$xml"
  run $VIRSH define "$xml"; rm -f "$xml"
  tpm_manufacture
  local t0; t0=$(date +%s)
  run $VIRSH start "$VM_NAME"
  log "installing (the kickstart powers the VM off when done; timeout ${INSTALL_TIMEOUT}s)"
  wait_off "$INSTALL_TIMEOUT"
  log "install finished in $(( $(date +%s) - t0 ))s"
  eject_installer
}

eject_installer() {
  local target
  target="$($VIRSH domblklist "$VM_NAME" --details | awk '$2 == "cdrom" && !t {t = $3} END {print t}')"
  [[ -n "$target" ]] || return 0
  run $VIRSH detach-disk "$VM_NAME" "$target" --config
  sudo rm -f "$VM_DIR/installer-$VM_NAME.iso"
}

wait_off() {
  local timeout="${1:-180}" i
  for ((i = 0; i < timeout; i += 5)); do
    [[ "$(state)" == "shut off" ]] && return 0
    sleep 5
  done
  die "$VM_NAME still running after ${timeout}s"
}

wait_ssh() {
  local timeout="${1:-600}" t0 i
  t0=$(date +%s)
  log "waiting for SSH on $VM_IP"
  for ((i = 0; i < timeout; i += 2)); do
    if ssh "${SSH_OPTS[@]}" "root@$VM_IP" true 2>/dev/null; then
      log "SSH up after $(( $(date +%s) - t0 ))s"; return 0
    fi
    sleep 2
  done
  die "no SSH on $VM_IP after ${timeout}s"
}

stop_vm() {
  [[ "$(state)" == "shut off" ]] && return 0
  $VIRSH shutdown "$VM_NAME" >/dev/null
  for _ in $(seq 1 90); do [[ "$(state)" == "shut off" ]] && return 0; sleep 2; done
  log "$VM_NAME did not shut down in 180s; forcing it off"
  $VIRSH destroy "$VM_NAME" >/dev/null
}

nvram_path() { $VIRSH dumpxml "$VM_NAME" | sed -n 's#.*<nvram[^>]*>\(.*\)</nvram>.*#\1#p'; }
require_off() { [[ "$(state)" == "shut off" ]] || die "$VM_NAME must be shut off"; }

# Run virt-fw-vars from the lab tool image against the VM's variable store.
fwvars() {
  local nv; nv="$(nvram_path)"
  $PODMAN run --rm --security-opt label=disable -v "$(dirname "$nv"):/nv" \
    -v "$LAB_DIR/sb:/sb:ro" localhost/basalt-lab-tools \
    virt-fw-vars "$@" 2>&1
}

sb() {
  local nv base sbdir="$LAB_DIR/sb/$VM_NAME"
  nv="$(nvram_path)"; base="$(basename "$nv")"
  install -d -m 0700 "$sbdir"
  case "${1:-show}" in
    show)
      fwvars -i "/nv/$base" -p | grep -E 'name=(SecureBoot|SecureBootEnable|SetupMode|PK|KEK|db) |^ *(SecureBoot|SecureBootEnable|SetupMode)|subject|CN=' | head -40 ;;
    backup)
      require_off
      sudo cp --preserve=all "$nv" "$sbdir/nvram.orig" && log "saved $nv" ;;
    disable)
      require_off
      [[ -f "$sbdir/nvram.orig" ]] || sb backup
      fwvars --inplace "/nv/$base" --set-false SecureBootEnable ;;
    foreign-db)
      require_off
      [[ -f "$sbdir/nvram.orig" ]] || sb backup
      if [[ ! -f "$LAB_DIR/sb/foreign-db.pem" ]]; then
        openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=Basalt lab foreign db key" \
          -keyout "$LAB_DIR/sb/foreign-db.key" -out "$LAB_DIR/sb/foreign-db.pem" 2>/dev/null
      fi
      fwvars --inplace "/nv/$base" --add-db "$(uuidgen)" /sb/foreign-db.pem ;;
    custom)
      # Custom db mode: the lab's PK, KEK and db replace the Microsoft and
      # Red Hat keys; dbx (revocations) is kept. The VM then boots only EFI
      # binaries signed with the lab db key (see scripts/lab/sb-custom-test.sh).
      require_off
      [[ -f "$sbdir/nvram.orig" ]] || sb backup
      [[ -s "$LAB_DIR/sb-keys/db.pem" ]] || die "no lab keys (scripts/lab/sb-keys.sh)"
      local g; g="$(cat "$LAB_DIR/sb-keys/guid")"
      $PODMAN run --rm --security-opt label=disable -v "$(dirname "$nv"):/nv" \
        -v "$LAB_DIR/sb-keys:/k:ro" localhost/basalt-lab-tools \
        virt-fw-vars --inplace "/nv/$base" -d PK -d KEK -d db \
          --set-pk "$g" /k/PK.pem --add-kek "$g" /k/KEK.pem --add-db "$g" /k/db.pem --secure-boot 2>&1 | tail -3 ;;
    restore)
      require_off
      [[ -f "$sbdir/nvram.orig" ]] || die "no saved variable store"
      sudo cp --preserve=all "$sbdir/nvram.orig" "$nv" && log "restored $nv" ;;
    *) die "usage: vm.sh sb show|backup|disable|foreign-db|restore|custom" ;;
  esac
}

case "${1:-}" in
  net-up) net_up ;;
  install) shift; install_vm "$@" ;;
  eject) eject_installer ;;
  boot-iso)
    # Boot an ISO once (VM shut off): attached as a read-only SATA CD-ROM,
    # first in the boot order; `vm.sh eject` removes it again.
    shift; require_off
    sudo cp "${1:?ISO}" "$VM_DIR/installer-$VM_NAME.iso"
    xml="$(mktemp)"
    cat >"$xml" <<EOF
<disk type='file' device='cdrom'>
  <driver name='qemu' type='raw'/>
  <source file='$VM_DIR/installer-$VM_NAME.iso'/>
  <target dev='sdz' bus='sata'/>
  <readonly/>
  <boot order='1'/>
</disk>
EOF
    run $VIRSH attach-device "$VM_NAME" "$xml" --config; rm -f "$xml" ;;
  wait-ssh) shift; wait_ssh "$@" ;;
  wait-off) shift; wait_off "$@" ;;
  start) run $VIRSH start "$VM_NAME"; wait_ssh ;;
  stop) stop_vm ;;
  ssh) shift; ssh "${SSH_OPTS[@]}" "root@$VM_IP" "$@" ;;
  scp-from) shift; scp "${SSH_OPTS[@]}" "root@$VM_IP:$1" "$2" ;;
  scp-to) shift; scp "${SSH_OPTS[@]}" "$1" "root@$VM_IP:$2" ;;
  ip) echo "$VM_IP" ;;
  console) exec $VIRSH console "$VM_NAME" ;;
  sb) shift; sb "$@" ;;
  status) $VIRSH dominfo "$VM_NAME"; $VIRSH domblklist "$VM_NAME"; $VIRSH net-dhcp-leases "$VM_NETWORK" ;;
  destroy)
    $VIRSH destroy "$VM_NAME" 2>/dev/null || true
    $VIRSH undefine "$VM_NAME" --nvram --tpm 2>/dev/null || true
    sudo rm -f "$VM_DIR/$VM_NAME.qcow2" "$VM_DIR/installer-$VM_NAME.iso" ;;
  *) sed -n '2,24p' "$0"; exit 2 ;;
esac
