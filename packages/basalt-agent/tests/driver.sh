#!/usr/bin/env bash
# basalt-agent escape-test driver. Runs on the lab VM as root. It sets up a
# realistic home with decoy credentials, starts a long-lived "victim"
# session, then runs allowed work and the escape attempts in both modes and
# collects the evidence. Nothing here is shipped; the fixtures are fake.
#
#   driver.sh setup        create users, decoys, a victim session
#   driver.sh native       allowed work + attacks, native mode
#   driver.sh container    allowed work + attacks, container mode
#   driver.sh credsetup    mock provider (TLS), exfil hosts, a lab key, labcred tools and image
#   driver.sh credentials MODE   proxy-held key: allowed work (0 AVC), read and exfiltration attempts
#   driver.sh selinux      static proof: no agent domain may read the secret store or the proxy
#   driver.sh avc SINCE     SELinux denials of agent domains since SINCE
set -u
U=dev
H=/home/$U
RT=/run/user/1000/basalt-agent
export_run() { su - $U -c "export XDG_RUNTIME_DIR=/run/user/1000; $1"; }

setup() {
  id $U >/dev/null 2>&1 || useradd -m $U
  id other >/dev/null 2>&1 || useradd -m other
  loginctl enable-linger $U other >/dev/null 2>&1
  # On a fresh VM the user managers start with the linger: wait for dev's bus.
  for _ in $(seq 30); do [ -S /run/user/1000/bus ] && break; sleep 1; done
  # Decoy credentials in the user's real home (never given to an agent).
  install -d -m700 "$H/.ssh" "$H/.gnupg/private-keys-v1.d" "$H/.local/share/keyrings" \
    "$H/.mozilla/firefox/lab.default" "$H/src/other" "$H/src/victim"
  echo "LAB-FAKE-SSH-KEY"     >"$H/.ssh/id_ed25519"
  echo "LAB-FAKE-GPG-KEY"     >"$H/.gnupg/private-keys-v1.d/lab.key"
  echo "LAB-FAKE-KEYRING"     >"$H/.local/share/keyrings/login.keyring"
  echo "LAB-FAKE-BROWSER"     >"$H/.mozilla/firefox/lab.default/key4.db"
  echo "LAB-FAKE-OTHER-PROJ"  >"$H/src/other/secret.txt"
  echo "LAB-FAKE-VICTIM-PROJ" >"$H/src/victim/secret.txt"
  echo "LAB-FAKE-OTHER-USER"  >/home/other/secret.txt; chmod 600 /home/other/secret.txt; chown other: /home/other/secret.txt
  chown -R $U: "$H/.ssh" "$H/.gnupg" "$H/.local/share/keyrings" "$H/.mozilla" "$H/src"
  # install -d made the parents as root on a fresh home; give them back.
  chown $U: "$H/.local" "$H/.local/share"
  # The attacker (labshell) must carry no secret of its own, so a match of
  # the victim marker can only mean a real cross-session read.
  rm -f "$H/.config/basalt-agent/secrets/labshell.env"
  install -d -m700 -o $U -g $U "$H/src/app"
  (cd "$H/src/app" && [ -d .git ] || sudo -u $U git init -q)
  echo "readme" >"$H/src/app/README"; chown $U: "$H/src/app/README"
  # A simulated Basalt shell control socket the agent must not drive. Run
  # it (and the victim) as transient user units so they survive this ssh.
  install -d -m700 -o $U -g $U /run/user/1000/basalt-shell
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 systemctl --user stop lab-shell-sock.service 2>/dev/null
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 \
    systemd-run --user --unit=lab-shell-sock --collect python3 /opt/agent-tests/lab-shell-sock.py
  # A victim session: a long-lived labshell with its own secret, to attack.
  export_run "printf 'LAB_SECRET=LAB-FAKE-VICTIM-SECRET\n' >~/.config/basalt-agent/secrets/labvictim.env 2>/dev/null; \
    install -d -m700 ~/.config/basalt-agent/secrets; printf 'LAB_SECRET=LAB-FAKE-VICTIM-SECRET\n' >~/.config/basalt-agent/secrets/labvictim.env; chmod 600 ~/.config/basalt-agent/secrets/labvictim.env"
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 systemctl --user stop lab-victim.service 2>/dev/null
  runuser -u $U -- env XDG_RUNTIME_DIR=/run/user/1000 \
    systemd-run --user --unit=lab-victim --collect \
    basalt-agent run labvictim --project "$H/src/victim" --mode native -- -c 'sleep 1800'
  sleep 6
  export_run "basalt-agent sessions" | awk '$2=="labvictim"{print "victim "$1" "$4}'
  echo "setup done"
}

