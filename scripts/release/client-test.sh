#!/usr/bin/env bash
# Client test of a Basalt OS repository: a fresh Fedora container installs
# packages from it with gpgcheck=1 and repo_gpgcheck=1, as an installed
# system does with basalt-release's basalt.repo.
#
#   scripts/release/client-test.sh SOURCE KEY [PACKAGE...]
#
# SOURCE  a tree from scripts/release/sign.sh (served over HTTP on
#         127.0.0.1:$CLIENT_TEST_PORT for the test), or a base URL such as
#         https://obpkg.org/basalt or https://obpkg.org/basalt-tools
# KEY     the armored public key (a file, or an https URL)
# PACKAGE default: basalt-release basalt-logos (required for other repositories)
# OB_REPO the repository in a local tree: basalt (default), basalt-tools,
#         basalt-testing or basalt-nonfree. Other repositories are enabled too (Fedora's), so
#         dependencies resolve as on an installed system.
#
# CLIENT_TEST_DEPS_URL, optional: a second signed repository enabled next
# to the tested one, for dependencies that are not in Fedora (for example
# https://obpkg.org/basalt when testing basalt-testing: basalt-shell-selinux
# needs basalt-agent-selinux), checked with CLIENT_TEST_DEPS_KEY (a file or
# https URL, default the published OpenBasalt release key).
#
# It also checks the failure modes: dnf must refuse to install when the
# metadata signature does not match (a tampered repomd.xml, local trees
# only) and when the key is a different one.
source "$(dirname "$0")/../lib.sh"
: "${CLIENT_TEST_PORT:=8790}"
: "${SIGN_PODMAN:=podman}"

