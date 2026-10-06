#!/usr/bin/env bash
# Unit tests for the model selection of basalt-llm (basalt-llm-select) and
# for how basalt-llm-start and basalt-llm-fetch use it. No network, no
# root, no model files: fake sysfs, meminfo, cgroup files and a fake
# server stand in for the machine.
#
#   packages/basalt-llm/tests/select-test.sh
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=../basalt-llm-select
source "$here/basalt-llm-select"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fails=0
pass=0

eq() { # eq DESCRIPTION EXPECTED GOT
  if [[ "$2" == "$3" ]]; then
    pass=$((pass + 1))
  else
    printf 'FAIL %s: expected [%s], got [%s]\n' "$1" "$2" "$3" >&2
    fails=$((fails + 1))
  fi
}
has() { # has DESCRIPTION NEEDLE HAYSTACK
  if [[ "$3" == *"$2"* ]]; then
    pass=$((pass + 1))
  else
    printf 'FAIL %s: [%s] not in [%s]\n' "$1" "$2" "$3" >&2
    fails=$((fails + 1))
  fi
}

# --- basalt_llm_pick: cores, available MiB, limit MiB -----------------------------
while read -r cores avail limit want why; do
  eq "pick $why" "$want" "$(basalt_llm_pick "$cores" "$avail" "$limit")"
done <<'CASES'
4   3584  3072        1.7b  exactly_at_every_threshold
8   16000 1073741824  1.7b  large_machine_no_limit
3   16000 1073741824  0.6b  three_cores
2   3500  3072        0.6b  target_vm_2vcpu_4gib
4   3583  3072        0.6b  available_one_mib_short
4   8000  3071        0.6b  limit_one_mib_short
4   0     3072        0.6b  meminfo_unreadable
1   512   1073741824  0.6b  tiny_machine
x   8000  3072        0.6b  malformed_cores
4   -1    3072        0.6b  malformed_available
4   8000  ""          0.6b  empty_limit
CASES
eq "pick with no arguments" 0.6b "$(basalt_llm_pick "" "" "")"

# --- aliases ----------------------------------------------------------------------
eq "alias 0.6b" basalt-translator-0.6b-q8_0 "$(basalt_llm_alias 0.6b)"
eq "alias 1.7b" basalt-translator-1.7b-q8_0 "$(basalt_llm_alias 1.7b)"
eq "alias passthrough" qwen3-1.7b-q8_0 "$(basalt_llm_alias qwen3-1.7b-q8_0)"
eq "alias path" /x/y.gguf "$(basalt_llm_alias /x/y.gguf)"

# --- cores from a fake CPU topology -----------------------------------------------
cpu() { # cpu DIR N SIBLINGS
  mkdir -p "$1/cpu$2/topology"
  echo "$3" >"$1/cpu$2/topology/core_cpus_list"
}
smt="$tmp/smt"   # 2 cores, 2 threads each: cpu0+2, cpu1+3
cpu "$smt" 0 0,2; cpu "$smt" 1 1,3; cpu "$smt" 2 0,2; cpu "$smt" 3 1,3
eq "cores SMT 4 threads 2 cores" 2 "$(basalt_llm_cores "$smt" 4)"
vm="$tmp/vm"     # a VM: every vCPU its own core
for i in 0 1 2 3 4 5; do cpu "$vm" "$i" "$i"; done
eq "cores VM 6 vCPU" 6 "$(basalt_llm_cores "$vm" 6)"
eq "cores limited by affinity" 2 "$(basalt_llm_cores "$vm" 2)"
eq "cores without topology" 4 "$(basalt_llm_cores "$tmp/none" 4)"
old="$tmp/old"   # older kernels: thread_siblings_list only
mkdir -p "$old/cpu0/topology" "$old/cpu1/topology"
echo 0-1 >"$old/cpu0/topology/thread_siblings_list"
echo 0-1 >"$old/cpu1/topology/thread_siblings_list"
eq "cores from thread_siblings_list" 1 "$(basalt_llm_cores "$old" 2)"

# --- memory -----------------------------------------------------------------------
printf 'MemTotal:       16384000 kB\nMemFree:  100 kB\nMemAvailable:    3670016 kB\n' >"$tmp/meminfo"
eq "MemAvailable in MiB" 3584 "$(basalt_llm_avail_mib "$tmp/meminfo")"
printf 'MemTotal: 1 kB\n' >"$tmp/meminfo-old"
eq "MemAvailable missing" 0 "$(basalt_llm_avail_mib "$tmp/meminfo-old")"
eq "meminfo unreadable" 0 "$(basalt_llm_avail_mib "$tmp/nonexistent")"

