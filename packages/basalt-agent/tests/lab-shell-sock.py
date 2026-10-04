#!/usr/bin/env python3
# A stand-in for the Basalt desktop shell control socket (lab only). It
# accepts connections and answers a "decide" (confirm) with an error, as
# the real daemon does for the agent role: agents request, they never
# confirm. Used to show an agent cannot confirm its own proposal.
import json
import os
import socket

path = os.path.join(os.environ["XDG_RUNTIME_DIR"], "basalt-shell", "shell.sock")
try:
    os.unlink(path)
except FileNotFoundError:
    pass
s = socket.socket(socket.AF_UNIX)
s.bind(path)
os.chmod(path, 0o600)
s.listen(8)
while True:
    try:
        c, _ = s.accept()
    except OSError:
        break
    with c:
        data = c.recv(4096)
        try:
            req = json.loads(data.decode().splitlines()[0])
            op = req.get("op")
        except Exception:
            op = None
        # The agent role may read and propose, never decide/execute.
        if op in ("decide", "execute"):
            reply = {"id": req.get("id"), "ok": False, "error": "role agent cannot confirm"}
        else:
            reply = {"id": req.get("id"), "ok": True, "result": {"role": "agent"}}
        c.sendall((json.dumps(reply) + "\n").encode())