source_arg="${1:?usage: $0 SOURCE KEY [PACKAGE...]}"
key="${2:?KEY}"
shift 2
pkgs=("$@")
: "${OB_REPO:=basalt}"
case "$OB_REPO" in basalt | basalt-tools | basalt-testing | basalt-nonfree | basalt-nonfree-testing) ;; *) die "OB_REPO must be basalt, basalt-tools, basalt-testing, basalt-nonfree or basalt-nonfree-testing" ;; esac
if [[ ${#pkgs[@]} -eq 0 ]]; then
  [[ "$OB_REPO" == basalt ]] || die "name the packages to install from $OB_REPO"
  pkgs=(basalt-release basalt-logos)
fi

work="$(mktemp -d)"
server=""
trap '[[ -n "$server" ]] && kill "$server" 2>/dev/null; rm -rf "$work"' EXIT

if [[ "$key" == https://* ]]; then
  curl -fsSL --proto '=https' -o "$work/key.asc" "$key" || die "cannot fetch $key"
else
  cp "$key" "$work/key.asc"
fi
chmod 644 "$work/key.asc"
: "${CLIENT_TEST_DEPS_URL:=}"
: "${CLIENT_TEST_DEPS_KEY:=https://obpkg.org/keys/openbasalt-release-key.asc}"
if [[ -n "$CLIENT_TEST_DEPS_URL" ]]; then
  [[ "$CLIENT_TEST_DEPS_URL" =~ ^https?://[^[:space:]]+$ ]] || die "CLIENT_TEST_DEPS_URL must be an http(s) URL"
  if [[ "$CLIENT_TEST_DEPS_KEY" == https://* ]]; then
    curl -fsSL --proto '=https' -o "$work/deps-key.asc" "$CLIENT_TEST_DEPS_KEY" || die "cannot fetch $CLIENT_TEST_DEPS_KEY"
  else
    cp "$CLIENT_TEST_DEPS_KEY" "$work/deps-key.asc"
  fi
else
  : >"$work/deps-key.asc"
fi
chmod 644 "$work/deps-key.asc"

if [[ -d "$source_arg" ]]; then
  tree="$(cd "$source_arg" && pwd)"
  # Served copy, so the tamper test below never touches the signed tree.
  [[ -d "$tree/$OB_REPO" ]] || die "$tree/$OB_REPO missing (set OB_REPO)"
  cp -a "$tree/$OB_REPO" "$work/www"
  chmod -R a+rX "$work/www"
  python3 -m http.server -d "$work/www" -b 127.0.0.1 "$CLIENT_TEST_PORT" >/dev/null 2>&1 &
  server=$!
  base="http://127.0.0.1:$CLIENT_TEST_PORT"
  for _ in $(seq 30); do curl -fs -o /dev/null "$base/" && break; sleep 1; done
else
  base="${source_arg%/}"
fi

client() {
  # $1: repository base URL; the rest: dnf arguments.
  local url="$1"
  shift
  $SIGN_PODMAN run --rm --network host --security-opt label=disable -v "$work/key.asc:/key.asc:ro" \
    -v "$work/deps-key.asc:/deps-key.asc:ro" -e URL="$url" -e DEPS_URL="${CLIENT_TEST_DEPS_URL%/}" \
    "$FEDORA_IMAGE" bash -euc '
    if [ -n "$DEPS_URL" ]; then
      cat >/etc/yum.repos.d/basalt-deps.repo <<EOF
[basalt-deps]
name=Basalt OS dependencies \$releasever - \$basearch
baseurl=$DEPS_URL/\$releasever/\$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///deps-key.asc
metadata_expire=0
EOF
    fi
    cat >/etc/yum.repos.d/basalt-test.repo <<EOF
[basalt-test]
name=Basalt OS test \$releasever - \$basearch
baseurl=$URL/\$releasever/\$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///key.asc
metadata_expire=0
EOF
    "$@"' bash "$@"
}

log "fresh Fedora $FEDORA_RELEASE installs ${pkgs[*]} from $base (gpgcheck=1, repo_gpgcheck=1)"
client "$base" bash -c 'dnf -y --disablerepo="*" --enablerepo=basalt-test makecache >/dev/null &&
  dnf -y --setopt=install_weak_deps=False install --allowerasing "$@" >/tmp/dnf.log 2>&1 || { tail -30 /tmp/dnf.log; exit 1; }
  rpm -q "$@"
  rpm -q --qf "%{NAME}: %{RSAHEADER:pgpsig}\n" "$@"' bash "${pkgs[@]}" || die "install failed"

if [[ -d "$source_arg" ]]; then
  log "negative: a tampered repomd.xml must be refused"
  sed -i 's|<revision>|<revision>9|' "$work/www/$FEDORA_RELEASE/$ARCH/repodata/repomd.xml"
  # dnf5 makecache only warns; an install must fail on the bad signature.
  if client "$base" dnf -y install "${pkgs[0]}" >"$work/neg.log" 2>&1; then
    die "dnf accepted a tampered repomd.xml"
  fi
  grep -q 'repomd.xml GPG signature verification error' "$work/neg.log" || { cat "$work/neg.log" >&2; die "unexpected failure"; }
  log "refused, as expected: $(grep -m1 'signature verification error' "$work/neg.log")"
  cp "$tree/$OB_REPO/$FEDORA_RELEASE/$ARCH/repodata/repomd.xml" "$work/www/$FEDORA_RELEASE/$ARCH/repodata/repomd.xml"
fi

log "negative: another key must be refused"
$SIGN_PODMAN run --rm --network none --security-opt label=disable -v "$work:/w" "$FEDORA_IMAGE" bash -euc '
  export GNUPGHOME=$(mktemp -d)
  gpg --batch --pinentry-mode loopback --passphrase "" --quick-generate-key "other <other@invalid>" rsa2048 sign 1d >/dev/null 2>&1
  gpg --batch --armor --export >/w/key.asc 2>/dev/null'
chmod 644 "$work/key.asc"
if client "$base" dnf -y install "${pkgs[0]}" >"$work/neg.log" 2>&1; then
  die "dnf accepted the repository with another key"
fi
grep -qi 'signature' "$work/neg.log" || { cat "$work/neg.log" >&2; die "unexpected failure"; }
log "refused, as expected: $(grep -m1 -i 'signature' "$work/neg.log")"
log "client test passed"
