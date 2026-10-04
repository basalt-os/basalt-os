#!/usr/bin/env bash
# System assistant (basalt-assistant) on a lab VM: events -> diagnosis ->
# proposal -> confirmed apply -> verified fix -> audit, and rollback.
#
#   scripts/lab/assistant-test.sh [STEP...]     default: all
#
# Steps (VM_NAME / VM_HOST select the VM; it needs nginx from Fedora):
#   reset     lab only: undo earlier runs (nginx config, port label, test
#             directory, fixture package, stored proposals)
#   setup     install basalt-assistant and nginx from the repositories, lab
#             timings for the daemon (disk check every 30 s), daemon started
#   config    break nginx.conf: the daemon hints at restoring it from a
#             snapshot (it cannot check the copy); `basalt confirm` checks the
#             copy as root and stores the proposal; confirmed apply; nginx
#             answers; then the apply is undone with
#             `basalt snapshots rollback --before ID` and a reboot
#   sandbox   `basalt why nginx` as root with log paths that do not exist:
#             the config checker runs in a throwaway overlay, nothing is
#             created on the system
#   selinux   nginx logs to a directory moved from /root (wrong label, the
#             denial is hidden by a dontaudit rule): semanage fcontext +
#             restorecon proposed and applied; nginx up, no new denials
#   port      nginx on an unlabeled port (logged AVC): semanage port proposed,
#             applied, verified
#   dnf       a package whose %post fails: the daemon hints at rolling back
#             to the pre snapshot; confirmed as root; applied; after the
#             reboot the package is gone
#   disk      a large file held only by a snapshot fills the disk: the daemon
#             reports it, `basalt disk` (root) finds the snapshot, its
#             deletion is applied and the space is back
#   confine   the daemon runs in basalt_assistant_t, may not write outside its
#             state directory (probe through systemd-run), 0 denials
#   mcp       the MCP server: tools, a proposal stored, nothing executed
#   audit     the audit chain verifies; recent records
# Every apply is non-interactive here: the confirmation code printed with
# the proposal is typed back (--yes --confirm CODE), as a person would.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
vm() { "$L/vm.sh" ssh "$@"; }
FIX="$LAB_DIR/assistant"
: "${WAIT_PROPOSAL:=150}"

denials_since() { vm "ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts $1 2>/dev/null | grep -c '^type=AVC' || true"; }
now_ts() { vm 'date +%H:%M:%S'; }

# wait_proposal KIND PATTERN: id of the newest pending proposal of KIND whose
# title matches PATTERN, created by the daemon (polls `basalt pending`).
# Proposals that existed before the step (see mark) are not taken.
mark() { SEEN="$(vm 'basalt pending --all' | awk '/^p-/ {print $1}' | paste -sd' ')"; }
SEEN=""
wait_proposal() {
  local kind="$1" pat="$2" t0 id
  t0=$(date +%s)
  while (( $(date +%s) - t0 < WAIT_PROPOSAL )); do
    id="$(vm "basalt pending" | awk -v k="$kind" -v p="$pat" -v seen=" $SEEN " \
      '$2 == "pending" && $3 == k && index($0, p) && !index(seen, " " $1 " ") {print $1; exit}')"
    if [[ -n "$id" ]]; then
      log "proposal $id after $(( $(date +%s) - t0 ))s"
      echo "$id"; return 0
    fi
    sleep 5
  done
  vm 'basalt pending; journalctl -u basalt-assistantd -n 30 --no-pager -o cat' >&2 || true
  die "no $kind proposal matching '$pat' within ${WAIT_PROPOSAL}s"
}

code_of() { vm "basalt show $1" | sed -n 's/.*--confirm \([0-9a-f]\{8\}\)).*/\1/p' | head -1; }

# confirm_hint ID: `basalt confirm` as root; prints the id of the proposal it
# stores (the daemon's restores and rollbacks are hints).
confirm_hint() {
  local out new
  out="$(vm "basalt confirm $1")" || { printf '%s\n' "$out" >&2; die "hint $1 not confirmed"; }
  printf '%s\n' "$out" >&2
  new="$(sed -n 's/.*stored as proposal \(p-[0-9a-f]*\).*/\1/p' <<<"$out" | head -1)"
  [[ -n "$new" ]] || die "basalt confirm $1 stored no proposal"
  echo "$new"
}

