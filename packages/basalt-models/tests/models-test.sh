#!/usr/bin/env bash
# Tests of the desktop's consented model downloads (basalt-models), with
# the real basalt-voice-fetch and basalt-llm-fetch against a local HTTPS
# server (a throw-away certificate); no root, no network, no systemd.
#
# - the polkit and policy decision (everyone, administrators, nobody,
#   unknown values fail closed), and the polkit actions' file;
# - request: target and manifest checks, refusals with their exit codes,
#   the request file and the progress file, the service started, ledger
#   records with who consented;
# - the download state machine: queued, downloading (with progress),
#   verifying, done; a checksum mismatch (file deleted, state failed
#   error=checksum); the server unreachable (state failed error=network,
#   partial file kept) and the retry resuming it;
# - the assistant's model: enabling (translator turned on in
#   assistant.conf) then done.
#
#   packages/basalt-models/tests/models-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
pkgs="$(cd "$here/.." && pwd)"
tmp="$(mktemp -d)"
srv_pid=""
cleanup() { [[ -n "$srv_pid" ]] && kill "$srv_pid" 2>/dev/null; rm -rf "$tmp"; }
trap cleanup EXIT
pass=0 fails=0
eq() { if [[ "$2" == "$3" ]]; then pass=$((pass + 1)); else printf 'FAIL %s: expected [%s], got [%s]\n' "$1" "$2" "$3" >&2; fails=$((fails + 1)); fi; }
has() { if [[ "$3" == *"$2"* ]]; then pass=$((pass + 1)); else printf 'FAIL %s: [%s] not in [%s]\n' "$1" "$2" "$3" >&2; fails=$((fails + 1)); fi; }
sv() { sed -n "s/^$1=//p" "$2" 2>/dev/null | head -1; }

export BASALT_MODELS_TEST=1

# --- the decision: policy x how polkit approved x who asks ------------------------
dec() { bash "$here/basalt-models-request" --decide "$@"; }
while read -r policy via uid admin want; do
  eq "decide $policy $via uid=$uid admin=$admin" "$want" "$(dec "$policy" "$via" "$uid" "$admin")"
done <<'CASES'
everyone       person 1000 no  allow
everyone       person 1000 yes allow
administrators person 1000 no  admin
administrators person 1000 yes allow
administrators admin  1000 no  allow
administrators person 0    no  allow
nobody         person 1000 yes deny
nobody         admin  1000 no  deny
nobody         person 0    no  deny
sometimes      person 1000 yes deny
CASES

# The polkit actions: the person's own (no password, active local
# sessions only) and the administrator's (password), each bound to its
# program.
pol="$here/org.basalt-os.models.policy"
if command -v xmllint >/dev/null; then
  r=0; xmllint --noout "$pol" 2>/dev/null || r=1
  eq "policy file is valid XML" 0 "$r"
fi
act() { awk -v id="$1" '$0 ~ "action id=\"" id "\"" {on = 1} on && /<\/action>/ {on = 0} on' "$pol"; }
a1="$(act org.basalt-os.models.download)"
has "person action: active sessions without a password" "<allow_active>yes</allow_active>" "$a1"
has "person action: never inactive" "<allow_inactive>no</allow_inactive>" "$a1"
has "person action: never remote" "<allow_any>no</allow_any>" "$a1"
has "person action runs request" ">/usr/libexec/basalt-models/request<" "$a1"
a2="$(act org.basalt-os.models.download-admin)"
has "admin action needs an administrator" "<allow_active>auth_admin_keep</allow_active>" "$a2"
has "admin action runs request-admin" ">/usr/libexec/basalt-models/request-admin<" "$a2"
has "translated message" 'xml:lang="pt_BR"' "$a1"

# --- a local HTTPS server with the test models ------------------------------------
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$tmp/key.pem" -out "$tmp/cert.pem" -days 1 \
  -subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 >/dev/null 2>&1
export CURL_CA_BUNDLE="$tmp/cert.pem"
mkdir -p "$tmp/www"
head -c 3000000 /dev/urandom >"$tmp/www/ggml-base.en.bin"
head -c 4000 /dev/urandom >"$tmp/www/ggml-silero-v5.1.2.bin"
head -c 5000 /dev/urandom >"$tmp/www/bad.bin"
head -c 2500000 /dev/urandom >"$tmp/www/qwen3-0.6b-q8_0.gguf"
cat >"$tmp/server.py" <<'PY'
# A slow HTTPS file server with byte ranges (resume), for the tests.
import http.server, os, re, ssl, sys, time
root, port, cert, key = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        p = os.path.join(root, os.path.basename(self.path.split("?")[0]))
        if not os.path.isfile(p):
            self.send_response(404); self.end_headers(); return
        data = open(p, "rb").read()
        start = 0
        m = re.match(r"bytes=(\d+)-", self.headers.get("Range", ""))
        if m and int(m.group(1)) < len(data):
            start = int(m.group(1))
            self.send_response(206)
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, len(data) - 1, len(data)))
        else:
            self.send_response(200)
        self.send_header("Content-Length", str(len(data) - start))
        self.end_headers()
        for i in range(start, len(data), 262144):
            self.wfile.write(data[i:i + 262144]); self.wfile.flush(); time.sleep(0.12)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER); ctx.load_cert_chain(cert, key)
