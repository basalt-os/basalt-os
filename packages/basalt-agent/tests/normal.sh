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
ok sh -c 'curl -fsS -o /dev/null https://registry.npmjs.org/left-pad'
ok sh -c 'npm install --no-audit --no-fund --silent left-pad && test -d node_modules/left-pad'
ok sh -c 'ls "$HOME" && echo agent-config >"$HOME/.agent-test"'
