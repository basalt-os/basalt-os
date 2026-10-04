#!/usr/bin/env bash
# basalt-resolver and basalt-ledger lab driver. Runs on the lab VM as root,
# after the basalt-agent driver's setup (users dev and other, ~/src/app).
# Nothing here is shipped; fixtures are fake (marker LAB-FAKE).
#
#   driver.sh setup       lab names, test HTTP server, the labnet profile
#   driver.sh egress      native session, direct egress on: allowed work, then escapes
#   driver.sh proxyonly   native session, direct egress off (default): allowed work
#   driver.sh container   container session: the proxy runs in the filtered slice
#   driver.sh ledger      append-only: rewrite/delete attempts, producer and reader rules
#   driver.sh events      collectors: escalations, failed auth, snapshot, rollback
#   driver.sh rotate      sealed rotation, chain verify, tamper on a copy, signed export
#   driver.sh restart     resolver restart: rules stay (fail closed), session restored
#   driver.sh avc EPOCH   SELinux denials since EPOCH (seconds)
#   driver.sh report      today's ledger summary and chain verify
set -u
U=dev
H=/home/$U
T=/opt/ledger-tests
as_dev() { su - $U -c "export XDG_RUNTIME_DIR=/run/user/1000; $1"; }
vm_ip() { ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}'; }
gw() { ip route | awk '/default/{print $3; exit}'; }
check() { # check NAME COMMAND...: PASS when the command succeeds
  local name=$1; shift
  if "$@" >/tmp/c.out 2>&1; then echo "PASS $name"; else echo "FAIL $name: $(tail -3 /tmp/c.out | tr '\n' ' ')"; fi
}
refused() { # refused NAME COMMAND...: PASS when the command fails
  local name=$1; shift
  if "$@" >/tmp/c.out 2>&1; then echo "FAIL $name (it worked: $(head -c 200 /tmp/c.out))"; else echo "PASS $name refused: $(tail -1 /tmp/c.out | head -c 160)"; fi
}
ledger_has() { # ledger_has FILTER... : at least one matching record
  basalt-ledger --json "$@" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin) else 1)'
}

setup() {
  systemctl is-active -q basalt-ledger basalt-resolver || systemctl restart basalt-ledger basalt-resolver
  local ip; ip=$(vm_ip)
  # Lab names come from a lab DNS server that systemd-resolved routes the
  # basalt-lab.test domain to (not /etc/hosts: names found there never
  # reach a DNS resolver, so they never fill a session set).
  sed -i '/basalt-lab.test/d' /etc/hosts
  systemctl stop lab-dns lab-http 2>/dev/null
  systemd-run --unit=lab-dns --collect python3 $T/labdns.py 127.0.0.77 "$ip" >/dev/null
  install -d /etc/systemd/resolved.conf.d
  printf '[Resolve]\nDNS=127.0.0.77\nDomains=~basalt-lab.test\n' >/etc/systemd/resolved.conf.d/basalt-lab.conf
  systemctl restart systemd-resolved
  install -d /srv/lab-www && echo "LAB-HTTP-OK" >/srv/lab-www/index.html
  systemd-run --unit=lab-http --collect python3 -m http.server 8008 --bind "$ip" --directory /srv/lab-www >/dev/null
  install -Dm644 $T/labnet.conf /etc/basalt-agent/profiles/labnet.conf
  install -Dm644 $T/labnet2.conf /etc/basalt-agent/profiles/labnet2.conf
  install -d -m700 -o $U -g $U "$H/src/app" "$H/src/app2"
  setsebool basalt_agent_direct_egress off
  sleep 1
  echo "setup done: *.basalt-lab.test -> $ip (lab DNS), HTTP on $ip:8008; resolves to $(resolvectl query model.basalt-lab.test 2>/dev/null | head -1)"
}

# AVC denials in the journal since an epoch (auditd may not see them all).
avc_count() { journalctl _TRANSPORT=audit --since "@$1" -o cat 2>/dev/null | grep -c 'avc:  denied'; }
avc_show() { journalctl _TRANSPORT=audit --since "@$1" -o cat 2>/dev/null | grep 'avc:  denied' | sed -E 's/pid=[0-9]+ //; s/ino=[0-9]+ //' | sort | uniq -c; }

session() { # session MODE PROFILE SCRIPT [ARGS]
  as_dev "LAB_GW=$(gw) basalt-agent run $2 --project ~/src/app --mode $1 -- $3 ${4:-}"
}

