#!/usr/bin/env bash
# Generate lab-only key material and the lab install site files.
#
#   $GNUPGHOME_LAB/ (0700)     development repository signing key (RSA 4096):
#     passphrase (0600)        its passphrase
#     keyid                    its fingerprint
#     RPM-GPG-KEY-basalt-lab   the public key (BASALT_GPG_PUBKEY for builds)
#   $LAB_DIR/keys/ (0700)
#     vm_ed25519[.pub]         SSH key for root on lab installs
#     authorized_keys          that key plus EXTRA_SSH_PUBKEYS
#   $LAB_DIR/site/             site.conf and site.ks for the lab ISO
#
# These are development keys. A release signing key is never generated on a
# build host: it is created offline and kept in the project's vault. Nothing
# here prints private material.
source "$(dirname "$0")/../lib.sh"

: "${LAB_REPO_URL:?set LAB_REPO_URL (the URL lab VMs use for the repository)}"
umask 077

# --- repository signing key ----------------------------------------------------
install -d -m 0700 "$GNUPGHOME_LAB"
if [[ ! -s "$GNUPGHOME_LAB/keyid" ]]; then
  log "generating the development repository signing key in $GNUPGHOME_LAB"
  openssl rand -hex 32 >"$GNUPGHOME_LAB/passphrase"
  GNUPGHOME="$GNUPGHOME_LAB" gpg --batch --quiet --pinentry-mode loopback \
    --passphrase-file "$GNUPGHOME_LAB/passphrase" \
    --quick-generate-key "Basalt OS lab repository (development key, not for release) <lab@basalt-os.invalid>" \
    rsa4096 sign 2y
  GNUPGHOME="$GNUPGHOME_LAB" gpg --batch --with-colons --list-secret-keys |
    awk -F: '$1 == "fpr" && !f {f = $10} END {print f}' >"$GNUPGHOME_LAB/keyid"
  GNUPGHOME="$GNUPGHOME_LAB" gpgconf --kill gpg-agent || true
fi
GNUPGHOME="$GNUPGHOME_LAB" gpg --batch --armor --export "$(cat "$GNUPGHOME_LAB/keyid")" \
  >"$GNUPGHOME_LAB/RPM-GPG-KEY-basalt-lab"
chmod 0644 "$GNUPGHOME_LAB/RPM-GPG-KEY-basalt-lab"

# --- SSH ----------------------------------------------------------------------------
keys="$LAB_DIR/keys"
install -d -m 0700 "$keys"
if [[ ! -f "$keys/vm_ed25519" ]]; then
  log "generating the lab VM SSH key"
  ssh-keygen -q -t ed25519 -N '' -C basalt-lab-vm -f "$keys/vm_ed25519"
fi
{
  cat "$keys/vm_ed25519.pub"
  if [[ -n "${EXTRA_SSH_PUBKEYS:-}" ]]; then
    for f in $EXTRA_SSH_PUBKEYS; do cat "$f"; done
  fi
} >"$keys/authorized_keys"
chmod 0644 "$keys/authorized_keys"

# --- site files for unattended lab installs -----------------------------------------------
site="$LAB_DIR/site"
install -d -m 0755 "$site"
cat >"$site/site.conf" <<EOF
# Lab install choices (read by the kickstart's %pre).
BASALT_ENCRYPT=1
BASALT_FINISH=poweroff
# Lab only: the key stays in /root, scripts/lab/install.sh moves it off the VM.
BASALT_RECOVERY_KEY=store
BASALT_REPO_URL=$LAB_REPO_URL
EOF
{
  # Anaconda accepts one sshkey line per user, so the keys are written in %post.
  echo "# Lab accounts: root with SSH keys only."
  echo "rootpw --lock"
  echo "%post --log=/root/basalt-install-site.log"
  echo "install -d -m 0700 /root/.ssh"
  echo "cat >/root/.ssh/authorized_keys <<'EOF'"
  cat "$keys/authorized_keys"
  echo "EOF"
  echo "chmod 0600 /root/.ssh/authorized_keys"
  echo "restorecon -R /root/.ssh"
  echo "%end"
} >"$site/site.ks"
chmod 0644 "$site/site.conf" "$site/site.ks"

echo "signing key: $(cat "$GNUPGHOME_LAB/keyid")"
find "$GNUPGHOME_LAB" "$keys" "$site" -maxdepth 1 -type f -printf '%m %p\n' | sort
