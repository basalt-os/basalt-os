#!/usr/bin/env bash
# basalt-gate lab driver. Runs on the lab VM as root, with SELinux
# enforcing, after basalt-gate, basalt-ledger and basalt-agent (its SELinux
# module) are installed and the users dev and other exist. Nothing here is
# shipped: lab.json (the lab.echo action, root at a terminal as its
# executor) and the polkit rule that lets dev and root approve without a
# password exist only on the lab VM.
#
#   driver.sh setup       lab registry and polkit rule, the service, an agent project
#   driver.sh service     the daemon, its domain, labels, the careful preset
#   driver.sh request     a request waits; the terminal decider sees its code; approve with polkit
#   driver.sh claims      executor claims: wrong executor, wrong digest, replay
#   driver.sh rule        a rule added through the gate approves by itself; dry run
#   driver.sh locked      locked actions refused whatever the rules say
#   driver.sh stop        emergency stop from an agent; resume from the terminal
#   driver.sh escape      an agent domain tries to decide, write the store, raise
#                         its own rule, claim, replay, enter the decider domain
#   driver.sh seal        an offline edit of the rule file is detected
#   driver.sh ledger      gate records in the ledger, chain verify
#   driver.sh avc EPOCH   SELinux denials since EPOCH (seconds)
set -u
U=dev
H=/home/$U
T=/opt/gate-tests
P=$H/gateproj
LEVEL=s0:c7,c8
STATE=/tmp/gate-lab
mkdir -p $STATE
as_dev() { su - $U -c "$1"; }
as_other() { su - other -c "$1"; }
# A process in the agent domain basalt_agent_t (as dev, or as root for the
# store tests), through basalt-agent's entry point, at its own MCS level.
as_agent() { su - $U -c "runcon -t basalt_agent_t -l $LEVEL -- /usr/libexec/basalt-agent/basalt-agent-exec $P $1"; }
as_agent_root() { runcon -t basalt_agent_t -l $LEVEL -- /usr/libexec/basalt-agent/basalt-agent-exec /root/gateproj "$@"; }
gc() { python3 $T/gateclient.py "$@"; }
field() { python3 -c 'import json,sys; d=json.loads(sys.stdin.read().strip().splitlines()[-1]); print(eval(sys.argv[1], {}, {"d": d}))' "$1"; }
check() { # check NAME COMMAND...: PASS when the command succeeds
  local name=$1; shift
  if "$@" >/tmp/c.out 2>&1; then echo "PASS $name"; else echo "FAIL $name: $(tail -3 /tmp/c.out | tr '\n' ' ' | head -c 300)"; fi
}
refused() { # refused NAME COMMAND...: PASS when the command fails
  local name=$1; shift
  if "$@" >/tmp/c.out 2>&1; then echo "FAIL $name (it worked: $(head -c 200 /tmp/c.out))"; else echo "PASS $name refused: $(tail -1 /tmp/c.out | head -c 200)"; fi
}
expect() { # expect NAME WANT GOT
  if [[ "$3" == "$2" ]]; then echo "PASS $1 ($3)"; else echo "FAIL $1: got $3, want $2"; fi
}
contains() { # contains NAME NEEDLE TEXT
  if [[ "$3" == *"$2"* ]]; then echo "PASS $1"; else echo "FAIL $1: no '$2' in: $(head -c 300 <<<"$3")"; fi
}
# request as dev through the shipped command line; prints the reply JSON.
dev_request() { as_dev "basalt-gate request --json $1" 2>/dev/null; }
# a decision at a terminal (script gives the terminal decider a tty).
dev_tty() { as_dev "script -qec \"$1\" /dev/null </dev/null"; }

# has_rec EVENT [TEXT]: the ledger holds a basalt-gate record of EVENT
# (containing TEXT).
has_rec() { basalt-ledger --producer basalt-gate --event "$1" --json -n 200 | grep -q -- "${2:-$1}"; }