# targets_file MODE: write the KEY=VALUE targets for attacks.sh.
# victim session facts, from the running session list and its session.json.
victim_facts() {
  VSESS=$(export_run "basalt-agent sessions" | awk '$2=="labvictim"{print $1; exit}')
  local j=$RT/$VSESS/session.json
  VRT=$RT/$VSESS
  VLPID=$(python3 -c "import json;print(json.load(open('$j'))['pid'])" 2>/dev/null)
  LAB_GW=${LAB_GW:-$(ip route | awk '/default/{print $3; exit}')}
}

common_targets() {
  victim_facts
  cat > "$1" <<EOF
USER_HOME=$H
OTHER_HOME=/home/other
VICTIM_MARKER=LAB-FAKE-VICTIM-SECRET
VICTIM_RT=$2
VICTIM_SESSION=$VSESS
LAUNCHER_PID=${VLPID:-1}
LAB_GW=$LAB_GW
SHELL_SOCK=/run/user/1000/basalt-shell/shell.sock
EOF
}

native() {
  echo "=== NORMAL WORK (native) ==="
  export_run "basalt-agent run labshell --project ~/src/app --mode native -- /opt/agent-tests/normal.sh"
  echo "=== ESCAPE ATTEMPTS (native) ==="
  # In native mode the agent sees real host paths; the victim runtime dir
  # is the victim's own ($RT/VSESS).
  victim_facts
  # The targets file goes in the project, so the launcher relabels it to
  # the session level and the agent can read it; the agent still cannot
  # reach what the targets point at.
  common_targets "$H/src/app/targets.env" "$VRT"
  chown $U: "$H/src/app/targets.env"
  export_run "basalt-agent run labshell --project ~/src/app --mode native -- /opt/agent-tests/attacks.sh ./targets.env"
  rm -f "$H/src/app/targets.env"
}

container() {
  echo "=== build image ==="
  export_run "BASALT_AGENT_FEDORA=44 basalt-agent image build labshell" 2>&1 | tail -2
  echo "=== NORMAL WORK (container) ==="
  export_run "basalt-agent run labshell --project ~/src/app --mode container -- /work/t/normal.sh" 2>/dev/null || true
  # The scripts travel through the project (the only writable mount).
  cp -r /opt/agent-tests "$H/src/app/t"; chown -R $U: "$H/src/app/t"
  export_run "basalt-agent run labshell --project ~/src/app --mode container -- /work/t/normal.sh"
  echo "=== ESCAPE ATTEMPTS (container) ==="
  # In the container only /work and the agent's own /home/agent are mounted;
  # the user's real home, other homes, other sessions and the host runtime
  # are not present at all. The targets therefore point at the real host
  # paths, which simply do not exist inside the container: every reach for
  # them fails, which is the isolation being demonstrated. LAUNCHER_PID is a
  # host pid absent in the container's pid namespace.
  cat > "$H/src/app/t/targets.env" <<EOF
USER_HOME=$H
OTHER_HOME=/home/other
VICTIM_MARKER=LAB-FAKE-VICTIM-SECRET
VICTIM_RT=/run/user/1000/basalt-agent/none
VICTIM_SESSION=none
LAUNCHER_PID=$$
LAB_GW=${LAB_GW:-$(ip route | awk '/default/{print $3; exit}')}
SHELL_SOCK=/run/user/1000/basalt-shell/shell.sock
EOF
  chown $U: "$H/src/app/t/targets.env"
  export_run "basalt-agent run labshell --project ~/src/app --mode container -- /work/t/attacks.sh /work/t/targets.env"
  rm -rf "$H/src/app/t"
}

# --- credentials: the key stays in the session proxy -----------------------
KEY_RE='LAB-FAKE-(PROVIDER-KEY-[0-9a-f]{16}|VICTIM-SECRET)'
MOCK_LOG=/var/log/lab-mock.jsonl
vm_ip() { ip -4 route get 1.1.1.1 | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}'; }
# Denials of agent sessions only (other lab services are not under test).
avc_since() { journalctl _TRANSPORT=audit --since "@$1" -o cat 2>/dev/null | grep 'avc:  denied' | grep -E 'scontext=[^ ]*(basalt_agent|container_t)' | sed -E 's/pid=[0-9]+ //; s/ino=[0-9]+ //' | sort | uniq -c; }

