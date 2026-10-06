#!/usr/bin/env bash
# basalt-gate phase 2 lab driver (ADR 0020: the existing approval paths
# move to the gate). Runs on a lab desktop VM as root, SELinux enforcing,
# with basalt-gate, basalt-ledger, basalt-assistant, basalt-agent and
# basalt-shell installed, the desktop user's session running (autologin)
# and enforce = all in /etc/basalt-gate/gate.conf. The desktop steps that
# need a click (the sheet) are driven from the lab host over VNC by
# scripts/lab/gate-phase2-test.sh; this driver prepares and checks them.
#
#   phase2.sh setup             lab polkit rule, gateclient, an agent project
#   phase2.sh propose           an agent asks the shell for a desktop change (prints the gate id)
#   phase2.sh agent-decide ID   an agent domain tries to decide it: refused, recorded
#   phase2.sh applied ID        the request was approved by the person on the sheet, claimed, done
#   phase2.sh apply             the system assistant's proposal through the gate with the code
#   phase2.sh grant-rule        a person's rule that pre-approves skill grants of ~/Documents
#   phase2.sh grant-check       the grant was allowed by that rule and stored by the shell
#   phase2.sh model-check       a model download consent recorded (asked, approved in the shell)
#   phase2.sh agent-tool        basalt-agent egress propose through the gate, approved at the terminal
#   phase2.sh ledger            gate records and the chain verify
#   phase2.sh avc EPOCH         SELinux denials since EPOCH for the gate's domains
set -u
U=${GATE_USER:-basalt}
H=/home/$U
T=/opt/gate-tests
P=$H/gateproj
LEVEL=s0:c7,c8
STATE=/tmp/gate-lab2
mkdir -p $STATE
as_user() { su - "$U" -c "$1"; }
as_agent() { su - "$U" -c "runcon -t basalt_agent_t -l $LEVEL -- /usr/libexec/basalt-agent/basalt-agent-exec $P $1"; }
field() { python3 -c 'import json,sys; d=json.loads(sys.stdin.read().strip().splitlines()[-1]); print(eval(sys.argv[1], {}, {"d": d}))' "$1"; }
check() {
  local name=$1; shift
  if "$@" >/tmp/c.out 2>&1; then echo "PASS $name"; else echo "FAIL $name: $(tail -3 /tmp/c.out | tr '\n' ' ' | head -c 300)"; fi
}
refused() {
  local name=$1; shift
  if "$@" >/tmp/c.out 2>&1; then echo "FAIL $name (it worked: $(head -c 200 /tmp/c.out))"; else echo "PASS $name refused: $(tail -1 /tmp/c.out | head -c 200)"; fi
}
expect() { if [[ "$3" == "$2" ]]; then echo "PASS $1 ($3)"; else echo "FAIL $1: got $3, want $2"; fi; }
contains() { if [[ "$3" == *"$2"* ]]; then echo "PASS $1"; else echo "FAIL $1: no '$2' in: $(head -c 300 <<<"$3")"; fi; }
has_rec() { basalt-ledger --producer basalt-gate --event "$1" --json -n 400 | grep -q -- "${2:-$1}"; }
# rec_has TEXT...: one basalt-gate record holds every TEXT (in its JSON).
rec_has() {
  basalt-ledger --producer basalt-gate --json -n 800 | python3 -c '
import json, sys
need = sys.argv[1:]
for r in json.load(sys.stdin):
    t = json.dumps(r)
    if all(n in t for n in need):
        sys.exit(0)
sys.exit(1)' "$@"
}
session_env='export XDG_RUNTIME_DIR=/run/user/1000 WAYLAND_DISPLAY=wayland-1'

setup() {
  rpm -q util-linux-script >/dev/null || dnf -y -q install util-linux-script >/dev/null
  sed "s/subject.user == \"dev\"/subject.user == \"$U\"/" $T/49-basalt-gate-lab.rules >/etc/polkit-1/rules.d/49-basalt-gate-lab.rules
  systemctl reload-or-restart polkit
  install -d -o "$U" -g "$U" $P
  install -m644 -o "$U" -g "$U" $T/gateclient.py $P/gateclient.py
  chcon -R -t basalt_agent_project_t -l $LEVEL $P
  grep -q '^enforce' /etc/basalt-gate/gate.conf || echo 'enforce = all' >>/etc/basalt-gate/gate.conf
  systemctl restart basalt-gated; sleep 1
  echo "setup done: $(basalt-gate status | head -4 | tr '\n' ' ')"
}