egress() {
  setsebool basalt_agent_direct_egress on
  echo "=== NORMAL WORK (native, direct egress on) ==="
  local t0; t0=$(date +%s); sleep 1
  session native labnet $T/egress-normal.sh
  sleep 1
  echo "AVC denials during normal work: $(avc_count "$t0")"
  avc_show "$t0"
  echo "=== ESCAPE ATTEMPTS (native, direct egress on) ==="
  # A second session (labnet2, allowed example.net) to attack.
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 systemctl --user stop lab-victim2.service 2>/dev/null
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 systemd-run --user --unit=lab-victim2 --collect \
    basalt-agent run labnet2 --project "$H/src/app2" --mode native -- -c 'sleep 300' >/dev/null
  sleep 4
  session native labnet $T/egress-attacks.sh
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 systemctl --user stop lab-victim2.service 2>/dev/null
  setsebool basalt_agent_direct_egress off
  sleep 2
  echo "=== LEDGER RECORDS OF THE ATTEMPTS ==="
  as_dev "basalt-ledger --since 3m --severity warning -n 40"
  for ev in dns.deny dns.rebinding egress.drop dns.direct egress.deny; do
    check "ledger has $ev" ledger_has --since 5m --event "$ev"
  done
}

proxyonly() {
  echo "=== NORMAL WORK (native, direct egress off: proxy only) ==="
  local t0; t0=$(date +%s); sleep 1
  session native labnet "-c" "'curl -fsS -o /dev/null https://registry.npmjs.org/left-pad && echo OK proxy; curl -fsS --noproxy \"*\" --connect-timeout 5 -o /dev/null https://registry.npmjs.org/left-pad 2>/dev/null && echo ESCAPED direct || echo DENIED direct-without-boolean'"
  sleep 1
  echo "AVC denials (only the denied direct attempt expected): $(avc_count "$t0")"
  avc_show "$t0"
}

container() {
  echo "=== CONTAINER SESSION (proxy in the filtered slice) ==="
  local t0; t0=$(date +%s); sleep 1
  as_dev "BASALT_AGENT_FEDORA=44 basalt-agent image build labshell >/dev/null 2>&1 || true"
  as_dev "basalt-agent run labshell --project ~/src/app --mode container -- -c 'curl -fsS -o /dev/null https://registry.npmjs.org/left-pad && echo OK allowed-via-proxy; curl -fsS --max-time 10 -o /dev/null https://example.com && echo ESCAPED unlisted || echo DENIED unlisted'"
  sleep 1
  check "container session registered with the resolver" ledger_has --since 3m --event egress.session.start --agent labshell
  check "container proxy lookups answered by the session resolver" ledger_has --since 3m --event dns.allow --agent labshell
  echo "AVC denials during the container session: $(avc_count "$t0")"
  avc_show "$t0"
}

