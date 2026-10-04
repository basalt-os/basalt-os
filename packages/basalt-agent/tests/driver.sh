#!/usr/bin/env bash
# basalt-agent escape-test driver. Runs on the lab VM as root. It sets up a
# realistic home with decoy credentials, starts a long-lived "victim"
# session, then runs allowed work and the escape attempts in both modes and
# collects the evidence. Nothing here is shipped; the fixtures are fake.
#
#   driver.sh setup        create users, decoys, a victim session
#   driver.sh native       allowed work + attacks, native mode
#   driver.sh container    allowed work + attacks, container mode
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

avc() { ausearch -m AVC,USER_AVC,SELINUX_ERR -ts "${1:-today}" -i 2>/dev/null | grep -E 'basalt_agent|basalt-agent' || echo "no basalt_agent AVC denials"; }

case "${1:-}" in
  setup) setup ;;
  native) native ;;
  container) container ;;
  avc) avc "${2:-today}" ;;
  *) echo "usage: driver.sh setup|native|container|avc"; exit 2 ;;
esac