# apply_proposal ID: show it, apply it with its confirmation code.
apply_proposal() {
  local id="$1" code
  vm "basalt show $id"
  code="$(code_of "$id")"
  [[ -n "$code" ]] || die "$id has no confirmation code (a report, or not pending)"
  log "applying $id (confirmation code $code)"
  vm "basalt apply $id --yes --confirm $code" || die "$id did not apply or did not verify"
}

reboot_vm() {
  vm 'systemctl reboot' || true
  sleep 10
  "$L/vm.sh" wait-ssh 300
  vm 'systemctl is-system-running --wait >/dev/null 2>&1 || true'
}

restore_nginx() {
  vm 'cp /root/nginx.conf.orig /etc/nginx/nginx.conf && systemctl restart nginx && systemctl is-active nginx'
}

fixtures() {
  # Lab-only packages built here: one whose %post fails.
  [[ -f "$FIX/basalt-lab-postfail-1-1.noarch.rpm" ]] && return 0
  mkdir -p "$FIX"
  in_fedora -v "$FIX:/out" "$FEDORA_IMAGE" bash -euc '
    dnf -q -y install rpm-build >/dev/null 2>&1
    mkdir -p /rb/SPECS
    cat >/rb/SPECS/postfail.spec <<EOF
Name: basalt-lab-postfail
Version: 1
Release: 1
Summary: Lab fixture whose post-install scriptlet fails
License: Apache-2.0
BuildArch: noarch
%description
Lab fixture: installed, then its %%post scriptlet exits 1.
%install
mkdir -p %{buildroot}/usr/share/basalt-lab-postfail
echo postfail > %{buildroot}/usr/share/basalt-lab-postfail/marker
%post
echo "basalt-lab-postfail: setup fails on purpose" >&2
exit 1
%files
/usr/share/basalt-lab-postfail
EOF
    rpmbuild --define "_topdir /rb" -bb /rb/SPECS/postfail.spec >/dev/null 2>&1
    cp /rb/RPMS/noarch/*.rpm /out/'
  sudo chown -R "$(id -u):$(id -g)" "$FIX"
}

step_reset() {
  log "reset (lab only): nginx config, port label, test directory, fixture package, proposals"
  vm 'cp /root/nginx.conf.orig /etc/nginx/nginx.conf 2>/dev/null; systemctl restart nginx
      semanage port -d -t http_port_t -p tcp 8085 2>/dev/null; semanage fcontext -d "/srv/nginx-logs(/.*)?" 2>/dev/null
      rm -rf /srv/nginx-logs; rpm -q basalt-lab-postfail >/dev/null && dnf -y -q remove basalt-lab-postfail
      rm -f /var/lib/basalt-assistant/proposals/*.json; systemctl restart basalt-assistantd; basalt status' || true
}

step_setup() {
  log "setup: basalt-assistant and nginx"
  vm 'dnf -y -q --refresh install basalt-assistant nginx' | tail -3
  vm 'rpm -q basalt-assistant basalt-assistant-selinux nginx; semodule -l | grep basalt_assistant'
  vm '[ -f /root/nginx.conf.orig ] || cp -p /etc/nginx/nginx.conf /root/nginx.conf.orig'
  # Lab timings: disk usage every 30 s (default 5 min).
  vm "sed -i 's/^disk_interval = .*/disk_interval = 30s/' /etc/basalt/assistant.conf"
  vm 'systemctl enable --now nginx basalt-assistantd && systemctl restart basalt-assistantd && sleep 2 && systemctl is-active basalt-assistantd'
  vm 'basalt status'
}

