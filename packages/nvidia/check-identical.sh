#!/usr/bin/env bash
# Prove the NVIDIA packaging rules on built RPMs (run in a Fedora
# container by scripts/release/build-nonfree.sh, or by hand with rpm2cpio
# and cpio installed):
#
#   packages/nvidia/check-identical.sh RUN_FILE RPM_DIR
#
# 1. Every file packages/nvidia/files.list takes from the .run is in the
#    package it names, byte-identical (SHA-256) to the file in the .run, and
#    every link points where the list says.
# 2. The nvidia-driver* packages hold nothing else but the Agreement, our
#    NOTICE, NVIDIA's README and changelog (also byte-identical) and our
#    power preset: no other file, nothing renamed, nothing compressed.
# 3. Every binary package of the set ships the Agreement (nvidia-driver*:
#    LICENSE identical to the .run's), or its own license file
#    (kmod-nvidia-open-*: COPYING of the open modules; nvidia-modprobe,
#    nvidia-persistenced, basalt-nvidia: their COPYING or LICENSE).
# 4. Every kernel module in kmod-nvidia-open-* carries an appended module
#    signature, unless SKIP_SIGNATURE_CHECK=1 (unsigned test builds).
set -euo pipefail
run="${1:?usage: $0 RUN_FILE RPM_DIR}"
dir="${2:?usage: $0 RUN_FILE RPM_DIR}"
here="$(cd "$(dirname "$0")" && pwd)"
list="$here/files.list"
# shellcheck source=/dev/null
source "$here/source.conf"
v="$NVIDIA_VERSION"
lib=/usr/lib64

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail=0
bad() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }
# Same content (sha256sum: coreutils, unlike cmp, is in every container).
same() { [[ "$(sha256sum <"$1")" == "$(sha256sum <"$2")" ]]; }

echo "$NVIDIA_RUN_SHA256  $run" | sha256sum -c --quiet - || { echo "the .run is not $NVIDIA_VERSION (checksum)" >&2; exit 1; }
sh "$run" --extract-only --target "$work/run" >/dev/null
echo "$NVIDIA_LICENSE_SHA256  $work/run/LICENSE" | sha256sum -c --quiet - || { echo "the Agreement in the .run changed" >&2; exit 1; }

# Unpack each binary RPM into its own tree.
declare -A tree=()
shopt -s nullglob
for r in "$dir"/*.rpm; do
  case "$r" in *.src.rpm) continue ;; esac
  name="$(rpm -qp --qf '%{NAME}' "$r" 2>/dev/null)"
  case "$name" in nvidia-driver* | kmod-nvidia-open* | nvidia-modprobe | nvidia-persistenced | basalt-nvidia) ;; *) continue ;; esac
  mkdir -p "$work/rpm/$name"
  (cd "$work/rpm/$name" && rpm2cpio "$r" | cpio -idm --quiet)
  tree[$name]="$work/rpm/$name"
done
[[ -n "${tree[nvidia-driver-libs]:-}" ]] || { echo "no nvidia-driver-libs RPM in $dir" >&2; exit 1; }

# 1. The list.
declare -A listed=()
n=0
while read -r kind sub a b c; do
  case "$kind" in
    file)
      src="${b//@V@/$v}"; dst="${c//@V@/$v}"; dst="${dst//@LIB@/$lib}"
      t="${tree[nvidia-driver-$sub]:-}"
      [[ -n "$t" ]] || { bad "package nvidia-driver-$sub missing"; continue; }
      if [[ ! -f "$t$dst" || -L "$t$dst" ]]; then bad "$dst missing from nvidia-driver-$sub"; continue; fi
      same "$work/run/$src" "$t$dst" || bad "$dst differs from $src in the .run"
      listed["$sub:$dst"]=1; n=$((n + 1)) ;;
    link)
      dst="${a//@V@/$v}"; dst="${dst//@LIB@/$lib}"; tgt="${b//@V@/$v}"
      t="${tree[nvidia-driver-$sub]:-}"
      [[ "$(readlink "$t$dst" 2>/dev/null)" == "$tgt" ]] || bad "link $dst does not point to $tgt"
      listed["$sub:$dst"]=1 ;;
  esac
done < <(grep -Ev '^[[:space:]]*(#|$)' "$list")
echo "checked $n NVIDIA files against the .run"

# 2 and 3. Nothing else in the nvidia-driver* packages; the Agreement everywhere.
for name in "${!tree[@]}"; do
  t="${tree[$name]}"
  case "$name" in
    nvidia-driver*)
      sub="${name#nvidia-driver}"; sub="${sub#-}"
      lic="$t/usr/share/licenses/$name/LICENSE"
      [[ -f "$lic" ]] || { bad "$name has no LICENSE (the Agreement)"; }
      [[ -f "$lic" ]] && { same "$lic" "$work/run/LICENSE" || bad "$name: LICENSE is not the .run's Agreement"; }
      [[ -f "$t/usr/share/licenses/$name/NOTICE" ]] || bad "$name has no NOTICE"
      while IFS= read -r -d '' f; do
        p="${f#"$t"}"
        [[ -n "${listed["$sub:$p"]:-}" ]] && continue
        case "$p" in
          /usr/share/licenses/"$name"/LICENSE | /usr/share/licenses/"$name"/NOTICE) ;;
          /usr/share/doc/nvidia-driver/README.txt) same "$f" "$work/run/README.txt" || bad "$p differs from the .run" ;;
          /usr/share/doc/nvidia-driver/NVIDIA_Changelog) same "$f" "$work/run/NVIDIA_Changelog" || bad "$p differs from the .run" ;;
          /usr/lib/systemd/system-preset/70-nvidia-power.preset) [[ "$name" == nvidia-driver-power ]] || bad "$p in $name" ;;
          *) bad "$name ships $p, which is not in files.list" ;;
        esac
      done < <(find "$t" \( -type f -o -type l \) -print0)
      ;;
    kmod-nvidia-open-*)
      [[ -f "$(find "$t/usr/share/licenses" -name 'COPYING*' -print -quit 2>/dev/null)" ]] || bad "$name has no COPYING"
      if [[ "${SKIP_SIGNATURE_CHECK:-0}" != 1 ]]; then
        while IFS= read -r -d '' ko; do
          tail -c 28 "$ko" | grep -q '~Module signature appended~' || bad "$name: ${ko#"$t"} is not signed"
        done < <(find "$t" -name '*.ko' -print0)
      fi
      ;;
    kmod-nvidia-open) ;;
    *)
      [[ -n "$(find "$t/usr/share/licenses" -type f 2>/dev/null | head -1)" ]] || bad "$name has no license file"
      ;;
  esac
done

[[ $fail == 0 ]] || { echo "the NVIDIA packages break the packaging rules" >&2; exit 1; }
echo "ok: NVIDIA files byte-identical to $(basename "$run"), Agreement in every nvidia-driver package, licenses present$([[ "${SKIP_SIGNATURE_CHECK:-0}" == 1 ]] || echo ", modules signed")"