ledger() {
  echo "=== PRODUCERS CANNOT REWRITE OR DELETE HISTORY ==="
  local head; head=$(basalt-ledger status | awk '/chain head/{print $4}' | tr -d ,)
  local rec3; rec3=$(basalt-ledger --json -n 100000 | python3 -c 'import json,sys; print([r for r in json.load(sys.stdin) if r["seq"]==3][0]["hash"])')
  for op in delete update truncate rewrite; do
    refused "api $op by dev" as_dev "echo '{\"op\":\"$op\",\"seq\":3}' | basalt-ledger api | grep -q '\"ok\":true'"
  done
  check "append with seq 3 and a forged hash becomes a new record" as_dev \
    "echo '{\"op\":\"append\",\"record\":{\"v\":1,\"seq\":3,\"prev\":\"00\",\"hash\":\"ff\",\"producer\":\"basalt-agent\",\"event\":\"session.end\",\"outcome\":\"ok\"}}' | basalt-ledger api | grep -q '\"ok\":true'"
  check "record 3 unchanged" sh -c "basalt-ledger --json -n 100000 | python3 -c 'import json,sys; r=[r for r in json.load(sys.stdin) if r[\"seq\"]==3][0]; sys.exit(0 if r[\"hash\"]==\"$rec3\" else 1)'"
  refused "dev writes as another uid" as_dev "echo '{\"op\":\"append\",\"record\":{\"v\":1,\"uid\":0,\"producer\":\"basalt-agent\",\"event\":\"session.end\",\"outcome\":\"ok\"}}' | basalt-ledger api | grep -q '\"ok\":true'"
  refused "dev impersonates the resolver" as_dev "echo '{\"op\":\"append\",\"record\":{\"v\":1,\"producer\":\"basalt-resolver\",\"event\":\"dns.allow\",\"outcome\":\"ok\"}}' | basalt-ledger api | grep -q '\"ok\":true'"
  refused "dev impersonates SELinux" as_dev "echo '{\"op\":\"append\",\"record\":{\"v\":1,\"producer\":\"selinux\",\"event\":\"selinux.avc\",\"outcome\":\"denied\"}}' | basalt-ledger api | grep -q '\"ok\":true'"
  refused "root (unconfined) impersonates the resolver" sh -c "echo '{\"op\":\"append\",\"record\":{\"v\":1,\"producer\":\"basalt-resolver\",\"event\":\"dns.allow\",\"outcome\":\"ok\"}}' | basalt-ledger api | grep -q '\"ok\":true'"
  refused "dev rotates" as_dev "basalt-ledger rotate"
  refused "dev reads the files" as_dev "cat /var/log/basalt-ledger/ledger.jsonl"
  refused "dev appends to the file" as_dev "echo x >>/var/log/basalt-ledger/ledger.jsonl"
  refused "root rewrites the active file in place" python3 -c '
f=open("/var/log/basalt-ledger/ledger.jsonl","r+b"); f.seek(10); f.write(b"X")'
  refused "root truncates the active file" truncate -s 100 /var/log/basalt-ledger/ledger.jsonl
  refused "root removes the active file" rm -f /var/log/basalt-ledger/ledger.jsonl
  check "chain still intact" basalt-ledger verify
  echo "=== READS: OWN RECORDS ONLY, ADMIN SEES ALL ==="
  check "dev sees only uid 1000" as_dev "basalt-ledger --json -n 100000 | python3 -c 'import json,sys; rs=json.load(sys.stdin); sys.exit(0 if rs and all(r[\"uid\"]==1000 for r in rs) else 1)'"
  check "other sees none of dev's records" su - other -c "basalt-ledger --json -n 100000 | python3 -c 'import json,sys; rs=json.load(sys.stdin) or []; sys.exit(0 if all(r[\"uid\"]!=1000 for r in rs) else 1)'"
  check "other asking for uid 1000 gets only its own records" su - other -c "basalt-ledger --json --uid 1000 -n 100000 | python3 -c 'import json,sys; rs=json.load(sys.stdin) or []; sys.exit(0 if all(r[\"uid\"]!=1000 for r in rs) else 1)'"
  refused "dev --all without admin auth" as_dev "basalt-ledger --all -n 1 </dev/null"
  check "refusals are on record" ledger_has --since 5m --event ledger.refused
  echo "records before: $head; after: $(basalt-ledger status | awk '/chain head/{print $4}' | tr -d ,)"
}

events() {
  echo "=== COLLECTORS: ESCALATIONS, FAILED AUTH, LOGINS, SNAPSHOTS ==="
  sudo -n /usr/bin/true
  pkexec /usr/bin/true
  # A failed escalation: a user who is not an administrator, wrong password
  # (sudo logs the attempt, the audit trail a failed authentication).
  su - other -c 'echo LAB-FAKE-WRONG | sudo -S -p "" /usr/bin/id' >/dev/null 2>&1
  local before; before=$(snapper -c root list --columns number | tail -1 | tr -d ' |')
  snapper -c root create -d "ledger lab snapshot" && local snap; snap=$(snapper -c root list --columns number | tail -1 | tr -d ' |')
  if [[ -n "${ROLLBACK:-}" ]]; then
    # basalt-rollback makes a writable copy of the snapshot the next root.
    basalt-rollback "$snap" --yes >/dev/null 2>&1 && echo "rolled back to $snap (takes effect at the next boot)"
  fi
  sleep 40   # the snapper collector polls every 30 s
  for ev in escalation.sudo escalation.pkexec login.session snapshot.create auth.failure; do
    check "ledger has $ev" ledger_has --since 5m --event "$ev"
  done
  [[ -n "${ROLLBACK:-}" ]] && check "ledger has snapshot.rollback" ledger_has --since 5m --event snapshot.rollback
  check "ledger has a denied escalation" ledger_has --since 5m --event escalation. --outcome denied
  basalt-ledger --since 5m --event escalation. -n 6
  basalt-ledger --since 5m --event snapshot. -n 4
  echo "(snapshots before: $before)"
}