credsetup() {
  local ip d=/etc/lab-mock key
  ip=$(vm_ip)
  systemctl is-active -q basalt-ledger basalt-resolver || systemctl restart basalt-ledger basalt-resolver
  # Lab names (*.basalt-lab.test -> this VM) from a lab DNS server the stub
  # resolver routes the domain to, so session resolvers see them as DNS.
  sed -i '/basalt-lab.test/d' /etc/hosts
  systemctl stop lab-dns lab-mock 2>/dev/null
  systemd-run --unit=lab-dns --collect python3 /opt/agent-tests/labdns.py 127.0.0.77 "$ip" >/dev/null
  install -d /etc/systemd/resolved.conf.d
  printf '[Resolve]\nDNS=127.0.0.77\nDomains=~basalt-lab.test\n' >/etc/systemd/resolved.conf.d/basalt-lab.conf
  systemctl restart systemd-resolved
  # A lab CA in the system trust store (the proxy verifies the provider).
  rm -rf $d; install -d -m700 $d
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj "/CN=Basalt lab mock CA" \
    -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign -keyout $d/ca.key -out $d/ca.pem 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=mock-provider.basalt-lab.test" \
    -keyout $d/server.key -out $d/server.csr 2>/dev/null
  printf 'subjectAltName=DNS:mock-provider.basalt-lab.test,DNS:exfil.basalt-lab.test\nextendedKeyUsage=serverAuth\n' >$d/ext
  openssl x509 -req -in $d/server.csr -CA $d/ca.pem -CAkey $d/ca.key -CAcreateserial -days 7 -extfile $d/ext -out $d/server.pem 2>/dev/null
  cp $d/ca.pem /etc/pki/ca-trust/source/anchors/basalt-lab-mock-ca.pem && update-ca-trust
  # A random fake key in dev's secret store; the mock knows only its hash.
  key="LAB-FAKE-PROVIDER-KEY-$(openssl rand -hex 8)"
  install -d -m700 -o $U -g $U "$H/.config/basalt-agent/secrets"
  printf 'ANTHROPIC_API_KEY=%s\n' "$key" >"$H/.config/basalt-agent/secrets/labcred.env"
  chown $U: "$H/.config/basalt-agent/secrets/labcred.env"; chmod 600 "$H/.config/basalt-agent/secrets/labcred.env"
  printf %s "$key" | sha256sum >$d/key.sha256
  install -m600 /dev/null $MOCK_LOG
  systemd-run --unit=lab-mock --collect python3 /opt/agent-tests/mockprovider.py "$ip" $d/server.pem $d/server.key $d/key.sha256 $MOCK_LOG >/dev/null
  install -Dm644 /opt/agent-tests/labcred.conf /etc/basalt-agent/profiles/labcred.conf
  install -d -m700 -o $U -g $U "$H/src/cred"
  echo readme >"$H/src/cred/README"; chown $U: "$H/src/cred/README"
  echo "=== install labcred (native tools) ==="
  export_run "basalt-agent install labcred" 2>&1 | tail -2
  echo "=== build labcred image ==="
  export_run "BASALT_AGENT_FEDORA=44 basalt-agent image build labcred" 2>&1 | tail -2
  sleep 1
  echo "credsetup done: mock provider https://$ip:8443, exfil http :8009 https :8444; $(resolvectl query mock-provider.basalt-lab.test 2>/dev/null | head -1)"
}

# proxy_pid SESSION: the pid of a session's proxy (matched by its level).
proxy_pid() {
  local lvl
  lvl=$(python3 -c "import json;print(json.load(open('$RT/$1/session.json'))['level'])" 2>/dev/null) || return
  ps -eo pid=,label= | awk -v l=":$lvl" '$2 ~ /basalt_agent_proxy_t/ && substr($2, length($2)-length(l)+1) == l {print $1; exit}'
}

