#!/usr/bin/env bash
# Record labeled cases for the shared evaluation suite (eval/cases) on a
# lab VM: each scenario breaks something on purpose, so its cause is known
# by construction; `basalt why UNIT --json` (as root, every probe) and
# `basalt fix selinux --json` record what the diagnosers saw; then the
# scenario is undone. Nothing is applied: the cases are the input of the
# decision layer and the expected answers, not the fixes.
#
#   scripts/lab/eval-capture.sh [SCENARIO...]     default: all
#
# Output: $LAB_DIR/eval-capture/<scenario>.json, one object per scenario:
#   {"scenario", "subject", "expected": {question: answer}, "diagnosis",
#    "actions", "why": <basalt why --json>, "selinux": <basalt fix selinux --json>}
# eval/tools/capture-to-cases.py turns them into basalt-case/v1.1 lines.
#
# Expected actions must pass the assistant's action validators
# (internal/action; `basalt-eval check`). A file.restore names no snapshot:
# eval/tools/capture-to-cases.py binds the number of the snapshot the
# diagnosers found (`restore` in basalt why --json).
#
# Needs a VM with basalt-assistant and nginx (scripts/lab/assistant-test.sh
# setup) and /root/nginx.conf.orig. Lab only: it edits nginx.conf, adds
# units named basalt-lab-*, port and file context rules, and removes them.
source "$(dirname "$0")/../lib.sh"
L="$REPO_ROOT/scripts/lab"
vm() { "$L/vm.sh" ssh "$@"; }
OUT="$LAB_DIR/eval-capture"
mkdir -p "$OUT"

# A lab unit: lab_unit NAME 'ExecStart line' ['extra [Service] lines'] ['[Unit] lines']
# The heredoc is quoted on the VM, so "$$$$" in ExecStart reaches systemd,
# which turns it into "$$" for the shell.
lab_unit() {
  local name="$1" exec="$2" extra="${3:-}" unitlines="${4:-}"
  vm "cat >/etc/systemd/system/$name.service <<'UNIT'
[Unit]
Description=Basalt lab fixture $name
$unitlines
[Service]
Type=oneshot
ExecStart=$exec
$extra
UNIT
systemctl daemon-reload"
}

reset_all() {
  vm 'cp /root/nginx.conf.orig /etc/nginx/nginx.conf
      rm -rf /etc/systemd/system/nginx.service.d/basalt-lab.conf /etc/nginx/conf.d/basalt-lab*.conf
      for u in $(systemctl list-units --all --plain --no-legend "basalt-lab-*" | awk "{print \$1}"); do systemctl stop "$u" 2>/dev/null; systemctl reset-failed "$u" 2>/dev/null; done
      rm -f /etc/systemd/system/basalt-lab-*.service; systemctl daemon-reload
      umount /mnt/basalt-lab-full 2>/dev/null; rmdir /mnt/basalt-lab-full 2>/dev/null
      semanage port -d -t http_port_t -p tcp 8085 2>/dev/null
      semanage fcontext -d "/srv/nginx-logs(/.*)?" 2>/dev/null; rm -rf /srv/nginx-logs /data /var/lib/basalt-lab-dac
      systemctl reset-failed nginx.service 2>/dev/null; systemctl restart nginx; systemctl is-active nginx' >/dev/null || true
}

# capture NAME SUBJECT EXPECTED_JSON DIAGNOSIS ACTIONS_JSON: record the
# diagnosis; prints the rules' answer.
capture() {
  local name="$1" subject="$2" expected="$3" diagnosis="$4" actions="$5"
  sleep 3
  vm "basalt why $subject --json" >"$OUT/.why" 2>/dev/null || echo null >"$OUT/.why"
  vm 'basalt fix selinux --since 5m --json' >"$OUT/.selinux" 2>/dev/null || echo null >"$OUT/.selinux"
  python3 - "$OUT" "$name" "$subject" "$expected" "$diagnosis" "$actions" <<'PY'
import json, sys
out, name, subject, expected, diagnosis, actions = sys.argv[1:]
def load(p):
    try:
        return json.load(open(p))
    except ValueError:
        return None
rec = {"scenario": name, "subject": subject, "expected": json.loads(expected), "diagnosis": diagnosis,
       "actions": json.loads(actions), "why": load(out + "/.why"), "selinux": load(out + "/.selinux")}
json.dump(rec, open(out + "/" + name + ".json", "w"), indent=1)
print(((rec["why"] or {}).get("decision") or {}).get("answer", {}).get("top", "-"))
PY
}

