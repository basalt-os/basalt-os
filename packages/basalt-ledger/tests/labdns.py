#!/usr/bin/env python3
"""Lab-only DNS server for the egress tests (never shipped).

Answers A queries for NAME.basalt-lab.test with the address given on the
command line (the lab VM's own address) and refuses everything else.
systemd-resolved routes the basalt-lab.test domain here, so the session
resolver sees these names through the normal stub, like real DNS names.

    labdns.py LISTEN_IP ANSWER_IP
"""
import socket
import struct
import sys

listen, answer = sys.argv[1], socket.inet_aton(sys.argv[2])
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind((listen, 53))
while True:
    q, addr = s.recvfrom(512)
    if len(q) < 12:
        continue
    qid, flags, qd = struct.unpack(">HHH", q[:6])
    off, labels = 12, []
    while off < len(q) and q[off] != 0:
        n = q[off]
        labels.append(q[off + 1:off + 1 + n].decode(errors="replace"))
        off += 1 + n
    qtype = struct.unpack(">H", q[off + 1:off + 3])[0] if off + 3 <= len(q) else 0
    question = q[12:off + 5]
    name = ".".join(labels).lower()
    ok = name.endswith(".basalt-lab.test")
    rcode = 0 if ok else 5
    an = 1 if ok and qtype == 1 else 0
    resp = struct.pack(">HHHHHH", qid, 0x8180 | rcode, 1, an, 0, 0) + question
    if an:
        resp += struct.pack(">HHHLH", 0xC00C, 1, 1, 60, 4) + answer
    s.sendto(resp, addr)