credentials() {
  local mode=$1 proj=$H/src/cred t0 n dir pidf own vp bg
  if [ "$mode" = container ]; then
    rm -rf "$proj/t"; cp -r /opt/agent-tests "$proj/t"; chown -R $U: "$proj/t"; dir=/work/t
  else
    dir=/opt/agent-tests
  fi
  echo "=== CREDENTIALS: allowed work ($mode) ==="
  : >$MOCK_LOG
  t0=$(date +%s); sleep 1
  export_run "basalt-agent run labcred --project ~/src/cred --mode $mode -- $dir/credwork.sh ."
  sleep 2
  n=$(avc_since "$t0" | wc -l)
  echo "AVC denials during allowed work ($mode): $n"; avc_since "$t0"
  echo "provider requests with the right key: $(grep -c '"server": "provider".*"key_ok": true' $MOCK_LOG)"

  echo "=== CREDENTIALS: read and exfiltration attempts ($mode) ==="
  : >$MOCK_LOG
  victim_facts
  vp=$(proxy_pid "$VSESS")
  pidf="$proj/pids.env"; rm -f "$pidf"
  printf "KEY_RE='%s'\nSECRET_FILE=%s\nPID_FILE=./pids.env\n" "$KEY_RE" "$H/.config/basalt-agent/secrets/labcred.env" >"$proj/targets.env"
  chown $U: "$proj/targets.env"
  t0=$(date +%s)
  export_run "basalt-agent run labcred --project ~/src/cred --mode $mode -- $dir/credattacks.sh ./targets.env" &
  bg=$!
  for _ in $(seq 60); do
    own=$(export_run "basalt-agent sessions" | awk '$2=="labcred"{print $1; exit}')
    [ -n "$own" ] && break; sleep 0.5
  done
  sleep 1
  printf 'OWN_PROXY_PID=%s\nVICTIM_PROXY_PID=%s\n' "$(proxy_pid "$own")" "$vp" >"$pidf.tmp"
  chown $U: "$pidf.tmp"; chcon --reference="$proj/README" "$pidf.tmp" 2>/dev/null; mv "$pidf.tmp" "$pidf"
  echo "session $own: own proxy pid $(proxy_pid "$own"), victim $VSESS proxy pid $vp"
  wait $bg
  rm -f "$proj/targets.env" "$pidf"
  echo "=== CREDENTIALS: where the key went ($mode) ==="
  python3 /opt/agent-tests/keycheck.py "$MOCK_LOG" "$KEY_RE"
  echo "key text in the session audit log: $(grep -cE "$KEY_RE" "$H/.local/state/basalt-agent/audit.jsonl")"
  echo "key text in the ledger: $(cat /var/log/basalt-ledger/*.jsonl 2>/dev/null | grep -cE "$KEY_RE")"
  echo "key text in the journal since the attacks: $(journalctl --since "@$t0" -o cat 2>/dev/null | grep -cE "$KEY_RE")"
  echo "credential records: $(grep -c '"event":"credential.use"' "$H/.local/state/basalt-agent/audit.jsonl") use, $(grep -c '"event":"credential.strip"' "$H/.local/state/basalt-agent/audit.jsonl") strip"
  if [ "$mode" = container ]; then rm -rf "$proj/t"; fi
}

selinux_proof() {
  echo "secret store label: $(ls -Zd "$H/.config/basalt-agent/secrets" | awk '{print $1}')"
  echo "secret file label: $(ls -Z "$H/.config/basalt-agent/secrets/labcred.env" | awk '{print $1}')"
  # Rules that would let a domain read the keys: open or read a file, or
  # list the directory (search and mmap are granted to all home content).
  for src in basalt_agent_domain basalt_agent_t container_t; do
    echo "allow rules $src -> basalt_agent_secret_t (file open/read, dir read): $(sesearch -A -s $src -t basalt_agent_secret_t -c file -p open,read 2>/dev/null | wc -l) + $(sesearch -A -s $src -t basalt_agent_secret_t -c dir -p read 2>/dev/null | wc -l)"
    echo "allow rules $src -> basalt_agent_proxy_t (process, file, dir, lnk_file): $(sesearch -A -s $src -t basalt_agent_proxy_t -c process,file,dir,lnk_file 2>/dev/null | wc -l)"
  done
}

avc() { ausearch -m AVC,USER_AVC,SELINUX_ERR -ts "${1:-today}" -i 2>/dev/null | grep -E 'basalt_agent|basalt-agent' || echo "no basalt_agent AVC denials"; }

case "${1:-}" in
  setup) setup ;;
  native) native ;;
  container) container ;;
  credsetup) credsetup ;;
  credentials) credentials "${2:-native}" ;;
  selinux) selinux_proof ;;
  avc) avc "${2:-today}" ;;
  *) echo "usage: driver.sh setup|native|container|credsetup|credentials MODE|selinux|avc"; exit 2 ;;
esac