s = http.server.ThreadingHTTPServer(("127.0.0.1", port), H)
s.socket = ctx.wrap_socket(s.socket, server_side=True)
s.serve_forever()
PY
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
start_server() {
  python3 "$tmp/server.py" "$tmp/www" "$port" "$tmp/cert.pem" "$tmp/key.pem" &
  srv_pid=$!
  for _ in $(seq 50); do curl -fs -o /dev/null "https://127.0.0.1:$port/ggml-silero-v5.1.2.bin" && return 0; sleep 0.1; done
  echo "test server did not start" >&2; exit 1
}
stop_server() { kill "$srv_pid" 2>/dev/null; wait "$srv_pid" 2>/dev/null || true; srv_pid=""; }
start_server

sha() { sha256sum "$1" | awk '{print $1}'; }
size() { stat -c %s "$1"; }
url="https://127.0.0.1:$port"
# Test manifests: the same columns as the real ones.
{
  echo "ggml-base.en $(sha "$tmp/www/ggml-base.en.bin") $(size "$tmp/www/ggml-base.en.bin") MIT $url/ggml-base.en.bin"
  echo "ggml-base-q5_1 $(sha "$tmp/www/bad.bin") 5000 MIT $url/ggml-base-q5_1.bin"
  echo "ggml-silero-v5.1.2 $(sha "$tmp/www/ggml-silero-v5.1.2.bin") 4000 MIT $url/ggml-silero-v5.1.2.bin"
  # A file whose checksum is not the manifest's.
  echo "ggml-small-q5_1 0000000000000000000000000000000000000000000000000000000000000001 5000 MIT $url/bad.bin"
} >"$tmp/voice.manifest"
{
  echo "qwen3-0.6b-q8_0 $(sha "$tmp/www/qwen3-0.6b-q8_0.gguf") $(size "$tmp/www/qwen3-0.6b-q8_0.gguf") Apache-2.0 $url/qwen3-0.6b-q8_0.gguf published"
  echo "qwen3-1.7b-q8_0 $(sha "$tmp/www/qwen3-0.6b-q8_0.gguf") $(size "$tmp/www/qwen3-0.6b-q8_0.gguf") Apache-2.0 $url/qwen3-0.6b-q8_0.gguf published"
  echo "basalt-translator-0.6b-q8_0 0000000000000000000000000000000000000000000000000000000000000000 0 Apache-2.0 - unpublished"
  echo "basalt-translator-1.7b-q8_0 0000000000000000000000000000000000000000000000000000000000000000 0 Apache-2.0 - unpublished"
} >"$tmp/llm.manifest"

# Wrappers: the real fetch tools on the test manifests and directories.
mkdir -p "$tmp/bin" "$tmp/voice" "$tmp/llm" "$tmp/run"
cat >"$tmp/bin/voice-fetch" <<SH
#!/usr/bin/env bash
exec bash "$pkgs/basalt-voice/basalt-voice-fetch" --manifest "$tmp/voice.manifest" --dir "$tmp/voice" "\$@"
SH
cat >"$tmp/bin/llm-fetch" <<SH
#!/usr/bin/env bash
exec bash "$pkgs/basalt-llm/basalt-llm-fetch" --manifest "$tmp/llm.manifest" --dir "$tmp/llm" "\$@"
SH
# systemctl: records what it was asked; is-active says no.
cat >"$tmp/bin/systemctl" <<SH
#!/usr/bin/env bash
echo "\$*" >>"$tmp/systemctl.log"
[[ "\$1" == is-active ]] && exit 3
[[ "\$1" == show ]] && { echo max; exit 0; }
exit 0
SH
# basalt-ledger api: keeps the records.
cat >"$tmp/bin/ledger" <<SH
#!/usr/bin/env bash
cat >>"$tmp/ledger.jsonl"
SH
chmod +x "$tmp/bin/"*
export BASALT_MODELS_CONF="$tmp/models.conf" BASALT_MODELS_RUN_DIR="$tmp/run"
export BASALT_MODELS_VOICE_FETCH="$tmp/bin/voice-fetch" BASALT_MODELS_LLM_FETCH="$tmp/bin/llm-fetch"
export BASALT_MODELS_SYSTEMCTL="$tmp/bin/systemctl" BASALT_MODELS_LEDGER="$tmp/bin/ledger"
export PATH="$tmp/bin:$PATH" # systemctl show inside basalt-llm-fetch

