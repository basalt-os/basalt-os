#!/usr/bin/env python3
"""Raw client of the approval gate for the lab (packages/basalt-gate/tests).

    gateclient.py REQUEST-JSON...   send each request line on one connection,
                                    print each reply line; LAST_ID in a
                                    request is the id of the previous reply

Used by driver.sh to send what the shipped command line never sends (a
claim with a wrong digest, a decision from an agent domain). Exit status 1
when any reply has "ok": false.
"""
import json
import os
import socket
import sys


def main():
    path = os.environ.get("BASALT_GATE_SOCKET", "/run/basalt-gate/gate.sock")
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(60)
    try:
        s.connect(path)
    except OSError as e:
        print(json.dumps({"ok": False, "error": "connect: %s" % e}))
        return 1
    f = s.makefile("rwb")
    status = 0
    last_id = ""
    for arg in sys.argv[1:]:
        arg = arg.replace("LAST_ID", last_id)
        f.write(arg.encode() + b"\n")
        f.flush()
        line = f.readline()
        if not line:
            print(json.dumps({"ok": False, "error": "connection closed"}))
            return 1
        sys.stdout.write(line.decode())
        reply = json.loads(line)
        last_id = reply.get("id") or last_id
        if not reply.get("ok"):
            status = 1
    return status


if __name__ == "__main__":
    sys.exit(main())
