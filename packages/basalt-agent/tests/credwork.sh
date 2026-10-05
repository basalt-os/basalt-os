#!/usr/bin/env bash
# Allowed work with a proxy-held key, inside a labcred session: every line
# must print OK, and the session must leave zero SELinux denials.
cd "${1:-.}" || exit 1
P=http://mock-provider.basalt-lab.test
ok() { local n=$1; shift; if "$@" >/tmp/w.out 2>&1; then echo "OK   $n"; else echo "FAIL $n: $(tail -c 300 /tmp/w.out | tr '\n' ' ')"; fi; }
has() { local n=$1 pat=$2; shift 2; if "$@" 2>/tmp/w.err | tee /tmp/w.out | grep -q "$pat"; then echo "OK   $n"; else echo "FAIL $n: $(tail -c 300 /tmp/w.out /tmp/w.err | tr '\n' ' ')"; fi; }
ok session-variable test -n "$BASALT_AGENT_SESSION"
ok placeholder-only test "$ANTHROPIC_API_KEY" = basalt-agent-key-injected-by-session-proxy
ok base-url test "$ANTHROPIC_BASE_URL" = "$P"
# Proxied plain HTTP (absolute URI): the proxy adds the key, TLS upstream.
has curl-route '"key_ok": true' curl -sS --max-time 20 "$P/v1/check"
# A CONNECT tunnel to port 80 (what Node's fetch does): served by the proxy.
has curl-tunnel-route '"key_ok": true' curl -sS --max-time 20 --proxytunnel "$P/v1/check"
has node-fetch-route '"key_ok": true' node -e "fetch('$P/v1/check').then(r=>r.text()).then(t=>console.log(t)).catch(e=>{console.error(e);process.exit(1)})"
has python-urllib-route '"key_ok": true' python3 -c "import urllib.request;print(urllib.request.urlopen('$P/v1/check',timeout=20).read().decode())"
# Claude Code end to end against the mock provider, through the proxy.
has claude-code MOCK-REPLY-OK timeout 120 claude -p "Reply with OK" --model claude-sonnet-4-5
ok registry curl -fsS --max-time 20 -o /dev/null https://registry.npmjs.org/left-pad