nginx_edit() { vm "$1; systemctl restart nginx 2>/dev/null || true"; }

sc_nginx_directive() {
  nginx_edit 'sed -i "s/^\(\s*\)server_name .*;/&\n\1bogus_directive on;/" /etc/nginx/nginx.conf'
  capture nginx-directive nginx.service '{"unit.cause":"config_error"}' \
    "nginx.conf has an unknown directive" \
    '[{"kind":"file.restore","params":{"path":"/etc/nginx/nginx.conf"}},{"kind":"unit.restart","params":{"unit":"nginx.service"}}]'
}
sc_nginx_syntax() {
  nginx_edit 'sed -i "0,/worker_connections 1024;/s//worker_connections 1024/" /etc/nginx/nginx.conf'
  capture nginx-syntax nginx.service '{"unit.cause":"config_error"}' \
    "nginx.conf misses a semicolon" \
    '[{"kind":"file.restore","params":{"path":"/etc/nginx/nginx.conf"}},{"kind":"unit.restart","params":{"unit":"nginx.service"}}]'
}
sc_nginx_include_missing() {
  nginx_edit 'sed -i "s#^\(\s*\)include /etc/nginx/default.d/\*.conf;#&\n\1include /etc/nginx/basalt-lab-missing.conf;#" /etc/nginx/nginx.conf'
  capture nginx-include-missing nginx.service '{"unit.cause":"missing_file"}' \
    "nginx.conf includes a file that does not exist" '[]'
}
sc_nginx_port_conflict() {
  vm 'systemctl stop nginx; systemd-run --unit basalt-lab-holder -p Type=simple python3 -m http.server 80 --bind 0.0.0.0; sleep 2; systemctl start nginx 2>/dev/null || true'
  capture nginx-port-conflict nginx.service '{"unit.cause":"port_conflict"}' \
    "port 80 is held by another process (python3 http.server)" '[]'
}
sc_nginx_moved_dir() {
  nginx_edit 'mkdir /root/nginx-logs && mv /root/nginx-logs /srv/
    sed -i "s#access_log  /var/log/nginx/access.log  main;#access_log  /srv/nginx-logs/access.log  main;#" /etc/nginx/nginx.conf'
  capture nginx-moved-dir nginx.service '{"unit.cause":"selinux_denial","avc.class":"mislabeled"}' \
    "/srv/nginx-logs keeps admin_home_t after a mv from /root; the policy default would be allowed" \
    '[{"kind":"selinux.restorecon","params":{"path":"/srv/nginx-logs"}},{"kind":"unit.restart","params":{"unit":"nginx.service"}}]'
}
sc_nginx_port_8085() {
  nginx_edit 'sed -i "s/listen       80;/listen       8085;/" /etc/nginx/nginx.conf'
  capture nginx-port-8085 nginx.service '{"unit.cause":"selinux_denial","avc.class":"port"}' \
    "nginx may not bind tcp 8085 (unreserved_port_t)" \
    '[{"kind":"selinux.port","params":{"type":"http_port_t","proto":"tcp","port":"8085","mode":"add"}},{"kind":"unit.restart","params":{"unit":"nginx.service"}}]'
}
sc_nginx_port_8181() {
  nginx_edit 'sed -i "s/listen       80;/listen       8181;/" /etc/nginx/nginx.conf'
  capture nginx-port-8181 nginx.service '{"unit.cause":"selinux_denial","avc.class":"port"}' \
    "tcp 8181 is labeled intermapper_port_t; nginx may not bind it (relabel needs review)" '[]'
}
sc_nginx_data_log() {
  nginx_edit 'mkdir -p /data/logs && restorecon -R /data
    sed -i "s#access_log  /var/log/nginx/access.log  main;#access_log  /data/logs/access.log  main;#" /etc/nginx/nginx.conf'
  capture nginx-data-log nginx.service '{"unit.cause":"selinux_denial","avc.class":"missing_fcontext"}' \
    "/data/logs has the generic default_t; a file context rule for httpd_log_t is needed" \
    '[{"kind":"selinux.fcontext","params":{"path":"/data/logs","type":"httpd_log_t"}},{"kind":"unit.restart","params":{"unit":"nginx.service"}}]'
}
sc_nginx_shadow() {
  nginx_edit 'sed -i "s#^\(\s*\)include /etc/nginx/default.d/\*.conf;#&\n\1include /etc/shadow;#" /etc/nginx/nginx.conf'
  capture nginx-shadow nginx.service '{"unit.cause":"selinux_denial","avc.class":"suspicious"}' \
    "nginx (httpd_t) tried to read /etc/shadow; never allow, fix the configuration" '[]'
}
sc_nginx_boolean() {
  vm 'cat >/etc/nginx/conf.d/basalt-lab-proxy.conf <<EOF
server { listen 8090; location / { proxy_pass http://127.0.0.1:9000; } }
EOF
semanage port -a -t http_port_t -p tcp 8090 2>/dev/null || true
systemctl restart nginx; systemd-run --unit basalt-lab-backend -p Type=simple python3 -m http.server 9000 --bind 127.0.0.1; sleep 2
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8090/ || true'
  capture nginx-proxy-boolean nginx.service '{"avc.class":"boolean"}' \
    "nginx may not connect to the backend port (http_port_t); httpd_can_network_relay is off" \
    '[{"kind":"selinux.boolean","params":{"name":"httpd_can_network_relay","value":"on"}}]'
  vm 'semanage port -d -t http_port_t -p tcp 8090 2>/dev/null || true'
}
sc_nginx_dep() {
  lab_unit basalt-lab-fail1 '/bin/false'
  vm 'mkdir -p /etc/systemd/system/nginx.service.d; printf "[Unit]\nRequires=basalt-lab-fail1.service\nAfter=basalt-lab-fail1.service\n" >/etc/systemd/system/nginx.service.d/basalt-lab.conf
      systemctl daemon-reload; systemctl restart nginx 2>/dev/null || true'
  capture nginx-dependency nginx.service '{"unit.cause":"dependency_failed"}' "basalt-lab-fail1.service, required by nginx, failed" '[]'
}
sc_exec_missing() {
  lab_unit basalt-lab-noexec '/usr/local/bin/basalt-lab-not-installed --serve'
  vm 'systemctl start basalt-lab-noexec 2>/dev/null || true'
  capture exec-missing basalt-lab-noexec.service '{"unit.cause":"missing_file"}' "the program in ExecStart does not exist" '[]'
}
sc_segv() {
  lab_unit basalt-lab-segv "/bin/sh -c 'kill -SEGV \$\$\$\$'"
  vm 'systemctl start basalt-lab-segv 2>/dev/null || true'
  capture crash-segv basalt-lab-segv.service '{"unit.cause":"crashed"}' "the process was killed by SIGSEGV" '[]'
}
sc_abrt() {
  lab_unit basalt-lab-abrt "/bin/sh -c 'echo assertion failed: queue != NULL >&2; kill -ABRT \$\$\$\$'"
  vm 'systemctl start basalt-lab-abrt 2>/dev/null || true'
  capture crash-abrt basalt-lab-abrt.service '{"unit.cause":"crashed"}' "the process aborted (SIGABRT) after an assertion" '[]'
}
sc_kill() {
  lab_unit basalt-lab-kill "/bin/sh -c 'kill -KILL \$\$\$\$'"
  vm 'systemctl start basalt-lab-kill 2>/dev/null || true'
  capture crash-kill basalt-lab-kill.service '{"unit.cause":"crashed"}' "the process was killed by SIGKILL" '[]'
}
sc_dep_custom() {
  lab_unit basalt-lab-fail2 "/bin/sh -c 'echo cannot reach license server >&2; exit 1'"
  lab_unit basalt-lab-dependent '/bin/true' '' $'Requires=basalt-lab-fail2.service\nAfter=basalt-lab-fail2.service'
  vm 'systemctl start basalt-lab-dependent 2>/dev/null || true'
  capture dep-custom basalt-lab-dependent.service '{"unit.cause":"dependency_failed"}' "basalt-lab-fail2.service, required by it, failed" '[]'
}
sc_disk_full() {
  vm 'mkdir -p /mnt/basalt-lab-full && mount -t tmpfs -o size=1m tmpfs /mnt/basalt-lab-full'
  lab_unit basalt-lab-writer "/bin/sh -c 'dd if=/dev/zero of=/mnt/basalt-lab-full/cache.bin bs=64k count=64'"
  vm 'systemctl start basalt-lab-writer 2>/dev/null || true'
  capture disk-full basalt-lab-writer.service '{"unit.cause":"disk_full"}' "the file system it writes to is full (No space left on device)" '[]'
}
sc_generic() {
  lab_unit basalt-lab-generic "/bin/sh -c 'echo upstream returned HTTP 500, giving up >&2; exit 1'"
  vm 'systemctl start basalt-lab-generic 2>/dev/null || true'
  capture generic-error basalt-lab-generic.service '{"unit.cause":"unknown"}' "the program reported an application error; no known cause class" '[]'
}
sc_dac() {
  vm 'install -d -m 0700 -o root -g root /var/lib/basalt-lab-dac'
  lab_unit basalt-lab-dac "/bin/sh -c 'echo state > /var/lib/basalt-lab-dac/state'" 'User=nobody'
  vm 'systemctl start basalt-lab-dac 2>/dev/null || true'
  capture dac-permission basalt-lab-dac.service '{"unit.cause":"unknown"}' \
    "Permission denied from file mode and owner (DAC), not SELinux: the directory is root-only" '[]'
}
sc_timeout() {
  lab_unit basalt-lab-timeout '/bin/sleep 30' 'TimeoutStartSec=3'
  vm 'systemctl start basalt-lab-timeout 2>/dev/null || true'
  capture start-timeout basalt-lab-timeout.service '{"unit.cause":"unknown"}' "start timed out; no known cause class" '[]'
}
sc_app_config() {
  lab_unit basalt-lab-appconf "/bin/sh -c 'echo \"config error: invalid value maybe for option debug in /etc/basalt-lab/app.conf line 3\" >&2; exit 78'"
  vm 'systemctl start basalt-lab-appconf 2>/dev/null || true'
  capture app-config basalt-lab-appconf.service '{"unit.cause":"config_error"}' "the application rejects its configuration file" '[]'
}
sc_app_missing_conf() {
  lab_unit basalt-lab-noconf "/bin/cat /etc/basalt-lab/missing.conf"
  vm 'systemctl start basalt-lab-noconf 2>/dev/null || true'
  capture app-missing-conf basalt-lab-noconf.service '{"unit.cause":"missing_file"}' "the file it reads does not exist" '[]'
}
sc_port_22() {
  lab_unit basalt-lab-port22 "/usr/bin/python3 -m http.server 22"
  vm 'sed -i "s/^Type=oneshot$/Type=simple/" /etc/systemd/system/basalt-lab-port22.service; systemctl daemon-reload; systemctl start basalt-lab-port22; sleep 3'
  capture port-22 basalt-lab-port22.service '{"unit.cause":"port_conflict"}' "port 22 is held by sshd" '[]'
}

all=(nginx_directive nginx_syntax nginx_include_missing nginx_port_conflict nginx_moved_dir nginx_port_8085 nginx_port_8181
     nginx_data_log nginx_shadow nginx_boolean nginx_dep exec_missing segv abrt kill dep_custom disk_full generic dac timeout
     app_config app_missing_conf port_22)
steps=("$@")
[[ ${#steps[@]} -gt 0 ]] || steps=("${all[@]}")
for s in "${steps[@]}"; do
  declare -F "sc_$s" >/dev/null || die "unknown scenario $s"
  reset_all
  log "$s: rules answer $("sc_$s" | tail -1)"
done
rm -f "$OUT/.why" "$OUT/.selinux"
reset_all
log "captured: $(ls "$OUT" | wc -l) file(s) in $OUT"
