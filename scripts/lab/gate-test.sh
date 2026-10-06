#!/usr/bin/env bash
# basalt-gate on a lab VM with SELinux enforcing: the service and its
# domain, a request a person approves at the terminal with polkit, claims
# (wrong executor, replay, another digest), a rule added through the gate
# that then approves by itself, a dry run, locked actions refused whatever
# the rules say, the emergency stop pulled by an agent and resumed at the
# terminal, escapes from an agent domain (decide, read or write the rule
# store, raise its own rule, claim, enter the decider domain), a tampered
# rule file, the ledger records and the SELinux denials. It installs the
# RPMs given, syncs the harness (packages/basalt-gate/tests) to the VM and
# runs its driver.sh there.
#
#   GATE_RPMS=DIR scripts/lab/gate-test.sh [PHASE...]   default: all phases
#
# Phases: install setup service request claims rule locked stop escape seal ledger avc.
# GATE_RPMS holds basalt-gate, basalt-gate-selinux, basalt-ledger and
# basalt-ledger-selinux (install phase). The VM (VM_NAME/VM_HOST from .env,
# or GATE_VM_SSH, a command that runs its arguments on the VM as root)
# needs basalt-agent's SELinux module and the users dev and other
# (scripts/lab/agent-test.sh setup).
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
vm() {
  if [[ -n "${GATE_VM_SSH:-}" ]]; then
    # shellcheck disable=SC2086 # GATE_VM_SSH is a command line on purpose
    $GATE_VM_SSH "$@"
  else
    "$L/vm.sh" ssh "$@"
  fi
}

phases=("$@")
[[ ${#phases[@]} -eq 0 ]] && phases=(install setup service request claims rule locked stop escape seal ledger avc)

log "syncing the test harness to the VM"
tar -C "$REPO_ROOT/packages/basalt-gate" -czf - tests |
  vm 'rm -rf /opt/gate-tests && mkdir -p /opt/gate-tests && tar -C /opt/gate-tests --strip-components=1 -xzf - &&
      chmod -R a+rX /opt/gate-tests && chmod a+x /opt/gate-tests/*.sh /opt/gate-tests/*.py'

since=$(vm "date +%s")
for p in "${phases[@]}"; do
  case "$p" in
    install)
      [[ -d "${GATE_RPMS:-}" ]] || die "GATE_RPMS=DIR with the basalt-gate and basalt-ledger RPMs"
      log "installing the RPMs from $GATE_RPMS"
      tar -C "$GATE_RPMS" -czf - . | vm 'rm -rf /root/rpms-gate && mkdir -p /root/rpms-gate && tar -C /root/rpms-gate -xzf - &&
        rpms=$(ls /root/rpms-gate/*.rpm | grep -Ev "\.src\.rpm$|debug") &&
        dnf -y -q install --nogpgcheck $rpms >/dev/null && rpm -Uvh --replacepkgs --nosignature $rpms >/dev/null &&
        rpm -q basalt-gate basalt-gate-selinux basalt-ledger basalt-ledger-selinux && semodule -l | grep -E "^basalt_(gate|ledger|agent)"'
      ;;
    avc) log "driver.sh avc"; vm "bash /opt/gate-tests/driver.sh avc '$since'" ;;
    *) log "driver.sh $p"; vm "bash /opt/gate-tests/driver.sh $p" ;;
  esac
done
log "gate-test done"
