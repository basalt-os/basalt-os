#!/usr/bin/env bash
# Test of basalt-dnf-user-keys against a fake system dnf cache: it copies
# the keys of the Basalt repositories' keyrings into the user's keyring of
# the same repository, and nothing else.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
fail=0
check() { if eval "$2"; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi; }

sys=$t/sys
mkdir -p "$sys/basalt-0123456789abcdef/pubring" "$sys/basalt-testing-fedcba9876543210/pubring" \
  "$sys/fedora-0123456789abcdef/pubring" "$sys/basalt-install-basalt-0123456789abcdef/pubring" \
  "$sys/basalt-tools-0123456789abcdef/pubring"
echo key1 >"$sys/basalt-0123456789abcdef/pubring/E4EED5ECA395B302.pub"
echo key2 >"$sys/basalt-testing-fedcba9876543210/pubring/E4EED5ECA395B302.pub"
echo other >"$sys/fedora-0123456789abcdef/pubring/AAAA.pub"
echo local >"$sys/basalt-install-basalt-0123456789abcdef/pubring/BBBB.pub"
echo writable >"$sys/basalt-tools-0123456789abcdef/pubring/CCCC.pub"
chmod 644 "$sys"/*/pubring/*.pub
chmod 666 "$sys/basalt-tools-0123456789abcdef/pubring/CCCC.pub"
ln -s "$sys/basalt-0123456789abcdef/pubring/E4EED5ECA395B302.pub" "$sys/basalt-testing-fedcba9876543210/pubring/LINK.pub"

home=$t/home
mkdir -p "$home/.cache/libdnf5/basalt-testing-fedcba9876543210/pubring"
echo mine >"$home/.cache/libdnf5/basalt-testing-fedcba9876543210/pubring/E4EED5ECA395B302.pub"
run() { HOME=$home XDG_CACHE_HOME= BASALT_DNF_SYSTEM_CACHE=$sys BASALT_DNF_KEYS_OWNER=$(id -u) "$here/basalt-dnf-user-keys"; }
run
u=$home/.cache/libdnf5
check "basalt keyring seeded" '[ "$(cat "$u/basalt-0123456789abcdef/pubring/E4EED5ECA395B302.pub")" = key1 ]'
check "an existing key is kept" '[ "$(cat "$u/basalt-testing-fedcba9876543210/pubring/E4EED5ECA395B302.pub")" = mine ]'
check "symlinks are not followed" '[ ! -e "$u/basalt-testing-fedcba9876543210/pubring/LINK.pub" ]'
check "other repositories are left alone" '[ ! -e "$u/fedora-0123456789abcdef" ]'
check "names other than a Basalt channel are left alone" '[ ! -e "$u/basalt-install-basalt-0123456789abcdef" ]'
check "a key others can write is not copied" '[ ! -e "$u/basalt-tools-0123456789abcdef/pubring/CCCC.pub" ]'
HOME=$home XDG_CACHE_HOME= BASALT_DNF_SYSTEM_CACHE=$sys BASALT_DNF_KEYS_OWNER=999999 "$here/basalt-dnf-user-keys"
rm -rf "$u/basalt-0123456789abcdef"
HOME=$home XDG_CACHE_HOME= BASALT_DNF_SYSTEM_CACHE=$sys BASALT_DNF_KEYS_OWNER=999999 "$here/basalt-dnf-user-keys"
check "keyrings of another owner are not trusted" '[ ! -e "$u/basalt-0123456789abcdef" ]'
run
check "a second run is quiet and idempotent" '[ "$(cat "$u/basalt-0123456789abcdef/pubring/E4EED5ECA395B302.pub")" = key1 ]'
exit $fail
