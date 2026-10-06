#!/usr/bin/env bash
# Checks of the speech model manifest (run by scripts/ci/lint.sh):
# - every row has five columns, a SHA-256 and a positive size;
# - every URL is https and pinned: a Hugging Face "resolve/<commit>" URL;
# - Whisper and Silero models are MIT;
# - every Piper voice is one trained on public-domain data (allowlist below)
#   and is labeled public-domain-data; other Piper voices are refused;
# - the default and multilingual sets of basalt-voice-fetch exist in the
#   manifest, and every Whisper model comes from one pinned revision;
# - basalt-voice-fetch refuses a file whose checksum does not match.
#
#   packages/basalt-voice/tests/manifest-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
manifest="$here/models.manifest"
fetch="$here/basalt-voice-fetch"
fail=0
bad() { printf 'manifest-test: %s\n' "$*" >&2; fail=1; }

# Piper voices whose training data is public domain (see the manifest
# header): LJ Speech, Kristin and Norman (LibriVox), John (from Kristin).
piper_ok='^en_US-(ljspeech|kristin|norman|john)-(medium|high)(\.json)?$'

rows=0
while read -r name sha size lic url extra; do
  [[ -z "$name" || "$name" == \#* ]] && continue
  rows=$((rows + 1))
  [[ -z "${extra:-}" && -n "${url:-}" ]] || { bad "$name: expected 5 columns"; continue; }
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || bad "$name: bad sha256"
  [[ "$size" =~ ^[1-9][0-9]*$ ]] || bad "$name: bad size"
  [[ "$url" =~ ^https://huggingface\.co/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/resolve/[0-9a-f]{40}/ ]] ||
    bad "$name: URL not pinned to a revision: $url"
  case "$name" in
    ggml-*) [[ "$lic" == MIT ]] || bad "$name: Whisper and Silero models must be MIT, got $lic" ;;
    en_*)
      [[ "$name" =~ $piper_ok ]] || bad "$name: Piper voice not trained on public-domain data"
      [[ "$lic" == public-domain-data ]] || bad "$name: Piper voice must be labeled public-domain-data" ;;
    *) bad "$name: unknown model family" ;;
  esac
done <"$manifest"
((rows > 0)) || bad "no rows"

for n in ggml-base.en ggml-silero-v5.1.2 en_US-ljspeech-medium en_US-ljspeech-medium.json; do
  awk -v n="$n" '$1 == n {f = 1} END {exit !f}' "$manifest" || bad "default model $n missing"
done
# The multilingual set (speech in languages other than English).
for n in ggml-small-q5_1 ggml-silero-v5.1.2; do
  awk -v n="$n" '$1 == n {f = 1} END {exit !f}' "$manifest" || bad "multilingual model $n missing"
done
grep -q 'multilingual | default-multilingual) names+=(ggml-small-q5_1 ggml-silero-v5.1.2)' "$fetch" || bad "fetch has no multilingual set"
# Every Whisper model of one pinned whisper.cpp revision.
revs=$(awk '!/^#/ && $1 ~ /^ggml-/ && $1 !~ /silero/ {split($5, p, "/"); print p[7]}' "$manifest" | sort -u | wc -l)
((revs == 1)) || bad "Whisper models come from $revs revisions, want one"

# The allowlist itself: Piper voices fine-tuned from lessac (every
# pt_BR voice, for example) or trained on CC BY data are not allowed.
for v in en_US-lessac-medium en_US-amy-medium pt_BR-faber-medium pt_BR-cadu-medium; do
  [[ ! "$v" =~ $piper_ok ]] || bad "allowlist accepts $v"
done

# --verify reports a model file whose checksum does not match (no network:
# a local file and a manifest row with a wrong checksum).
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
printf 'hello\n' >"$tmp/m.bin"
printf 'm 0000000000000000000000000000000000000000000000000000000000000000 6 MIT https://huggingface.co/x/y/resolve/0000000000000000000000000000000000000000/m.bin\n' >"$tmp/manifest"
if "$fetch" --manifest "$tmp/manifest" --dir "$tmp" --verify >/dev/null 2>&1; then
  bad "verify accepted a file with a wrong checksum"
fi

[[ $fail == 0 ]] || exit 1
echo "manifest-test: $rows models ok"
