#!/usr/bin/env bash
# Client test of the APT repository: fresh Debian 13, Ubuntu 26.04 and Ubuntu
# 24.04 containers set it up the documented way (the key dearmored into
# /etc/apt/keyrings/openbasalt.gpg, a deb822 .sources file with Signed-By),
# run apt update and install packages from it.
#
#   scripts/release/client-test-apt.sh SOURCE KEY PACKAGE...
#
# SOURCE  a tree from scripts/release/sign-apt.sh (its apt/ served over HTTP
#         on 127.0.0.1:$CLIENT_TEST_PORT for the test), or a base URL such
#         as https://obpkg.org/apt
# KEY     the armored public key (a file, or an https URL)
# CLIENT_TEST_SUITE   default stable
# CLIENT_TEST_IMAGES  default: Debian 13, Ubuntu 26.04 and Ubuntu 24.04
# CLIENT_TEST_RECOMMENDS=1 installs Recommends too (the Samba packages).
#
# Each container also checks that every installed package is the one the
# index lists and runs "<binary> version" for each binary in /usr/bin of the
# installed packages. Then the failure modes: apt must refuse a tampered
# InRelease (local trees only) and a repository signed by another key.
source "$(dirname "$0")/../lib.sh"
: "${CLIENT_TEST_PORT:=8791}"
: "${SIGN_PODMAN:=podman}"
: "${CLIENT_TEST_SUITE:=stable}"
: "${CLIENT_TEST_IMAGES:=docker.io/library/debian:13 docker.io/library/ubuntu:26.04 docker.io/library/ubuntu:24.04}"
: "${CLIENT_TEST_RECOMMENDS:=0}"
# Binaries not asked for "version": daemons that start on any argument.
: "${CLIENT_TEST_NO_VERSION:=conductor-helper}"

