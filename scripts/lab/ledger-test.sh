#!/usr/bin/env bash
# basalt-resolver and basalt-ledger on a lab VM: per-session default-deny
# egress (allowed work, escape attempts, DNS rebinding, fail-closed
# restart) and the audit ledger (append-only, reads, collectors, sealed
# rotation, tamper evidence, signed export). It syncs the harness
# (packages/basalt-ledger/tests) to the VM and runs its driver.sh there.
#
#   scripts/lab/ledger-test.sh [PHASE...]   default: all phases
#
# Phases: setup egress proxyonly container ledger events rotate keys retention restart avc report.
# The VM (VM_NAME/VM_HOST from .env) must have basalt-agent, basalt-resolver
# and basalt-ledger installed and basalt-agent's lab setup done
# (scripts/lab/agent-test.sh setup).
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
: "${VM_NAME:=basalt-lab-vm}"
vm() { "$L/vm.sh" ssh "$@"; }

phases=("$@")
[[ ${#phases[@]} -eq 0 ]] && phases=(setup egress proxyonly container ledger events rotate keys retention restart avc report)

log "syncing the test harness to the VM"
tar -C "$REPO_ROOT/packages/basalt-ledger" -czf - tests |
  vm 'rm -rf /opt/ledger-tests && mkdir -p /opt/ledger-tests && tar -C /opt/ledger-tests --strip-components=1 -xzf - &&
      chmod -R a+rX /opt/ledger-tests && chmod a+x /opt/ledger-tests/*.sh'

since=$(vm "date +%s")
for p in "${phases[@]}"; do
  log "driver.sh $p"
  case "$p" in
    avc|report) vm "bash /opt/ledger-tests/driver.sh $p '$since'" ;;
    *) vm "bash /opt/ledger-tests/driver.sh $p" ;;
  esac
done
log "ledger-test done"
