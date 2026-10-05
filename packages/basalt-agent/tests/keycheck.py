#!/usr/bin/env python3
"""Lab check (never shipped): did a key reach any host but the provider?

    keycheck.py MOCK_LOG KEY_RE

Reads the mock servers' request log (mockprovider.py) and reports every
request to an exfil host that carried text matching KEY_RE anywhere.
"""
import json
import re
import sys

rx = re.compile(sys.argv[2])
prov = exfil = leaked = 0
paths = []
for line in open(sys.argv[1]):
    e = json.loads(line)
    if e["server"] == "provider":
        prov += 1
        continue
    exfil += 1
    paths.append(e["path"])
    if rx.search(json.dumps(e)):
        leaked += 1
        print("KEY LEAKED to", e["server"], e["path"])
print(f"exfil requests received: {exfil} ({', '.join(paths)}), carrying the key: {leaked}; provider requests: {prov}")
print("PASS no key reached an exfil host" if leaked == 0 else "FAIL the key reached an exfil host")
sys.exit(1 if leaked else 0)