step_config() {
  mark
  log "config: a broken nginx.conf"
  vm 'sed -i "s/^\(\s*\)server_name .*;/&\n\1bogus_directive on;/" /etc/nginx/nginx.conf; systemctl restart nginx || true'
  local hint id; hint="$(wait_proposal unit 'configuration error')"
  vm "basalt show $hint" | grep -q 'basalt confirm' || die "$hint: the daemon proposed a restore instead of a hint"
  id="$(confirm_hint "$hint")"
  apply_proposal "$id"
  vm 'curl -s -o /dev/null -w "nginx answers: HTTP %{http_code}\n" http://localhost/; nginx -t 2>&1 | tail -1'
  log "undo the apply through its pre snapshot"
  local out rid code
  out="$(vm "basalt snapshots rollback --before $id --yes")"
  printf '%s\n' "$out"
  rid="$(sed -n 's/^\[\(p-[0-9a-f]*\)\].*/\1/p' <<<"$out" | head -1)"
  code="$(sed -n 's/.*--confirm \([0-9a-f]\{8\}\)).*/\1/p' <<<"$out" | head -1)"
  [[ -n "$rid" && -n "$code" ]] || die "no rollback proposal"
  vm "basalt apply $rid --yes --confirm $code" || die "rollback did not verify"
  reboot_vm
  if vm 'grep -q bogus_directive /etc/nginx/nginx.conf'; then
    log "after the rollback the broken nginx.conf is back, as before the apply"
  else
    die "rollback did not bring the pre-apply state back"
  fi
  vm 'basalt audit 6'
  restore_nginx
}

step_selinux() {
  mark
  log "selinux: nginx logs to a directory moved from /root"
  local t; t="$(now_ts)"
  vm 'semanage fcontext -d "/srv/nginx-logs(/.*)?" 2>/dev/null; rm -rf /srv/nginx-logs; mkdir /root/nginx-logs && mv /root/nginx-logs /srv/ && ls -dZ /srv/nginx-logs
      sed -i "s#access_log  /var/log/nginx/access.log  main;#access_log  /srv/nginx-logs/access.log  main;#" /etc/nginx/nginx.conf
      systemctl restart nginx || true'
  log "AVC records for it: $(denials_since "$t") (a dontaudit rule hides the denial)"
  local id; id="$(wait_proposal unit 'blocked by SELinux')"
  apply_proposal "$id"
  vm 'ls -dZ /srv/nginx-logs; systemctl is-active nginx; tail -1 /srv/nginx-logs/access.log 2>/dev/null; curl -s -o /dev/null http://localhost/; ls -Z /srv/nginx-logs'
  restore_nginx
}

step_port() {
  mark
  log "port: nginx on tcp 8085 (unreserved_port_t)"
  vm 'seinfo --portcon=8085 | grep "tcp 8085" || echo "8085 has no own label"; sed -i "s/listen       80;/listen       8085;/" /etc/nginx/nginx.conf; systemctl restart nginx || true'
  local id; id="$(wait_proposal unit 'blocked by SELinux')"
  apply_proposal "$id"
  vm 'systemctl restart nginx && curl -s -o /dev/null -w "nginx on 8085: HTTP %{http_code}\n" http://localhost:8085/; semanage port -l -C'
  restore_nginx
}

step_dnf() {
  mark
  fixtures
  log "dnf: a package whose %post scriptlet fails"
  "$L/vm.sh" scp-to "$FIX/basalt-lab-postfail-1-1.noarch.rpm" /root/
  vm 'dnf -y install /root/basalt-lab-postfail-1-1.noarch.rpm 2>&1 | tail -4; rpm -q basalt-lab-postfail'
  local hint id; hint="$(wait_proposal dnf 'basalt-lab-postfail')"
  id="$(confirm_hint "$hint")"
  apply_proposal "$id"
  reboot_vm
  if vm 'rpm -q basalt-lab-postfail'; then die "the package is still installed after the rollback"; fi
  log "after the rollback the half-installed package is gone"
  vm 'basalt status'
}

step_sandbox() {
  log "sandbox: the config checker of a root diagnosis changes nothing"
  # nginx -t opens (and so creates) the log files its configuration names:
  # an empty log directory must stay empty after `basalt why nginx`.
  vm 'rm -rf /var/log/basalt-lab-sandbox; mkdir /var/log/basalt-lab-sandbox
      sed -i "s#access_log  /var/log/nginx/access.log  main;#access_log  /var/log/basalt-lab-sandbox/access.log  main;#" /etc/nginx/nginx.conf
      sed -i "s#^error_log .*#error_log /var/log/basalt-lab-sandbox/error.log;#" /etc/nginx/nginx.conf
      basalt why nginx --json | python3 -c "import json,sys; c=json.load(sys.stdin).get(\"config_check\") or {}; print(\"checker:\", c.get(\"command\"), \"passed\" if c.get(\"ok\") else \"failed\")"
      ls -A /var/log/basalt-lab-sandbox'
  if [[ -n "$(vm 'ls -A /var/log/basalt-lab-sandbox')" ]]; then die "the config checker created files in /var/log/basalt-lab-sandbox"; fi
  log "nothing created by the config checker"
  vm 'rm -rf /var/log/basalt-lab-sandbox'
  restore_nginx
}

