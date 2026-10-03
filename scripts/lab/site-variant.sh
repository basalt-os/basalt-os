#!/usr/bin/env bash
# A lab site directory that differs from the default lab site in a few
# install choices, for an extra lab ISO.
#
#   scripts/lab/site-variant.sh NAME KEY=VALUE...
#   e.g. site-variant.sh tang BASALT_UNLOCK=tang BASALT_PROFILE=minimal
#
# Writes $LAB_DIR/site-NAME/ (site.ks from $SITE_DIR, site.conf with the
# overrides appended). Build its ISO with:
#   SITE_DIR=$LAB_DIR/site-NAME SITE_NAME=lab-NAME make iso
source "$(dirname "$0")/../lib.sh"
name="${1:?variant name}"; shift
: "${SITE_DIR:?set SITE_DIR (the default lab site, scripts/lab/keys.sh)}"
out="$LAB_DIR/site-$name"
install -d -m 0755 "$out"
cp "$SITE_DIR/site.ks" "$out/site.ks"
{ cat "$SITE_DIR/site.conf"; echo "# variant $name"; printf '%s\n' "$@"; } >"$out/site.conf"
chmod 0644 "$out/site.conf" "$out/site.ks"
echo "$out"; sed -n '/# variant/,$p' "$out/site.conf"