propose() {
  local out
  out=$(as_user "$session_env; timeout 5 basalt-shell propose motion.set '{\"motion\":\"full\"}' --wait 0" 2>&1)
  field 'd["gate"]["id"]' <<<"$(tr -d '\n' <<<"$out")" | tee $STATE/shell-id
}

agent_decide() {
  local id=$1
  contains "the agent runs in basalt_agent_t" "basalt_agent_t" "$(as_agent "cat /proc/self/attr/current")"
  refused "an agent decides the shell's request at the gate" as_agent "python3 gateclient.py '{\"op\":\"decide\",\"id\":\"$id\",\"approve\":true}'"
  refused "an agent confirms it with a code" as_agent "python3 gateclient.py '{\"op\":\"confirm\",\"id\":\"$id\",\"code\":\"00000000\"}'"
  refused "an agent claims it" as_agent "python3 gateclient.py '{\"op\":\"claim\",\"id\":\"$id\",\"calls\":[{\"action\":\"motion.set\",\"args\":{\"motion\":\"full\"}}]}'"
  local r
  r=$(as_agent "python3 gateclient.py '{\"op\":\"observe\",\"calls\":[{\"action\":\"motion.set\",\"args\":{}}],\"outcome\":\"approved\"}'")
  contains "an agent cannot report decisions" "cannot report decisions" "$r"
  refused "the agent asks the shell daemon to decide" as_user "$session_env; basalt-shell ctl decide '{\"id\":\"none\",\"approve\":true}'"
  check "the attempt is recorded" has_rec gate.decision "only a trusted surface decides"
}

applied() {
  local id=$1
  check "approved by a person on the shell's sheet" rec_has gate.decision "$id" person:shell
  check "claimed by basalt-shell" rec_has gate.claim "$id" basalt-shell
  check "result reported" rec_has gate.result "$id"
  contains "the shell's activity names the person at the gate" "gate:person:shell" "$(as_user 'tail -n 30 ~/.local/state/basalt-shell/audit.jsonl')"
}

apply() {
  # A lab proposal of the system assistant: clean dnf's package cache.
  local id=p-9a7e51
  cat >/var/lib/basalt-assistant/proposals/$id.json <<EOF
{"id":"$id","created":"$(date -u +%FT%TZ)","updated":"$(date -u +%FT%TZ)","source":"cli","kind":"dnf","subject":"dnf",
 "key":"lab-gate-phase2","title":"Lab: clean the package cache","report":"Lab proposal for the approval gate test.",
 "actions":[{"kind":"dnf.clean","params":{}}],"needs_review":false,"severity":0.1,"status":"pending","seen":1,
 "last_seen":"$(date -u +%FT%TZ)"}
EOF
  chmod 600 /var/lib/basalt-assistant/proposals/$id.json
  restorecon /var/lib/basalt-assistant/proposals/$id.json
  local code; code=$(basalt show $id | sed -n 's/.*--confirm \([0-9a-f]\{8\}\).*/\1/p' | head -1)
  echo "code $code"
  refused "a wrong code" bash -c "basalt apply $id --yes --confirm 00000000 </dev/null"
  check "basalt apply with the code, through the gate" bash -c "basalt apply $id --yes --confirm $code </dev/null | tee $STATE/apply.out"
  cat $STATE/apply.out | tail -6
  local gid; gid=$(grep -o 'basalt-gate-exec@g-[0-9a-f]*' $STATE/apply.out | head -1 | sed 's/.*@//')
  echo "$gid" >$STATE/apply-id
  expect "the proposal is applied" applied "$(python3 -c "import json;print(json.load(open('/var/lib/basalt-assistant/proposals/$id.json'))['status'])")"
  check "recorded as the person at the root terminal" has_rec gate.decision "person:tty-root"
  check "claimed by basalt-gate-exec" rec_has gate.claim "$gid" basalt-gate-exec
  check "the executor unit succeeded" bash -c "systemctl show -p Result basalt-gate-exec@$gid.service | grep -q success"
  contains "the executor ran in basalt_gate_exec_t" "basalt_gate_exec_t" "$(journalctl -b _SYSTEMD_UNIT=basalt-gate-exec@$gid.service -o verbose | grep _SELINUX_CONTEXT | grep -v init_t | head -1)"
  contains "the code in the queue is the fingerprint" "code:       $code" "$(basalt-gate show "$gid")"
  check "the result and the snapshots are recorded" rec_has gate.result "$gid" snapshots
}