req() { # req ARGS...: "rc=N output" of the request program (REQ_UID: who asks)
  local rc=0 out
  out="$(PKEXEC_UID="${REQ_UID:-$(id -u)}" bash "$here/basalt-models-request" "$@" 2>"$tmp/req.err")" || rc=$?
  echo "rc=$rc $out $(cat "$tmp/req.err")"
}

# --- request: checks and refusals -------------------------------------------------
echo "downloads = everyone" >"$tmp/models.conf"
has "status reports the policy" "policy=everyone" "$(req status)"
has "bad target" "rc=2" "$(req download voice '../etc/passwd')"
has "target with a space" "rc=2" "$(req download voice 'a b')"
has "unknown kind" "rc=2" "$(req download kernel english)"
has "unknown voice model" "rc=2" "$(req download voice ggml-nosuch)"
has "llm auto is refused (fail closed: unpublished translators)" "rc=2" "$(req download llm auto)"
has "llm unpublished by name" "rc=2" "$(req download llm basalt-translator-0.6b-q8_0)"
printf 'downloads = nobody  # off\n' >"$tmp/models.conf"
out="$(req download voice english)"
has "policy nobody refuses" "rc=10" "$out"
has "policy nobody says why" "turned model downloads off" "$out"
has "refusal is in the ledger" '"event":"model.download.request","outcome":"denied"' "$(cat "$tmp/ledger.jsonl")"
printf '[x]\ndownloads=administrators\nadmin_group = basalt-no-such-group\n' >"$tmp/models.conf"
# uid 65534 (nobody): not root and not in the administrators' group, also
# when the tests run as root (CI containers).
has "administrators: a person who is not one needs an administrator" "rc=11" "$(REQ_UID=65534 req download voice english)"
out="$(BASALT_MODELS_ADMIN_ACTION=1 req download voice english)"
has "administrators: allowed after an administrator's password" "rc=0 started voice-english" "$out"
printf 'downloads = everyone\n' >"$tmp/models.conf"

# --- request: a download is queued and the service started -----------------------
rm -f "$tmp/systemctl.log" "$tmp/ledger.jsonl" "$tmp/run/"*
out="$(req download voice english)"
has "request starts the service" "rc=0 started voice-english" "$out"
has "service started without blocking" "start --no-block basalt-models-fetch@voice-english.service" "$(cat "$tmp/systemctl.log")"
eq "progress file starts queued" queued "$(sv state "$tmp/run/voice-english.state")"
eq "queued total is what is missing" $((3000000 + 4000)) "$(sv total "$tmp/run/voice-english.state")"
eq "request file has the uid" "$(id -u)" "$(sv uid "$tmp/run/voice-english.request")"
eq "request file has the models" "ggml-base.en,ggml-silero-v5.1.2" "$(sv models "$tmp/run/voice-english.request")"
l="$(cat "$tmp/ledger.jsonl")"
has "ledger: who consented" "\"uid\":$(id -u)" "$l"
has "ledger: what and how much" '"what":"english","models":"ggml-base.en,ggml-silero-v5.1.2","size":"3004000"' "$l"
eq "progress file readable by everyone" 644 "$(stat -c %a "$tmp/run/voice-english.state")"

# --- the service: download with progress, verify, done ----------------------------
run() { # run STEP INSTANCE: exit status of the service's step
  local rc=0
  bash "$here/basalt-models-run" "$1" "$2" >"$tmp/run.log" 2>&1 || rc=$?
  echo "$rc"
}
: >"$tmp/seen"
watch_states() { # records every state the progress file goes through
  while :; do
    s=$(sv state "$tmp/run/$1.state"); b=$(sv bytes "$tmp/run/$1.state")
    echo "$s $b" >>"$tmp/seen"
    sleep 0.1
  done
}
watch_states voice-english &
wp=$!
rc=$(run fetch voice-english)
kill "$wp" 2>/dev/null; wait "$wp" 2>/dev/null || true
eq "voice download succeeds" 0 "$rc"
eq "state done" "done" "$(sv state "$tmp/run/voice-english.state")"
eq "done bytes" 3004000 "$(sv bytes "$tmp/run/voice-english.state")"
r=0; grep -q '^downloading [1-9]' "$tmp/seen" || r=1
eq "progress seen while downloading" 0 "$r"
eq "model in place" "$(sha "$tmp/www/ggml-base.en.bin")" "$(sha "$tmp/voice/ggml-base.en.bin")"
eq "detector in place" "$(sha "$tmp/www/ggml-silero-v5.1.2.bin")" "$(sha "$tmp/voice/ggml-silero-v5.1.2.bin")"
eq "model readable by everyone" 644 "$(stat -c %a "$tmp/voice/ggml-base.en.bin")"
has "ledger: download ok with who" '"event":"model.download","outcome":"ok"' "$(cat "$tmp/ledger.jsonl")"
has "plan says present after" "ggml-base.en 3000000 MIT 127.0.0.1:$port present" "$("$tmp/bin/voice-fetch" --plan english)"
rc=$(run fetch voice-english)
eq "a second download of a present set is a no-op" 0 "$rc"
eq "no-op is done" "done" "$(sv state "$tmp/run/voice-english.state")"

