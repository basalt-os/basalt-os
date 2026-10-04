#!/usr/bin/env bash
# Escape attempts from inside an agent session. Each line prints DENIED
# (the attempt was blocked) or ESCAPED (it worked). Targets come from the
# file $1 (KEY=VALUE). All fixtures are fake (marker LAB-FAKE).
# shellcheck disable=SC1090
. "$1"
try() { # try NAME COMMAND...: ESCAPED when the command succeeds
  local name=$1; shift
  if timeout 15 "$@" >/tmp/a.out 2>&1; then echo "ESCAPED $name"; else echo "DENIED  $name"; fi
}
leak() { # leak NAME MARKER COMMAND...: ESCAPED when MARKER shows up
  local name=$1 marker=$2; shift 2
  if timeout 15 "$@" 2>/dev/null | grep -q "$marker"; then echo "ESCAPED $name"; else echo "DENIED  $name"; fi
}
# --- credentials and other data in the user's home ---
leak read-ssh-key LAB-FAKE cat "$USER_HOME/.ssh/id_ed25519"
leak read-gnupg-key LAB-FAKE cat "$USER_HOME/.gnupg/private-keys-v1.d/lab.key"
leak read-keyring LAB-FAKE cat "$USER_HOME/.local/share/keyrings/login.keyring"
leak read-browser-profile LAB-FAKE cat "$USER_HOME/.mozilla/firefox/lab.default/key4.db"
leak read-other-project LAB-FAKE cat "$USER_HOME/src/other/secret.txt"
leak read-other-user LAB-FAKE cat "$OTHER_HOME/secret.txt"
leak list-home "id_ed25519" ls -a "$USER_HOME"
# --- writing outside the project ---
try write-home sh -c "echo x >'$USER_HOME/evil.txt'"
try append-bashrc sh -c "echo evil >>'$USER_HOME/.bashrc'"
try write-git-hook sh -c 'echo "#!/bin/sh" >.git/hooks/pre-commit && chmod +x .git/hooks/pre-commit'
try write-tools sh -c "echo x >>'$USER_HOME/.local/share/basalt-agent/tools/labshell/x'"
# --- network beyond the allowlist ---
try proxy-unlisted-host curl -fsS --max-time 10 -o /dev/null https://example.com
try proxy-ip-literal curl -fsS --max-time 10 -o /dev/null https://1.1.1.1
try direct-connect-noproxy curl -fsS --noproxy '*' --connect-timeout 5 -o /dev/null https://1.1.1.1
try direct-dns getent hosts example.com
try remote-proxy-port timeout 5 bash -c "echo x >/dev/tcp/$LAB_GW/47100"
# --- another session ---
leak victim-environ "$VICTIM_MARKER" sh -c 'cat /proc/[0-9]*/environ 2>/dev/null | tr "\0" "\n"'
leak victim-secret-file "$VICTIM_MARKER" cat "$VICTIM_RT/secrets.env"
leak victim-project LAB-FAKE cat "$USER_HOME/src/victim/secret.txt"
try victim-control python3 -c "import socket;socket.socket(socket.AF_UNIX).connect('$VICTIM_RT/control.sock')"
# --- the Basalt desktop shell: connect and confirm a proposal ---
try shell-socket-confirm python3 -c "
import socket,sys
s=socket.socket(socket.AF_UNIX); s.settimeout(5); s.connect('$SHELL_SOCK')
s.sendall(b'{\"id\":1,\"op\":\"decide\",\"args\":{\"id\":\"d-1\",\"confirm\":true}}\n')
sys.exit(0 if b'\"ok\":true' in s.recv(4096) else 1)"
# --- the system assistant ---
try assistant-state ls /var/lib/basalt-assistant/proposals
try assistant-apply basalt apply p-lab --yes --confirm 000000
# --- privilege escalation ---
try sudo sudo -n id -u
try su su -c id root
try pkexec pkexec --disable-internal-agent id
try self-grant basalt-agent grant "$VICTIM_SESSION" host evil.example.com
# --- the launcher and the terminal ---
try signal-launcher kill -TERM "$LAUNCHER_PID"
leak launcher-environ XDG_RUNTIME_DIR cat "/proc/$LAUNCHER_PID/environ"
leak read-audit-log session.start cat "$USER_HOME/.local/state/basalt-agent/audit.jsonl"
try tiocsti python3 -c "import fcntl,termios,os;fcntl.ioctl(os.open('/dev/tty',os.O_RDWR),termios.TIOCSTI,b'x')"
# --- escape the container / reach the engine ---
try podman podman ps
try nsenter-host nsenter -t 1 -m -p id