grant_rule() {
  cat >$H/r-docs.toml <<'EOF'
[[rule]]
id = "r-docs"
effect = "allow-tell"
actions = ["grant.folder"]
requesters = ["person"]
resources = { path_beneath = ["~/Documents"] }
expires = "7d"
EOF
  chown "$U:$U" $H/r-docs.toml
  install -d -o "$U" -g "$U" $H/Documents
  local r id
  r=$(as_user "basalt-gate rules add --json ~/r-docs.toml" 2>/dev/null || as_user "basalt-gate rules add ~/r-docs.toml" 2>&1)
  id=$(grep -o 'g-[0-9a-f]\{12\}' <<<"$r" | head -1)
  check "the person approves the rule at the terminal" bash -c "su - $U -c \"script -qec 'basalt-gate approve $id' /dev/null </dev/null\" | grep -q '$id: allowed'"
  contains "the rule is listed" "r-docs" "$(as_user 'basalt-gate rules list')"
}

grant_check() {
  check "a grant of ~/Documents was allowed by the rule" rec_has gate.decision grant.folder rule:r-docs@
  check "the shell stored the grant" bash -c "su - $U -c 'tail -n 40 ~/.local/state/basalt-shell/audit.jsonl' | grep -q 'grant: folder'"
}

model_check() {
  check "a model download request was asked" rec_has gate.request model.download
  check "the consent was recorded as the person's approval in the shell" rec_has gate.decision model.download approved person:shell
}

agent_tool() {
  local out
  out=$(su - "$U" -c "script -qec 'basalt-agent egress propose claude add docs.example.org' /dev/null </dev/null" 2>&1)
  contains "egress propose asks the gate and the person approves at the terminal" "applied:" "$out"
  check "recorded: the tool asked, a person at the terminal approved" rec_has gate.decision agent.egress.change person:tty
  check "the tool reported the result" rec_has gate.result "exit code 0"
  local r
  r=$(as_agent "python3 gateclient.py '{\"op\":\"propose\",\"calls\":[{\"action\":\"agent.egress.change\",\"args\":{\"profile\":\"claude\",\"op\":\"add\",\"entry\":\"evil.example.org\"}}]}'")
  contains "an agent session cannot ask for it" '"decision":"refused"' "$r"
}

ledger() {
  check "the ledger chain verifies" basalt-ledger verify
  basalt-ledger --producer basalt-gate -n 40
}

avc() {
  local since=$1
  local out
  out=$(ausearch -m AVC,USER_AVC,SELINUX_ERR -ts "$(date -d "@$since" '+%m/%d/%Y %H:%M:%S')" 2>/dev/null | grep -E 'basalt_gate|basalt_shell|basalt_assistant' || true)
  if [[ -z "$out" ]]; then echo "PASS no AVC for the gate's, the shell's and the assistant's domains"; else echo "FAIL AVC:"; echo "$out" | head -40; fi
}

case "${1:-}" in
  setup) setup ;;
  propose) propose ;;
  agent-decide) agent_decide "$2" ;;
  applied) applied "$2" ;;
  apply) apply ;;
  grant-rule) grant_rule ;;
  grant-check) grant_check ;;
  model-check) model_check ;;
  agent-tool) agent_tool ;;
  ledger) ledger ;;
  avc) avc "$2" ;;
  *) sed -n 2,26p "$0"; exit 2 ;;
esac