eq "limit max" "$BASALT_LLM_UNLIMITED" "$(basalt_llm_mib_of max)"
eq "limit infinity" "$BASALT_LLM_UNLIMITED" "$(basalt_llm_mib_of infinity)"
eq "limit empty" "$BASALT_LLM_UNLIMITED" "$(basalt_llm_mib_of "")"
eq "limit 3G" 3072 "$(basalt_llm_mib_of 3221225472)"
eq "limit garbage" 0 "$(basalt_llm_mib_of 3G)"

cg="$tmp/cg"
mkdir -p "$cg"
echo 3221225472 >"$cg/memory.high"; echo 4294967296 >"$cg/memory.max"
eq "cgroup shipped unit (MemoryHigh=3G, MemoryMax=4G)" 3072 "$(basalt_llm_cgroup_limit_mib "$cg")"
echo max >"$cg/memory.high"; echo 2147483648 >"$cg/memory.max"
eq "cgroup MemoryMax=2G" 2048 "$(basalt_llm_cgroup_limit_mib "$cg")"
echo max >"$cg/memory.high"; echo max >"$cg/memory.max"
eq "cgroup unlimited" "$BASALT_LLM_UNLIMITED" "$(basalt_llm_cgroup_limit_mib "$cg")"
eq "cgroup files missing" "$BASALT_LLM_UNLIMITED" "$(basalt_llm_cgroup_limit_mib "$tmp/nocg")"

# --- basalt_llm_auto: choice and log line -----------------------------------------
basalt_llm_auto 8 12000 3072
eq "auto large" basalt-translator-1.7b-q8_0 "$BASALT_LLM_CHOICE"
has "auto reason numbers" "auto: 8 CPU cores, 12000 MiB available, memory limit 3072 MiB" "$BASALT_LLM_REASON"
basalt_llm_auto 2 3000 "$BASALT_LLM_UNLIMITED"
eq "auto small" basalt-translator-0.6b-q8_0 "$BASALT_LLM_CHOICE"
has "auto reason no limit" "memory limit none" "$BASALT_LLM_REASON"

# --- basalt-llm-start: resolution, log, fallback ----------------------------------
models="$tmp/models"
mkdir -p "$models" "$tmp/run"
cat >"$tmp/fake-server" <<'SH'
#!/usr/bin/env bash
echo "server $*"
SH
chmod +x "$tmp/fake-server"
start() { # start MODEL: output of basalt-llm-start (stdout and stderr), never fails
  env -i PATH="$PATH" MODEL="$1" BASALT_LLM_MODEL_DIR="$models" BASALT_LLM_SERVER="$tmp/fake-server" \
    RUNTIME_DIRECTORY="$tmp/run" bash "$here/basalt-llm-start" 2>&1 || true
}
out="$(start auto)"
has "start auto, nothing downloaded" "is missing (auto:" "$out"
has "start auto points to fetch" "run: basalt-llm-fetch recommended" "$out"
# The published stand-in alone (what the desktop downloads while the
# translators are unpublished): auto runs it (it is a candidate on every
# machine).
: >"$models/qwen3-0.6b-q8_0.gguf"
out="$(start auto)"
has "start auto uses the stand-in when no translator is downloaded" "--model $models/qwen3-0.6b-q8_0.gguf" "$out"
has "start auto says why it runs the stand-in" "is not downloaded, qwen3-0.6b-q8_0 is" "$out"
: >"$models/basalt-translator-0.6b-q8_0.gguf"
out="$(start auto)"
has "start auto uses the 0.6B when it is the only one" "--model $models/basalt-translator-0.6b-q8_0.gguf" "$out"
has "start auto logs the choice" "basalt-llm: model basalt-translator-0.6b-q8_0 (auto:" "$out"
has "start alias is the model name" "--alias basalt-translator-0.6b-q8_0" "$out"
out="$(start 1.7b)"
has "start explicit 1.7b without file fails" "basalt-translator-1.7b-q8_0.gguf is missing (MODEL=1.7b" "$out"
: >"$models/basalt-translator-1.7b-q8_0.gguf"
out="$(start 1.7b)"
has "start explicit 1.7b" "--model $models/basalt-translator-1.7b-q8_0.gguf" "$out"
out="$(start 0.6b)"
has "start explicit 0.6b" "--model $models/basalt-translator-0.6b-q8_0.gguf" "$out"
out="$(start "")"
has "start empty MODEL means auto" "(auto:" "$out"
: >"$tmp/mine.gguf"
out="$(start "$tmp/mine.gguf")"
has "start absolute path" "--model $tmp/mine.gguf --alias mine" "$out"
out="$(start 'bad name')"
has "start invalid name" "invalid model name" "$out"

