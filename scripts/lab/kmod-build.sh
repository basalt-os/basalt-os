#!/usr/bin/env bash
# Build the lab test module (packages/lab/kmodtest) for one kernel and sign
# it four ways, in a Fedora container with that kernel's kernel-devel.
#
#   scripts/lab/kmod-build.sh KERNEL_VERSION     e.g. 7.2.8-200.fc44.x86_64
#
# Output in $LAB_DIR/kmod/<kernel>/:
#   unsigned.ko   no signature
#   foreign.ko    signed with a throwaway key the machine does not know
#   ca.ko         signed directly with the module CA key (the enrolled MOK)
#   signed.ko     signed with the module signing key issued by that CA
# Keys come from scripts/lab/sb-keys.sh; nothing is printed but file names.
source "$(dirname "$0")/../lib.sh"

kver="${1:?kernel version (uname -r of the VM)}"
keys="$LAB_DIR/sb-keys"
[[ -s "$keys/module-signing.key" ]] || die "no module keys (scripts/lab/sb-keys.sh)"
out="$LAB_DIR/kmod/$kver"
install -d -m 0755 "$out"
work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
cp "$REPO_ROOT/packages/lab/kmodtest/"* "$work/"
release="$(sed -n 's/.*\.fc\([0-9]*\)\..*/\1/p' <<<"$kver")"

in_fedora -v "$work:/src" -v "$keys:/keys:ro" -v "$out:/out" -e KVER="$kver" \
  "registry.fedoraproject.org/fedora:${release:-$FEDORA_RELEASE}" bash -euc '
  dnf -q -y install gcc make elfutils-libelf-devel openssl "kernel-devel-$KVER" >/dev/null 2>&1 || {
    # Older kernels leave the mirrors; Koji keeps every build.
    v="${KVER%-*}"; r="${KVER##*-}"; r="${r%.x86_64}"
    dnf -q -y install gcc make elfutils-libelf-devel openssl >/dev/null
    dnf -q -y install "https://kojipkgs.fedoraproject.org/packages/kernel/$v/$r/x86_64/kernel-devel-$KVER.rpm" >/dev/null
  }
  kdir="/usr/src/kernels/$KVER"
  make -s -C "$kdir" M=/src modules >/dev/null
  sf="$kdir/scripts/sign-file"
  umask 077
  openssl req -x509 -new -newkey rsa:2048 -nodes -days 2 -subj "/CN=Foreign module key/" \
    -keyout /tmp/foreign.key -out /tmp/foreign.pem 2>/dev/null
  openssl x509 -in /tmp/foreign.pem -outform DER -out /tmp/foreign.der
  cp /src/basalt_kmodtest.ko /out/unsigned.ko
  cp /src/basalt_kmodtest.ko /out/foreign.ko; "$sf" sha512 /tmp/foreign.key /tmp/foreign.der /out/foreign.ko
  cp /src/basalt_kmodtest.ko /out/ca.ko; "$sf" sha512 /keys/module-ca.key /keys/module-ca.der /out/ca.ko
  cp /src/basalt_kmodtest.ko /out/signed.ko; "$sf" sha512 /keys/module-signing.key /keys/module-signing.der /out/signed.ko
  chmod 0644 /out/*.ko
'
sudo chown -R "$(id -u):$(id -g)" "$out"
ls -l "$out"