# --- checksum mismatch: deleted, failed error=checksum ----------------------------
rc=$(run fetch voice-ggml-small-q5_1)
eq "checksum mismatch exit" 4 "$rc"
eq "checksum mismatch state" failed "$(sv state "$tmp/run/voice-ggml-small-q5_1.state")"
eq "checksum mismatch error" checksum "$(sv error "$tmp/run/voice-ggml-small-q5_1.state")"
r=0; [[ ! -e "$tmp/voice/bad.bin" && ! -e "$tmp/voice/bad.bin.part" ]] || r=1
eq "mismatching file removed" 0 "$r"
has "ledger: failed download with reason" '"outcome":"error"' "$(grep ggml-small "$tmp/ledger.jsonl")"
has "ledger: reason checksum" '"reason":"checksum"' "$(grep ggml-small "$tmp/ledger.jsonl")"

# --- offline: failed error=network, partial kept, resumed later -------------------
rm -f "$tmp/voice/ggml-base.en.bin"
head -c 1000000 "$tmp/www/ggml-base.en.bin" >"$tmp/voice/ggml-base.en.bin.part"
stop_server
rc=$(run fetch voice-english)
eq "offline exit" 3 "$rc"
eq "offline state" failed "$(sv state "$tmp/run/voice-english.state")"
eq "offline error" network "$(sv error "$tmp/run/voice-english.state")"
eq "partial file kept for the retry" 1000000 "$(stat -c %s "$tmp/voice/ggml-base.en.bin.part")"
start_server
rc=$(run fetch voice-english)
eq "retry when the network is back" 0 "$rc"
eq "retry done" "done" "$(sv state "$tmp/run/voice-english.state")"
eq "resumed file verified" "$(sha "$tmp/www/ggml-base.en.bin")" "$(sha "$tmp/voice/ggml-base.en.bin")"

# --- the assistant's model: recommended, enabling, translator on, done ------------
printf '[decision]\nbackend = rules\n\n[translator]\n# comment\nenabled = no\n#prompt = auto\n\n[humanize]\nenabled = no\n' >"$tmp/assistant.conf"
export BASALT_MODELS_ASSISTANT_CONF="$tmp/assistant.conf"
has "llm plan recommended is the stand-in" "qwen3-" "$("$tmp/bin/llm-fetch" --plan recommended)"
out="$(req download llm recommended)"
has "llm request starts the service" "rc=0 started llm-recommended" "$out"
rc=$(run fetch llm-recommended)
eq "llm download succeeds" 0 "$rc"
eq "llm waits for the service before done" enabling "$(sv state "$tmp/run/llm-recommended.state")"
rc=$(run after llm-recommended)
eq "llm after step" 0 "$rc"
eq "llm done" "done" "$(sv state "$tmp/run/llm-recommended.state")"
eq "translator turned on, humanize untouched" "enabled = yes|enabled = no" \
  "$(awk '/^enabled/ {printf "%s%s", s, $0; s = "|"}' "$tmp/assistant.conf")"
has "ledger: model enabled" '"event":"model.enable"' "$(cat "$tmp/ledger.jsonl")"
r=0; ls "$tmp/llm/"qwen3-*.gguf >/dev/null 2>&1 || r=1
eq "llm model in place" 0 "$r"
printf '[decision]\nbackend = rules\n' >"$tmp/assistant.conf"
run after llm-recommended >/dev/null
has "translator section added when missing" "[translator]" "$(cat "$tmp/assistant.conf")"
eq "after for a voice instance changes nothing" 0 "$(run after voice-english)"

# --- remove -----------------------------------------------------------------------
out="$(req remove voice ggml-base.en)"
has "remove a voice model" "rc=0 removed voice ggml-base.en" "$out"
r=0; [[ ! -e "$tmp/voice/ggml-base.en.bin" ]] || r=1
eq "voice model removed" 0 "$r"
has "remove takes a model name" "rc=2" "$(req remove voice english)"
has "ledger: removal" '"event":"model.remove"' "$(cat "$tmp/ledger.jsonl")"

echo "models-test: $pass passed, $fails failed"
((fails == 0))
