#!/usr/bin/env bash
# Runs inside the Containerfile build. Turns the Fedora bootc base into Basalt OS:
# branding, packages, services, firewall, locale, and build-time assertions.
# Inputs: BASALT_VERSION (build arg), /ctx/packages/*.txt (bind mount).
set -euo pipefail

: "${BASALT_VERSION:?BASALT_VERSION build arg is required}"
ctx=/ctx
TUI_TOOLS_FPR=767CFB337B01F32FFC073F3F389120B277E4FB44

step() { printf '\n### %s\n' "$*"; }
list() { grep -vE '^[[:space:]]*(#|$)' "$1" || true; }

# The base release, read before the branding swap rewrites os-release.
base_version="$(. /usr/lib/os-release && echo "$VERSION_ID")"
base_name="$(. /usr/lib/os-release && echo "$NAME")"

step "verify the tui-tools signing key fingerprint"
key=/etc/pki/rpm-gpg/RPM-GPG-KEY-tui-tools
# /root is not populated during a container build; give gpg its own home.
GNUPGHOME="$(mktemp -d)"; export GNUPGHOME
fpr="$(gpg --show-keys --with-colons "$key" 2>/dev/null | awk -F: '/^fpr:/ && !seen {print $10; seen=1}')"
if [[ "$fpr" != "$TUI_TOOLS_FPR" ]]; then
  echo "tui-tools key fingerprint mismatch: got '$fpr'" >&2
  exit 1
fi
rm -rf "$GNUPGHOME"; unset GNUPGHOME
rpm --import "$key"

step "replace distribution branding with generic-release"
# generic-release conflicts with the fedora-release* packages, so allowerasing
# removes them (and console-login-helper-messages-issuegen, which requires a
# path only fedora-release provides). The package repositories and their GPG
# keys (fedora-repos, fedora-gpg-keys) stay: Basalt OS is built on Fedora's
# packages and says so.
dnf -y install --allowerasing generic-release

step "install packages"
# shellcheck disable=SC2046
dnf -y install $(list "$ctx/packages/base.txt")
# shellcheck disable=SC2046
dnf -y install $(list "$ctx/packages/tui-tools.txt")
remove="$(list "$ctx/packages/remove.txt")"
if [[ -n "$remove" ]]; then
  # shellcheck disable=SC2086
  dnf -y remove $remove
fi

step "os-release"
# Written after the package transactions so generic-release does not overwrite it.
cat >/usr/lib/os-release <<EOF
NAME="Basalt OS"
VERSION="${BASALT_VERSION} (pre-alpha)"
ID=basalt
ID_LIKE=fedora
VERSION_ID=${BASALT_VERSION}
PRETTY_NAME="Basalt OS ${BASALT_VERSION} (pre-alpha)"
VARIANT="Server"
VARIANT_ID=server
ANSI_COLOR="0;38;2;163;71;46"
LOGO=basalt-logo
DEFAULT_HOSTNAME="basalt"
HOME_URL="https://basalt-os.org/"
BUG_REPORT_URL="https://github.com/basalt-os/basalt-os/issues"
PLATFORM_ID="platform:f${base_version}"
BASALT_BASE="${base_name} ${base_version}"
EOF
ln -sf ../usr/lib/os-release /etc/os-release
printf 'Basalt OS %s (pre-alpha)\n' "$BASALT_VERSION" >/etc/system-release
printf 'cpe:/o:basalt-os:basalt-os:%s\n' "$BASALT_VERSION" >/etc/system-release-cpe

# Console banner and login message.
cat >/etc/issue <<'EOF'
\S{PRETTY_NAME} \n \l
Kernel \r on \m

EOF
cp /etc/issue /etc/issue.net
cat >/usr/lib/motd.d/10-basalt <<EOF

  Basalt OS ${BASALT_VERSION} (pre-alpha, based on ${base_name} ${base_version})
  Not for production use. Image-based: updates with 'bootc upgrade',
  rollback with 'bootc rollback'. tui-tools: run 'tui-tools'.

EOF
install -d /usr/share/basalt
printf '%s\n' "$BASALT_VERSION" >/usr/share/basalt/version

step "SELinux policy modules"
# Small local modules for denials seen on Basalt OS boots (selinux/*.cil).
for mod in /ctx/selinux/*.cil; do
  semodule -i "$mod"
done

step "locale"
printf 'LANG=en_US.UTF-8\n' >/etc/locale.conf

step "services"
systemctl enable firewalld.service auditd.service sshd.service
# A server boots to the multi-user target; no graphical target.
systemctl set-default multi-user.target

step "initramfs"
# Rebuild the initramfs so it carries Basalt OS's os-release (the initrd
# reports it in boot messages and as its default hostname). Same dracut
# configuration as the base image; no host-only optimizations.
kver="$(ls /usr/lib/modules)"
[[ "$(wc -w <<<"$kver")" == 1 ]] || { echo "expected one kernel, got: $kver" >&2; exit 1; }
# /root is a symlink to /var/roothome, which only exists on a booted system;
# dracut wants to install it, so give it a temporary target.
mkdir -p /var/roothome
dracut --no-hostonly --reproducible --force --kver "$kver" "/usr/lib/modules/$kver/initramfs.img"
rmdir /var/roothome
initrd_release="$(lsinitrd "/usr/lib/modules/$kver/initramfs.img" -f usr/lib/initrd-release)"
grep -q '^ID=basalt' <<<"$initrd_release" || { echo "initrd-release is not Basalt OS" >&2; exit 1; }

step "firewall: default zone public (SSH only, see /etc/firewalld/zones/public.xml)"
if [[ "$(firewall-offline-cmd --get-default-zone)" != public ]]; then
  firewall-offline-cmd --set-default-zone=public
fi

step "assertions"
check() { "$@" || { echo "assertion failed: $*" >&2; exit 1; }; }
check grep -qx 'SELINUX=enforcing' /etc/selinux/config
check grep -qx 'SELINUXTYPE=targeted' /etc/selinux/config
loaded_modules="$(semodule -l)"
for mod in /ctx/selinux/*.cil; do
  check grep -qx "$(basename "$mod" .cil)" <<<"$loaded_modules"
done
if rpm -qa | grep -E '^fedora-(release|logos)'; then
  echo "assertion failed: Fedora branding packages still installed" >&2
  exit 1
fi
# sshd needs a host key to check its configuration; use a throwaway one.
ssh-keygen -q -t ed25519 -N '' -f /tmp/hostkey
sshd_eff="$(sshd -T -h /tmp/hostkey -C user=root,host=check,addr=192.0.2.1)"
check grep -qx 'passwordauthentication no' <<<"$sshd_eff"
check grep -qx 'kbdinteractiveauthentication no' <<<"$sshd_eff"
check grep -qxE 'permitrootlogin (prohibit-password|without-password)' <<<"$sshd_eff"
rm -f /tmp/hostkey /tmp/hostkey.pub
check test "$(firewall-offline-cmd --get-default-zone)" = public
check test "$(firewall-offline-cmd --zone=public --list-services)" = ssh
check systemctl is-enabled --quiet firewalld.service
check test "$(systemctl get-default)" = multi-user.target
check test -x /usr/bin/basalt-install
for tool in $(list "$ctx/packages/tui-tools.txt"); do check command -v "$tool"; done

step "clean up"
dnf clean all
rm -rf /var/cache/* /var/log/* /var/lib/dnf /tmp/* /var/tmp/* /run/dnf