source_arg="${1:?usage: $0 SOURCE KEY PACKAGE...}"
key="${2:?KEY}"
shift 2
pkgs=("$@")
[[ ${#pkgs[@]} -gt 0 ]] || die "name the packages to install"

work="$(mktemp -d)"
server=""
trap '[[ -n "$server" ]] && kill "$server" 2>/dev/null; rm -rf "$work"' EXIT
if [[ "$key" == https://* ]]; then
  curl -fsSL --proto '=https' -o "$work/key.asc" "$key" || die "cannot fetch $key"
else
  cp "$key" "$work/key.asc"
fi
chmod 644 "$work/key.asc"

if [[ -d "$source_arg" ]]; then
  tree="$(cd "$source_arg" && pwd)"
  [[ -d "$tree/apt/dists/$CLIENT_TEST_SUITE" ]] || die "$tree/apt/dists/$CLIENT_TEST_SUITE missing"
  # Served copy, so the tamper test below never touches the signed tree.
  mkdir -p "$work/www"
  cp -a "$tree/apt" "$work/www/apt"
  chmod -R a+rX "$work/www"
  python3 -m http.server -d "$work/www" -b 127.0.0.1 "$CLIENT_TEST_PORT" >"$work/http.log" 2>&1 &
  server=$!
  base="http://127.0.0.1:$CLIENT_TEST_PORT/apt"
  for _ in $(seq 30); do curl -fs -o /dev/null "$base/dists/$CLIENT_TEST_SUITE/InRelease" && break; sleep 1; done
else
  base="${source_arg%/}"
fi

# client IMAGE SCRIPT [ARGS...]: a fresh container with the repository set up
# as the documentation says; SCRIPT runs after that.
client() {
  local image="$1" script="$2"
  shift 2
  $SIGN_PODMAN run --rm --network host --security-opt label=disable -v "$work/key.asc:/key.asc:ro" \
    -e URL="$base" -e SUITE="$CLIENT_TEST_SUITE" -e RECOMMENDS="$CLIENT_TEST_RECOMMENDS" -e DEBIAN_FRONTEND=noninteractive \
    -e NO_VERSION="$CLIENT_TEST_NO_VERSION" \
    "$image" bash -euc '
    apt-get -qq update >/dev/null
    apt-get -qq install -y --no-install-recommends ca-certificates curl gnupg >/dev/null
    install -d -m 0755 /etc/apt/keyrings
    gpg --dearmor -o /etc/apt/keyrings/openbasalt.gpg /key.asc
    chmod 0644 /etc/apt/keyrings/openbasalt.gpg
    printf "Types: deb\nURIs: %s\nSuites: %s\nComponents: main\nSigned-By: /etc/apt/keyrings/openbasalt.gpg\n" "$URL" "$SUITE" \
      >/etc/apt/sources.list.d/openbasalt.sources
    '"$script" bash "$@"
}

install_script='
  . /etc/os-release
  echo "--- $PRETTY_NAME, apt $(dpkg-query -W -f "\${Version}" apt)"
  apt-get update >/tmp/update.log 2>&1 || { cat /tmp/update.log; exit 1; }
  if grep -E "^(W|E):" /tmp/update.log; then echo "apt update warned or failed"; exit 1; fi
  rec=--no-install-recommends
  [ "$RECOMMENDS" = 1 ] && rec=--install-recommends
  apt-get install -y $rec "$@" >/tmp/install.log 2>&1 || { tail -30 /tmp/install.log; exit 1; }
  for p in "$@"; do
    v=$(dpkg-query -W -f "\${Version}" "$p")
    src=$(apt-cache madison "$p" | awk -F"|" -v v=" $v " "index(\$2, v) {print \$3; exit}")
    echo "$p $v from:$src"
    case "$src" in *obpkg*|*127.0.0.1*) ;; *) echo "$p does not come from the tested repository"; exit 1 ;; esac
    for b in $(dpkg -L "$p" | grep "^/usr/bin/"); do
      # Daemons without a version command (conductor-helper runs on any argument).
      case " $NO_VERSION " in *" ${b##*/} "*) echo "  $b: present (no version command)"; continue ;; esac
      out=$("$b" version 2>&1 | head -1) || { echo "$b version failed: $out"; exit 1; }
      echo "  $b: $out"
    done
  done'

for image in $CLIENT_TEST_IMAGES; do
  log "fresh $image installs ${pkgs[*]} from $base ($CLIENT_TEST_SUITE)"
  client "$image" "$install_script" "${pkgs[@]}" || die "install failed on $image"
done

neg_script='
  if apt-get update >/tmp/update.log 2>&1 && ! grep -qE "^(W|E):.*(openbasalt|127.0.0.1|obpkg)" /tmp/update.log; then
    echo "ACCEPTED"; cat /tmp/update.log; exit 0
  fi
  grep -E "^(W|E):" /tmp/update.log | head -3
  if apt-get install -y "$1" >/tmp/install.log 2>&1; then echo "INSTALLED"; exit 0; fi
  echo "REFUSED"'

if [[ -d "$source_arg" ]]; then
  # The indexes came by hash (Acquire-By-Hash), never by name.
  grep -q 'GET /apt/dists/[^ ]*/by-hash/SHA256/' "$work/http.log" || die "apt did not fetch the indexes by hash"
  if grep -E 'GET /apt/dists/[^ ]*/(Packages[^ ]*|by-hash/SHA512/[^ ]*) ' "$work/http.log"; then
    die "apt fetched an index by name or by an absent hash"
  fi
  log "indexes fetched by hash only"
  for image in $CLIENT_TEST_IMAGES; do
    log "negative ($image): a tampered InRelease must be refused"
    # A change to the signed text (trailing whitespace would not count: the
    # cleartext signature framework ignores it).
    sed -i 's/^Description: \(.\)/Description: X\1/' "$work/www/apt/dists/$CLIENT_TEST_SUITE/InRelease"
    grep -q '^Description: X' "$work/www/apt/dists/$CLIENT_TEST_SUITE/InRelease" || die "tamper edit failed"
    client "$image" "$neg_script" "${pkgs[0]}" >"$work/neg.log" 2>&1 || true
    cp "$tree/apt/dists/$CLIENT_TEST_SUITE/InRelease" "$work/www/apt/dists/$CLIENT_TEST_SUITE/InRelease"
    grep -q '^REFUSED$' "$work/neg.log" || { cat "$work/neg.log" >&2; die "apt accepted a tampered InRelease on $image"; }
    log "refused, as expected: $(grep -m1 -E '^(W|E):' "$work/neg.log")"
  done
fi

log "negative: another key must be refused"
cp "$work/key.asc" "$work/good-key.asc"
gpg_home="$(mktemp -d)"
GNUPGHOME="$gpg_home" gpg --batch --pinentry-mode loopback --passphrase '' --quick-generate-key "other <other@invalid>" rsa3072 sign 1d >/dev/null 2>&1
GNUPGHOME="$gpg_home" gpg --batch --armor --export >"$work/key.asc"
rm -rf "$gpg_home"
for image in $CLIENT_TEST_IMAGES; do
  client "$image" "$neg_script" "${pkgs[0]}" >"$work/neg.log" 2>&1 || true
  grep -q '^REFUSED$' "$work/neg.log" || { cat "$work/neg.log" >&2; die "apt accepted the repository with another key on $image"; }
  log "refused on $image, as expected: $(grep -m1 -E '^(W|E):' "$work/neg.log")"
done
cp "$work/good-key.asc" "$work/key.asc"
log "client test passed"