rotate() {
  echo "=== SEALED ROTATION AND CHAIN VERIFY ==="
  check "rotate 1" basalt-ledger rotate
  session native labnet "-c" "'curl -fsS -o /dev/null https://registry.npmjs.org/left-pad'" >/dev/null 2>&1
  check "rotate 2" basalt-ledger rotate
  as_dev "basalt-ledger --event session.start -n 1" >/dev/null
  check "chain verifies across the sealed files" basalt-ledger verify
  basalt-ledger verify
  ls -l /var/log/basalt-ledger/
  lsattr /var/log/basalt-ledger/*.jsonl
  refused "root removes a sealed file" sh -c 'rm -f "$(ls /var/log/basalt-ledger/ledger-*.jsonl | head -1)"'
  echo "=== TAMPER EVIDENCE ON A COPY ==="
  rm -rf /root/ledger-copy && mkdir /root/ledger-copy && cp /var/log/basalt-ledger/*.jsonl /root/ledger-copy/
  check "the copy verifies offline" basalt-ledger verify --path /root/ledger-copy/ledger.jsonl
  local first; first=$(ls /root/ledger-copy/ledger-*.jsonl | head -1)
  sed -i '2s/"outcome":"[a-z]*"/"outcome":"allowed"/' "$first"
  refused "an edited sealed file is detected" basalt-ledger verify --path /root/ledger-copy/ledger.jsonl
  rm -f "$first"
  refused "a removed oldest file is detected" basalt-ledger verify --path /root/ledger-copy/ledger.jsonl
  echo "=== SIGNED EXPORT ==="
  as_dev "basalt-ledger export --since today -o /tmp/ledger-export.json"
  check "dev's export verifies against the host key" as_dev "basalt-ledger verify-export /tmp/ledger-export.json --key /var/log/basalt-ledger/keys/export-ed25519.key.pub"
  as_dev "sed -i '0,/\"outcome\": \"denied\"/s//\"outcome\": \"allowed\"/' /tmp/ledger-export.json"
  refused "an edited export is detected" as_dev "basalt-ledger verify-export /tmp/ledger-export.json --key /var/log/basalt-ledger/keys/export-ed25519.key.pub"
}

restart() {
  echo "=== RESOLVER RESTART: FAIL CLOSED, SESSION RESTORED ==="
  setsebool basalt_agent_direct_egress on
  rm -f "$H/src/app/restart.log"
  # The session tries a direct connection every 3 s for 45 s and logs
  # "EPOCH ok|fail" in its project.
  as_dev "nohup basalt-agent run labnet --project ~/src/app --mode native -- -c 'for i in \$(seq 15); do if curl -fsS --noproxy \"*\" --max-time 2 -o /dev/null https://registry.npmjs.org/left-pad; then echo \$(date +%s) ok; else echo \$(date +%s) fail; fi >>restart.log; sleep 3; done' >/dev/null 2>&1 &"
  sleep 7
  local down up; down=$(date +%s)
  systemctl stop basalt-resolver
  check "the resolver stops promptly" sh -c "test \$(( \$(date +%s) - $down )) -lt 10"
  check "session rules stay while the resolver is down" sh -c 'nft list table inet basalt_egress | grep -q "jump s_"'
  sleep 10
  up=$(date +%s)
  systemctl start basalt-resolver
  sleep 2
  check "session restored after the restart" sh -c 'basalt-resolver sessions | grep -q labnet'
  sleep 40
  cat "$H/src/app/restart.log"
  check "no connection while the resolver was down (fail closed)" awk -v d="$down" -v u="$up" '$1>d+1 && $1<u && $2=="ok"{bad=1} END{exit bad}' "$H/src/app/restart.log"
  check "connections refused while the resolver was down" awk -v d="$down" -v u="$up" '$1>d+1 && $1<u && $2=="fail"{n++} END{exit !(n>0)}' "$H/src/app/restart.log"
  check "connections work again after the restart" awk -v u="$up" '$1>u+2 && $2=="ok"{n++} END{exit !(n>0)}' "$H/src/app/restart.log"
  rm -f "$H/src/app/restart.log"
  setsebool basalt_agent_direct_egress off
}

avc() {
  local since=${1:-0}
  echo "AVC denials of the services (basalt_resolver_t, basalt_ledger_t, journalctl_t, iptables_t) since the start of the run:"
  journalctl _TRANSPORT=audit --since "@$since" -o cat 2>/dev/null | grep 'avc:  denied' | grep -E 'scontext=[^ ]*:(basalt_resolver_t|basalt_ledger_t|journalctl_t|iptables_t)' || echo "  none"
  echo "All AVC denials since the start of the run (agent escape attempts are expected here):"
  avc_show "$since"
}

report() {
  basalt-ledger summary --since today
  basalt-ledger verify
}

case "${1:-}" in
  setup|egress|proxyonly|container|ledger|events|rotate|restart) "$1" ;;
  avc) avc "${2:-0}" ;;
  report) report ;;
  *) echo "usage: driver.sh setup|egress|proxyonly|container|ledger|events|rotate|restart|avc|report"; exit 2 ;;
esac
