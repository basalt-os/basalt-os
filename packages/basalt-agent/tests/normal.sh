#!/usr/bin/env bash
# Allowed work inside an agent session: every line must print OK and the
# session must leave zero SELinux denials.
cd "${1:-.}" || exit 1
ok() { if "$@" >/dev/null 2>&1; then echo "OK   $*"; else echo "FAIL $*"; fi; }
ok cat README
ok sh -c 'echo change >>README'
ok mkdir -p build/out
ok sh -c 'printf "all:\n\t@echo built >build/out/result\n" >Makefile && make'
ok rm -rf build
ok python3 -c 'print(sum(range(10)))'
ok node -e 'console.log([1,2,3].map(x => x * 2))'
ok sh -c 't=$(mktemp) && echo x >"$t" && rm "$t"'
ok git status
ok git add -A
ok git -c user.name=agent -c user.email=agent@example.invalid commit -qm "agent change"
ok git log --oneline -1
# A local server the agent starts and reaches (a dev or test server, the
# callback of a command line login): listen on 127.0.0.1, then connect.
ok python3 -c 'import http.server as h, threading as t, urllib.request as u; s = h.HTTPServer(("127.0.0.1", 0), h.SimpleHTTPRequestHandler); t.Thread(target=s.serve_forever, daemon=True).start(); assert u.build_opener(u.ProxyHandler({})).open("http://127.0.0.1:%d/" % s.server_port, timeout=5).status == 200'
ok node -e 'const s = require("http").createServer((q, r) => r.end("ok")).listen(0, "127.0.0.1", () => { s.close(); })'
ok sh -c 'curl -fsS -o /dev/null https://registry.npmjs.org/left-pad'
ok sh -c 'npm install --no-audit --no-fund --silent left-pad && test -d node_modules/left-pad'
ok sh -c 'ls "$HOME" && echo agent-config >"$HOME/.agent-test"'
