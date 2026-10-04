#!/usr/bin/env bash
# Allowed network work inside a labnet session (direct egress on): every
# line must print OK, and the session must leave zero SELinux denials.
ok() { if timeout 30 "$@" >/dev/null 2>&1; then echo "OK   $*"; else echo "FAIL $*"; fi; }
ok curl -fsS -o /dev/null https://registry.npmjs.org/left-pad
ok curl -fsS --noproxy '*' -o /dev/null https://registry.npmjs.org/left-pad
ok getent hosts registry.npmjs.org
ok sh -c 'curl -fsS --noproxy "*" http://model.basalt-lab.test:8008/ | grep -q LAB-HTTP-OK'
ok sh -c 'curl -fsS http://model.basalt-lab.test:8008/ | grep -q LAB-HTTP-OK'
ok sh -c 'cd "$(mktemp -d)" && npm install --no-audit --no-fund --silent left-pad && test -d node_modules/left-pad'
ok python3 -c 'import socket; socket.create_connection(("registry.npmjs.org", 443), 10).close()'
ok git status
