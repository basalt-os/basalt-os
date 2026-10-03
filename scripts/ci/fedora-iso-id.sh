#!/usr/bin/env bash
# Print "<file name> <sha256>" of the current Fedora Everything netinst ISO,
# read from the release's CHECKSUM file. CI uses it as the cache key for
# the ISO download; it does not verify anything (scripts/iso.sh fetch checks
# the CHECKSUM signature and the ISO itself).
source "$(dirname "$0")/../lib.sh"

: "${FEDORA_ISO_BASE:=https://dl.fedoraproject.org/pub/fedora/linux/releases/$FEDORA_RELEASE/Everything/$ARCH/iso}"

sums="$(curl -fsSL --retry 3 "$FEDORA_ISO_BASE/" | grep -oE "Fedora-Everything-[0-9.]+-[0-9.]+-$ARCH-CHECKSUM" | head -1)"
[[ -n "$sums" ]] || die "no CHECKSUM file at $FEDORA_ISO_BASE"
line="$(curl -fsSL --retry 3 "$FEDORA_ISO_BASE/$sums" | sed -n 's/^SHA256 (\(Fedora-Everything-netinst-[^)]*\.iso\)) = \([0-9a-f]\{64\}\)$/\1 \2/p')"
[[ -n "$line" ]] || die "no netinst ISO in $sums"
echo "$line"