step_disk() {
  # Any pending disk proposal counts: the daemon folds repeats into it.
  SEEN=""
  log "disk: a large file held only by a snapshot"
  vm 'df -h / | tail -1
      avail=$(df -B1 --output=avail / | tail -1); size=$(( avail * 88 / 100 ))
      fallocate -l "$size" /opt/basalt-filler && snapper -c root create -d "lab: snapshot that keeps a large file"
      rm -f /opt/basalt-filler; sync; btrfs filesystem sync /; df -h / | tail -1'
  local id; id="$(wait_proposal disk 'full')"
  vm "basalt show $id"
  log "the daemon cannot measure snapshot space (confined); basalt disk as root"
  local out pid
  out="$(vm 'basalt disk')"
  printf '%s\n' "$out"
  pid="$(sed -n 's/^\[\(p-[0-9a-f]*\)\].*/\1/p' <<<"$out" | head -1)"
  [[ -n "$pid" ]] || die "basalt disk proposed nothing"
  apply_proposal "$pid"
  vm 'df -h / | tail -1'
  if [[ "$pid" != "$id" ]]; then vm "basalt ignore $id --reason 'handled by $pid'"; fi
}

step_confine() {
  log "confinement"
  vm 'ps -eo label,pid,comm | grep basalt-assistan'
  log "denials of basalt_assistant_t this boot: $(vm "ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c basalt_assistant_t || true")"
  local t; t="$(now_ts)"
  # The probe runs the daemon binary in its domain (systemd-run, no unit
  # hardening, so only SELinux decides) and tries to create files.
  vm 'systemd-run --wait --pipe --quiet /usr/libexec/basalt/basalt-assistantd --probe-write \
        /etc/basalt-probe /root/basalt-probe /var/tmp/basalt-probe /srv/basalt-probe /var/log/basalt-probe \
        /var/log/basalt-assistant/probe /var/lib/basalt-assistant/probe' || true
  log "denials caused by the probe (expected):"
  vm "ausearch --input-logs -m AVC -ts $t 2>/dev/null | grep -o 'avc: .*' | sed 's/ ino=[0-9]*//; s/pid=[0-9]* //' | sort -u" || true
}

step_mcp() {
  log "mcp: tools over stdio"
  vm 'printf "%s\n" \
   "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"clientInfo\":{\"name\":\"lab\",\"version\":\"1\"}}}" \
   "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}" \
   "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"basalt_why_unit\",\"arguments\":{\"unit\":\"nginx\"}}}" \
   "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"basalt_propose_action\",\"arguments\":{\"kind\":\"selinux.boolean\",\"params\":{\"name\":\"httpd_can_network_connect\",\"value\":\"on\"},\"reason\":\"lab test\"}}}" \
   | basalt-mcp | python3 -c "
import json, sys
for line in sys.stdin:
    r = json.loads(line)
    res = r.get(\"result\", {})
    if \"tools\" in res:
        print(\"tools:\", \" \".join(t[\"name\"] for t in res[\"tools\"]))
    elif \"content\" in res:
        print(\"call\", r[\"id\"], \"error\" if res.get(\"isError\") else \"ok\", res[\"content\"][0][\"text\"].splitlines()[-1])
    else:
        print(\"init\", res.get(\"serverInfo\"))
"'
  vm 'getsebool httpd_can_network_connect; basalt pending | grep action'
}

step_audit() {
  vm 'basalt audit verify; basalt audit 25; lsattr /var/log/basalt-assistant/audit.jsonl; journalctl -t basalt-assistant -n 3 --no-pager -o verbose | grep -E "MESSAGE=|BASALT_AUDIT_HASH" | head -6'
}

steps=("$@")
[[ ${#steps[@]} -gt 0 ]] || steps=(setup selinux port config sandbox dnf disk confine mcp audit)
for s in "${steps[@]}"; do
  declare -F "step_$s" >/dev/null || die "unknown step $s"
  "step_$s"
done
log "assistant test: ${steps[*]} done"
