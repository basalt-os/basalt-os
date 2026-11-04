#!/usr/bin/env bash
# Print the license texts of every Go module vendored into cotty (the
# modules the binaries link), one section per module, in the order of
# vendor/modules.txt. Fails on a module without a license file.
#
#   third-party-licenses.sh VENDOR_DIR
#
# Used by cotty.spec (%install) and packages/cotty/build-deb.sh, so the
# .rpm and the .deb carry the same text.
set -euo pipefail
vendor="${1:?usage: $0 VENDOR_DIR}"
[[ -f "$vendor/modules.txt" ]] || { echo "third-party-licenses.sh: no $vendor/modules.txt" >&2; exit 1; }

printf 'Third-party license texts of cotty\n\n'
printf 'cotty links the Go standard library (BSD-3-Clause, see the Go\n'
printf 'distribution) and the modules below.\n'
# "# <module> <version>" lines name the modules; the packages follow them.
grep '^# ' "$vendor/modules.txt" | while read -r _ mod ver _; do
  dir="$vendor/$mod"
  [[ -d "$dir" ]] || continue
  found=0
  while IFS= read -r -d '' f; do
    printf '\n==== %s %s: %s ====\n\n' "$mod" "$ver" "${f#"$dir"/}"
    cat "$f"
    found=1
  done < <(find "$dir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) -print0 | sort -z)
  [[ "$found" == 1 ]] || { echo "third-party-licenses.sh: no license file for $mod" >&2; exit 1; }
done