# --- stand-ins and the candidates of auto -----------------------------------------
eq "stand-in of the 0.6B" qwen3-0.6b-q8_0 "$(basalt_llm_standin basalt-translator-0.6b-q8_0)"
eq "stand-in of the 1.7B" qwen3-1.7b-q8_0 "$(basalt_llm_standin basalt-translator-1.7b-q8_0)"
eq "stand-in passthrough" qwen3-4b-q4_k_m "$(basalt_llm_standin qwen3-4b-q4_k_m)"
eq "candidates of the 1.7B" "basalt-translator-1.7b-q8_0 basalt-translator-0.6b-q8_0 qwen3-1.7b-q8_0 qwen3-0.6b-q8_0" \
  "$(basalt_llm_candidates basalt-translator-1.7b-q8_0)"
eq "candidates of the 0.6B never include a 1.7B" "basalt-translator-0.6b-q8_0 qwen3-0.6b-q8_0" \
  "$(basalt_llm_candidates basalt-translator-0.6b-q8_0)"

# --- basalt-llm-fetch: unpublished entries fail closed ----------------------------
fetch() { # fetch ARGS...: output and exit status of basalt-llm-fetch
  local rc=0
  out="$(bash "$here/basalt-llm-fetch" --manifest "$here/models.manifest" --dir "$tmp/fetched" "$@" 2>&1)" || rc=$?
  echo "rc=$rc $out"
}
for m in 0.6b 1.7b basalt-translator-0.6b-q8_0 auto; do
  out="$(fetch "$m")"
  has "fetch $m fails" "rc=1" "$out"
  has "fetch $m says why" "is not yet published" "$out"
  has "fetch $m points to the key ceremony" "key ceremony" "$out"
done
r=0; [[ ! -e "$tmp/fetched/basalt-translator-0.6b-q8_0.gguf.part" ]] || r=1
eq "fetch left no partial file" 0 "$r"
out="$(fetch --list)"
has "list shows unpublished" "basalt-translator-1.7b-q8_0" "$out"
has "list marks unpublished" "absent, not yet published" "$out"
has "list shows the auto choice" "MODEL=auto selects basalt-translator-" "$out"
# recommended: the auto choice, or its published stand-in (the plan says
# which and how big, without root and without downloading).
out="$(fetch --plan recommended)"
has "plan recommended is a published stand-in" "rc=0 qwen3-" "$out"
has "plan recommended names the host" "huggingface.co absent" "$out"
out="$(fetch --plan nosuchmodel)"
has "plan unknown model" "rc=2" "$out"
out="$(fetch --list --porcelain)"
has "porcelain list marks unpublished" "basalt-translator-0.6b-q8_0 0 Apache-2.0,unpublished - absent" "$out"
out="$(fetch nosuchmodel)"
has "fetch unknown model" "unknown model nosuchmodel" "$out"
# Every published entry has a real checksum and an https URL; every
# unpublished one is a placeholder that can never verify.
while read -r n sha _size _lic url status; do
  case "$status" in
    published)
      r=0; [[ "$sha" =~ ^[0-9a-f]{64}$ && ! "$sha" =~ ^0+$ && "$url" == https://* ]] || r=1
      eq "manifest $n published entry is real" 0 "$r"
      ;;
    unpublished)
      r=0; [[ "$sha" =~ ^0{64}$ && "$url" == - ]] || r=1
      eq "manifest $n placeholder" 0 "$r"
      ;;
    *) eq "manifest $n status" "published|unpublished" "$status" ;;
  esac
done < <(awk '!/^#/ && NF >= 6' "$here/models.manifest")

echo "select-test: $pass passed, $fails failed"
((fails == 0))
