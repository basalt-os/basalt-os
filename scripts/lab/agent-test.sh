#!/usr/bin/env bash
# basalt-agent on a lab VM: install the package, then exercise both modes
# and the escape-test matrix. It syncs the test harness
# (packages/basalt-agent/tests) and the policy/binary build to the VM and
# runs packages/basalt-agent/tests/driver.sh there.
#
#   scripts/lab/agent-test.sh [setup|native|container|avc|all]   default: all
#
# The VM is selected by the usual VM_NAME/VM_HOST (.env). The agent package
# must already be installed on the VM (make rpm-agent repo, then dnf install
# on the VM), or set AGENT_DEV=1 to push a local `go build` and the compiled
# policy instead (developer loop; needs go on this host).
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
pkg="$REPO_ROOT/packages/basalt-agent"
: "${VM_NAME:=basalt-lab-vm}"
vm() { "$L/vm.sh" ssh "$@"; }

mode="${1:-all}"

log "syncing the test harness to the VM"
tar -C "$pkg" --exclude=./bin --exclude='*.pp*' --exclude=./selinux/tmp -czf - tests selinux dist |
  vm 'rm -rf /root/agent && mkdir -p /root/agent && tar -C /root/agent -xzf - &&
      rm -rf /opt/agent-tests && cp -r /root/agent/tests /opt/agent-tests && chmod -R a+rX /opt/agent-tests &&
      install -Dm644 /opt/agent-tests/labvictim.conf /etc/basalt-agent/profiles/labvictim.conf &&
      install -Dm644 /opt/agent-tests/labshell.conf /etc/basalt-agent/profiles/labshell.conf'

if [[ "${AGENT_DEV:-0}" == 1 ]]; then
  log "AGENT_DEV: pushing a local build and compiled policy"
  ( cd "$pkg" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=dev" -o /tmp/basalt-agent ./cmd/basalt-agent )
  tar -C /tmp -czf - basalt-agent | vm 'tar -C /root/agent -xzf - &&
    install -Dm755 /root/agent/basalt-agent /usr/bin/basalt-agent &&
    for n in exec proxy grant; do install -Dm755 /root/agent/basalt-agent /usr/libexec/basalt-agent/basalt-agent-$n; done &&
    mkdir -p /usr/share/basalt-agent && cp -r /root/agent/dist/profiles /root/agent/dist/egress /usr/share/basalt-agent/ &&
    install -Dm644 /root/agent/dist/basalt-agent.nft /usr/share/basalt-agent/basalt-agent.nft &&
    install -Dm644 /root/agent/dist/basalt-agent-egress.service /usr/lib/systemd/system/basalt-agent-egress.service &&
    install -Dm644 /root/agent/dist/org.basalt-os.agent.policy /usr/share/polkit-1/actions/org.basalt-os.agent.policy &&
    cd /root/agent/selinux && make -f /usr/share/selinux/devel/Makefile basalt_agent_base.pp basalt_agent.pp >/dev/null &&
    semodule -i basalt_agent_base.pp basalt_agent.pp basalt_agent_ports.cil &&
    restorecon -RF /usr/bin/basalt-agent /usr/libexec/basalt-agent &&
    systemctl daemon-reload && systemctl enable --now basalt-agent-egress.service'
fi

run() { log "driver.sh $1"; vm "bash /opt/agent-tests/driver.sh $1"; }
case "$mode" in
  setup|native|container|avc) run "$mode" ;;
  all)
    run setup
    run native
    run container
    log "SELinux denials of agent domains today (expect only the denied escape attempts, none during allowed work)"
    vm 'bash /opt/agent-tests/driver.sh avc today | tail -20 || true'
    ;;
  *) die "usage: agent-test.sh [setup|native|container|avc|all]" ;;
esac
log "agent-test done"