setup() {
  rpm -q util-linux-script >/dev/null || dnf -y -q install util-linux-script >/dev/null
  install -m644 $T/lab.json /usr/share/basalt/gate/actions.d/lab.json
  restorecon -RF /usr/share/basalt/gate
  install -m644 $T/49-basalt-gate-lab.rules /etc/polkit-1/rules.d/49-basalt-gate-lab.rules
  systemctl reload-or-restart polkit
  install -d -o $U -g $U $P
  install -m644 -o $U -g $U $T/gateclient.py $P/gateclient.py
  chcon -R -t basalt_agent_project_t -l $LEVEL $P
  install -d /root/gateproj && cp $T/gateclient.py /root/gateproj/ && chcon -R -t basalt_agent_project_t -l $LEVEL /root/gateproj
  rm -rf /var/lib/basalt-gate/* && systemctl enable --now basalt-ledger basalt-gated && systemctl restart basalt-gated
  sleep 1
  echo "setup done: $(basalt-gate status | head -3 | tr '\n' ' ')"
}

service() {
  check "service active" systemctl is-active basalt-gated
  local dom; dom=$(ps -eo label,comm | awk '/basalt-gated/ {print $1}')
  contains "daemon confined" ":basalt_gate_t:" "$dom"
  contains "socket labeled" "basalt_gate_runtime_t" "$(ls -Z /run/basalt-gate/gate.sock)"
  contains "store labeled" "basalt_gate_var_lib_t" "$(ls -Z /var/lib/basalt-gate/rules.json)"
  contains "store root only" "-rw-------" "$(ls -l /var/lib/basalt-gate/rules.json)"
  contains "data labeled" "basalt_gate_data_t" "$(ls -Zd /usr/share/basalt/gate/actions.d)"
  local st; st=$(as_dev "basalt-gate status")
  contains "careful preset by default" "preset:     careful" "$st"
  contains "dev is a tool, not a decider" "tool (requester, executor)" "$st"
  check "start recorded" has_rec gate.start
  check "pkcheck reachable" test -x /usr/bin/pkcheck
}

request() {
  local r id
  r=$(dev_request "--action lab.echo --arg text=hello")
  id=$(field 'd["id"]' <<<"$r"); echo "$id" >$STATE/req1
  expect "a request waits for a person" asked "$(field 'd["decision"]' <<<"$r")"
  contains "no rule covers it" "default" "$(field 'd["by"]' <<<"$r")"
  contains "the requester never sees the code" "None" "$(as_dev "python3 $T/gateclient.py '{\"op\":\"status\",\"id\":\"$id\"}'" | field 'd["request"].get("code")')"
  # The terminal decider (basalt_gate_tty_t) sees the code: only deciders do.
  local q; q=$(as_dev "basalt-gate queue")
  contains "queue lists it" "$id" "$q"
  contains "the terminal decider sees the code" "code " "$q"
  # Deciding needs a terminal.
  refused "approve without a terminal" su - $U -c "basalt-gate approve $id </dev/null"
  # The user other (no lab polkit rule) cannot approve without a password.
  r=$(as_other "basalt-gate request --json --action lab.echo --arg text=other" 2>/dev/null)
  local oid; oid=$(field 'd["id"]' <<<"$r")
  refused "other approves without a password" timeout 30 su - other -c "script -qec 'basalt-gate approve $oid' /dev/null </dev/null >/tmp/other.out 2>&1; grep -q ': allowed' /tmp/other.out"
  refused "dev approves other's request" bash -c "su - $U -c \"script -qec 'basalt-gate approve $oid' /dev/null </dev/null\" | grep -q ': allowed'"
  # dev approves at the terminal: polkit (the lab rule says yes for dev).
  check "dev approves at the terminal" bash -c "su - $U -c \"script -qec 'basalt-gate approve $id' /dev/null </dev/null\" | grep -q '$id: allowed'"
  check "approval recorded as a person at the terminal" has_rec gate.decision person:tty
  r=$(as_dev "python3 $T/gateclient.py '{\"op\":\"status\",\"id\":\"$id\"}'")
  expect "allowed now" allowed "$(field 'd["decision"]' <<<"$r")"
  field 'd["request"]["digest"]' <<<"$r" >$STATE/req1.digest
  # Wait wakes on a decision.
  r=$(dev_request "--action lab.echo --arg text=waiting")
  id=$(field 'd["id"]' <<<"$r")
  (sleep 2; su - $U -c "script -qec 'basalt-gate decline $id' /dev/null </dev/null" >/dev/null) &
  r=$(as_dev "python3 $T/gateclient.py '{\"op\":\"hello\",\"client\":\"basalt-gate/x\"}' '{\"op\":\"wait\",\"id\":\"$id\",\"timeout\":30}'" | tail -1)
  wait
  expect "a waiting requester learns it was declined" declined "$(field 'd["decision"]' <<<"$r")"
}

claims() {
  local id digest r
  id=$(cat $STATE/req1); digest=$(cat $STATE/req1.digest)
  refused "dev (uid 1000) is not the executor" as_dev "python3 $T/gateclient.py '{\"op\":\"claim\",\"id\":\"$id\",\"digest\":\"$digest\"}'"
  check "root claims with the approved digest" gc "{\"op\":\"claim\",\"id\":\"$id\",\"digest\":\"$digest\"}"
  refused "a second claim (replay)" gc "{\"op\":\"claim\",\"id\":\"$id\",\"digest\":\"$digest\"}"
  check "root reports the result" gc "{\"op\":\"result\",\"id\":\"$id\",\"ok\":true,\"exit\":0,\"detail\":\"echoed hello\"}"
  # Approve A, run B: another digest voids the decision.
  r=$(dev_request "--action lab.echo --arg text=approved-text")
  id=$(field 'd["id"]' <<<"$r")
  su - $U -c "script -qec 'basalt-gate approve $id' /dev/null </dev/null" >/dev/null
  # The executor computes the digest from what it would run; the lab reads
  # it from the requester's view.
  digest=$(as_dev "python3 $T/gateclient.py '{\"op\":\"status\",\"id\":\"$id\"}'" | field 'd["request"]["digest"]')
  refused "claim with another digest" gc "{\"op\":\"claim\",\"id\":\"$id\",\"digest\":\"sha256:$(printf '0%.0s' {1..64})\"}"
  refused "the voided decision cannot be claimed" gc "{\"op\":\"claim\",\"id\":\"$id\",\"digest\":\"$digest\"}"
  check "claims recorded" has_rec gate.claim digest_match
  check "refused claims recorded" has_rec gate.claim "already claimed"
}

rule() {
  local r id
  r=$(as_dev "basalt-gate rules add $T/rule-echo.toml" 2>&1)
  id=$(grep -o 'g-[0-9a-f]\{12\}' <<<"$r" | head -1)
  contains "a rule change is itself a request" "waits for approval" "$r"
  check "dev approves the rule at the terminal" bash -c "su - $U -c \"script -qec 'basalt-gate approve $id' /dev/null </dev/null\" | grep -q ': allowed'"
  contains "the rule is listed as a sentence" "Let the tool basalt-gate do lab.echo without asking" "$(as_dev "basalt-gate rules list")"
  r=$(dev_request "--action lab.echo --arg text=by-rule")
  expect "the rule approves by itself" allowed "$(field 'd["decision"]' <<<"$r")"
  contains "the decision names the rule" "rule:r-lab-echo@" "$(field 'd["by"]' <<<"$r")"
  check "rule add recorded" has_rec gate.rule.add r-lab-echo
  check "rule decision recorded" has_rec gate.decision "rule:r-lab-echo@"
  # The rule is dev's: other still asks.
  r=$(as_other "basalt-gate request --json --action lab.echo --arg text=other2" 2>/dev/null)
  expect "another user's request still asks" asked "$(field 'd["decision"]' <<<"$r")"
  # Dry run: last hour, with this rule, dev's echo requests would be allowed.
  r=$(as_dev "basalt-gate simulate --rules $T/rule-echo.toml --scope user --since 1h")
  contains "dry run replays the recorded requests" "would be allowed" "$r"
  echo "      $(head -1 <<<"$r")"
}

locked() {
  local r
  # A system rule lets agents run terminal tools without asking (approved
  # by root at the terminal as an administrator).
  r=$(basalt-gate rules add $T/rule-agent-tools.toml --scope system 2>&1)
  local id; id=$(grep -o 'g-[0-9a-f]\{12\}' <<<"$r" | head -1)
  check "root approves the system rule" bash -c "script -qec 'basalt-gate approve $id' /dev/null </dev/null | grep -q ': allowed'"
  for argv in '["setenforce","0"]' '["sudo","-n","setenforce","0"]' '["semodule","-r","basalt_gate"]' '["mkfs.ext4","/dev/vdb"]' \
              '["cryptsetup","luksKillSlot","/dev/vda3","0"]' '["systemctl","stop","basalt-ledger.service"]'; do
    r=$(as_agent "python3 gateclient.py '{\"op\":\"hello\",\"client\":\"x\"}' '{\"op\":\"propose\",\"calls\":[{\"action\":\"tool.exec\",\"args\":{\"tool\":\"lab\",\"operation\":\"run\",\"argv\":$argv}}]}'" | tail -1)
    expect "locked whatever the rule says: $argv" refused "$(field 'd["decision"]' <<<"$r")"
  done
  contains "refused by a locked hard limit" "hard-limit:locked:" "$(field 'd["by"]' <<<"$r")"
  r=$(as_agent "python3 gateclient.py '{\"op\":\"propose\",\"calls\":[{\"action\":\"tool.exec\",\"args\":{\"tool\":\"lab\",\"operation\":\"run\",\"argv\":[\"systemctl\",\"restart\",\"chronyd\"]}}]}'" | tail -1)
  expect "the same rule allows what is not locked" allowed "$(field 'd["decision"]' <<<"$r")"
  check "locked refusals recorded" has_rec gate.decision hard-limit:locked
}

stop() {
  local r
  check "an agent pulls the emergency stop" as_agent "python3 gateclient.py '{\"op\":\"stop\",\"reason\":\"lab\"}'"
  contains "status says stopped" "STOPPED" "$(as_dev "basalt-gate status")"
  r=$(dev_request "--action lab.echo --arg text=while-stopped")
  expect "the rule does not apply while stopped" asked "$(field 'd["decision"]' <<<"$r")"
  contains "stopped is the reason" "stop" "$(field 'd["by"]' <<<"$r")"
  refused "the agent cannot resume" as_agent "python3 gateclient.py '{\"op\":\"resume\"}'"
  refused "dev's tool connection cannot resume" as_dev "python3 $T/gateclient.py '{\"op\":\"resume\"}'"
  systemctl restart basalt-gated; sleep 1
  contains "the stop survives a restart" "STOPPED" "$(as_dev "basalt-gate status")"
  check "dev resumes at the terminal" bash -c "su - $U -c \"script -qec 'basalt-gate resume' /dev/null </dev/null\" | grep -q 'resumed'"
  r=$(dev_request "--action lab.echo --arg text=after-resume")
  expect "the rule applies again" allowed "$(field 'd["decision"]' <<<"$r")"
  check "stop recorded" has_rec gate.stop
  check "resume recorded" has_rec gate.resume
}

escape() {
  local r id digest
  # Show the denials the base policy does not audit, for the evidence.
  semodule -DB
  contains "the agent runs in basalt_agent_t" "basalt_agent_t" "$(as_agent "cat /proc/self/attr/current")"
  r=$(as_agent "python3 gateclient.py '{\"op\":\"hello\",\"role\":\"decider\",\"client\":\"basalt-gate-tty\"}'")
  contains "asking to be a decider changes nothing" '"roles":["requester"],"kind":"agent"' "$r"
  # A request the agent's user did not make, and the agent's own one.
  r=$(as_other "basalt-gate request --json --action lab.echo --arg text=pending-for-agent" 2>/dev/null)
  id=$(field 'd["id"]' <<<"$r")
  refused "the agent decides someone's request" as_agent "python3 gateclient.py '{\"op\":\"decide\",\"id\":\"$id\",\"approve\":true}'"
  refused "the agent decides its own request" as_agent "python3 gateclient.py '{\"op\":\"propose\",\"calls\":[{\"action\":\"lab.echo\",\"args\":{\"text\":\"mine\"}}]}' '{\"op\":\"decide\",\"id\":\"LAST_ID\",\"approve\":true}'"
  r=$(as_agent "python3 gateclient.py '{\"op\":\"propose\",\"calls\":[{\"action\":\"gate.rule.change\",\"args\":{\"op\":\"add\",\"scope\":\"user\",\"rule\":{\"id\":\"r-mine\",\"effect\":\"allow-quiet\",\"actions\":[\"lab.echo\"],\"requesters\":[\"agent\"]}}}]}'" | tail -1)
  expect "the agent raises its own rule" refused "$(field 'd["decision"]' <<<"$r")"
  refused "the agent applies a rule directly" as_agent "python3 gateclient.py '{\"op\":\"rules.apply\",\"rule\":{\"op\":\"add\",\"scope\":\"user\",\"rule\":{\"id\":\"r-mine\",\"effect\":\"refuse\",\"actions\":[\"*\"],\"requesters\":[\"any\"]}}}'"
  refused "the agent reads the rules" as_agent "python3 gateclient.py '{\"op\":\"rules.list\"}'"
  # Claims: another executor's action, and a replay of a used claim.
  id=$(cat $STATE/req1); digest=$(cat $STATE/req1.digest)
  refused "the agent claims another executor's action" as_agent "python3 gateclient.py '{\"op\":\"claim\",\"id\":\"$id\",\"digest\":\"$digest\"}'"
  # The store: as root in the agent domain (no DAC in the way), SELinux denies.
  refused "the agent (as root) reads the rule store" as_agent_root cat /var/lib/basalt-gate/rules.json
  refused "the agent (as root) writes the rule store" as_agent_root sh -c 'echo x >>/var/lib/basalt-gate/rules.json'
  refused "the agent (as root) writes the configuration" as_agent_root sh -c 'echo x >>/etc/basalt-gate/gate.conf'
  # The decider domain: the agent cannot enter it.
  r=$(as_agent "/usr/libexec/basalt-gate/basalt-gate-tty '{\"op\":\"pending\"}'" 2>&1)
  contains "the agent cannot run the terminal decider" "ermission denied" "$r"
  check "policy: no agent may open, read or write the store" bash -c "! sesearch -A -s basalt_agent_t -t basalt_gate_var_lib_t | grep -Eq 'open|read|write|append'"
  check "policy: no agent transition into the decider" bash -c "! sesearch -T -s basalt_agent_t -t basalt_gate_tty_exec_t | grep -q type_transition && ! sesearch -A -s basalt_agent_t -t basalt_gate_tty_t -c process -p transition | grep -q allow"
  check "policy: user domains enter the decider" bash -c "sesearch -T -s unconfined_t -t basalt_gate_tty_exec_t | grep -q basalt_gate_tty_t"
  check "the agent's attempts are recorded" has_rec gate.decision "only a trusted surface decides"
  semodule -B
}

seal() {
  systemctl stop basalt-gated
  cp -a /var/lib/basalt-gate/rules.json $STATE/rules.json.good
  sed -i 's/"lab.echo"/"tool.exec"/' /var/lib/basalt-gate/rules.json
  systemctl start basalt-gated; sleep 1
  contains "a tampered rule file is not trusted" "NOT TRUSTED" "$(basalt-gate status)"
  check "seal error recorded as critical" has_rec gate.seal_error critical
  check "the tampered file is kept aside" bash -c "ls /var/lib/basalt-gate/rules.json.bad-*"
  local r; r=$(dev_request "--action lab.echo --arg text=after-tamper")
  expect "only the careful rules are in force" asked "$(field 'd["decision"]' <<<"$r")"
}

ledger() {
  local n; n=$(basalt-ledger --producer basalt-gate --json -n 100000 | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))')
  echo "INFO gate records in the ledger: $n"
  check "ledger chain verifies" basalt-ledger verify
  echo "INFO sample:"; basalt-ledger --producer basalt-gate -n 12 | sed 's/^/      /'
}

avc() {
  local since=${1:-0}
  # The kernel's audit records reach the journal (ausearch's time window
  # depends on audit.log's clock and rotation; the journal is simpler).
  local all; all=$(journalctl -q --no-pager --since "@$since" -g 'avc:  denied|SELINUX_ERR' 2>/dev/null || true)
  local gate; gate=$(grep -E 'scontext=[^ ]*:basalt_gate(_tty)?_t:' <<<"$all" || true)
  local agent; agent=$(grep -E 'scontext=[^ ]*:basalt_agent_t:' <<<"$all" || true)
  local other; other=$(grep -vE 'scontext=[^ ]*:basalt_(gate(_tty)?|agent)_t:' <<<"$all" | grep -v '^$' || true)
  if [[ -z "$gate" ]]; then echo "PASS zero AVC denials for basalt_gate_t and basalt_gate_tty_t"; else echo "FAIL denials for the gate's domains:"; sed 's/^/      /' <<<"$gate"; fi
  echo "INFO denials of the agent domain (the escape attempts, expected): $(grep -c . <<<"$agent")"
  sed -n 's/.*avc:  denied  \({[^}]*}\).*comm="\([^"]*\)".*tcontext=[^:]*:[^:]*:\([^:]*\):.*tclass=\([a-z_]*\).*/\2 \1 \3 \4/p' <<<"$agent" | sort | uniq -c | sed 's/^/     /' 
  if [[ -z "$other" ]]; then echo "PASS no other denials"; else echo "INFO other denials:"; sed 's/^/      /' <<<"$other" | head -20; fi
  echo "INFO SELinux mode: $(getenforce)"
}

case "${1:-}" in
  setup) setup ;;
  service) service ;;
  request) request ;;
  claims) claims ;;
  rule) rule ;;
  locked) locked ;;
  stop) stop ;;
  escape) escape ;;
  seal) seal ;;
  ledger) ledger ;;
  avc) avc "${2:-0}" ;;
  *) echo "usage: driver.sh setup|service|request|claims|rule|locked|stop|escape|seal|ledger|avc EPOCH"; exit 2 ;;
esac
