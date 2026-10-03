#!/usr/bin/env bash
# Signed Basalt OS RPM repository: $REPO_DIR/<fedora>/<arch>/ (binary) and
# $REPO_DIR/<fedora>/source/ (source RPMs), with GPG-signed packages and a
# detached signature of repodata/repomd.xml (dnf repo_gpgcheck).
#
#   scripts/repo.sh publish [RPM...]   sign and add RPMs (default: everything in
#                                      $BUILD_DIR/rpms/<fedora>/ except lab/), refresh metadata
#   scripts/repo.sh remove NAME        drop all versions of package NAME, refresh metadata
#   scripts/repo.sh verify             check package and metadata signatures
#
# --lab (first argument) works on a separate repository under $REPO_DIR/lab
# instead, for test fixtures that must never reach the release repository
# or the installer media.
#
# Signing uses the key in $GNUPGHOME_LAB (scripts/lab/keys.sh): the key
# fingerprint in keyid, its passphrase in passphrase (0600). The private key
# never leaves that directory and nothing here prints key material.
source "$(dirname "$0")/lib.sh"

if [[ "${1:-}" == --lab ]]; then
  shift
  REPO_DIR="$REPO_DIR/lab"
  mkdir -p "$REPO_DIR"
fi
bin="$REPO_DIR/$FEDORA_RELEASE/$ARCH"
src="$REPO_DIR/$FEDORA_RELEASE/source"

# Run the signing and metadata tools in a Fedora container, with the key
# directory and the repository mounted.
tools() {
  in_fedora -v "$GNUPGHOME_LAB:/gnupg" -v "$REPO_DIR:/repo" -v "$BUILD_DIR:/build:ro" \
    -e GNUPGHOME=/gnupg -e FPR="$(cat "$GNUPGHOME_LAB/keyid")" -e OWNER="$(id -u):$(id -g)" \
    "$FEDORA_IMAGE" bash -euc '
    dnf -q -y install rpm-sign createrepo_c gnupg2 >/dev/null 2>&1 || { echo "dnf install failed" >&2; exit 1; }
    '"$1"'
    gpgconf --kill gpg-agent 2>/dev/null || true
    # Files written in the container belong to the user who runs the script.
    chown -R "$OWNER" /repo /gnupg'
}

sign_and_index() {
  # $1: shell snippet that copies RPMs into /repo/... before indexing.
  tools "
    $1
    unsigned=\$(for f in /repo/$FEDORA_RELEASE/$ARCH/*.rpm /repo/$FEDORA_RELEASE/source/*.rpm; do
      [ -e \"\$f\" ] || continue
      rpm -qp --qf '%{SIGPGP:pgpsig}%{RSAHEADER:pgpsig}%{OPENPGP:pgpsig}\n' \"\$f\" 2>/dev/null | grep -q 'Key ID' || echo \"\$f\"
    done)
    if [ -n \"\$unsigned\" ]; then
      rpmsign --define \"_gpg_name \$FPR\" --define \"_gpg_path /gnupg\" \
        --define \"_gpg_sign_cmd_extra_args --batch --pinentry-mode loopback --passphrase-file /gnupg/passphrase\" \
        --addsign \$unsigned >/dev/null
      echo \"signed: \$(echo \$unsigned | wc -w) package(s)\"
    fi
    for d in /repo/$FEDORA_RELEASE/$ARCH /repo/$FEDORA_RELEASE/source; do
      [ -d \"\$d\" ] || continue
      createrepo_c -q --update \"\$d\"
      rm -f \"\$d/repodata/repomd.xml.asc\"
      gpg --batch --yes --pinentry-mode loopback --passphrase-file /gnupg/passphrase \
        --local-user \"\$FPR\" --detach-sign --armor \"\$d/repodata/repomd.xml\"
    done
    gpg --batch --armor --export \"\$FPR\" >/repo/RPM-GPG-KEY-basalt
  "
}

case "${1:-}" in
  publish)
    shift
    [[ -f "$GNUPGHOME_LAB/keyid" ]] || die "no signing key in $GNUPGHOME_LAB (scripts/lab/keys.sh)"
    mkdir -p "$bin" "$src"
    if [[ $# -gt 0 ]]; then files=("$@"); else mapfile -t files < <(find "$RPM_DIR" -maxdepth 1 -name '*.rpm'); fi
    [[ ${#files[@]} -gt 0 ]] || die "no RPMs to publish (scripts/build-rpms.sh)"
    for f in "${files[@]}"; do
      case "$f" in
        *.src.rpm) cp -f "$f" "$src/" ;;
        *) cp -f "$f" "$bin/" ;;
      esac
      log "added $(basename "$f")"
    done
    sign_and_index ":"
    log "repository: $REPO_DIR/$FEDORA_RELEASE"
    ;;
  remove)
    name="${2:?package name}"
    for f in "$bin"/*.rpm "$src"/*.rpm; do
      [[ -e "$f" ]] || continue
      [[ "$(rpm -qp --qf '%{NAME}' "$f" 2>/dev/null)" == "$name" ]] && { rm -f "$f"; log "removed $(basename "$f")"; }
    done
    sign_and_index ":"
    ;;
  verify)
    # Verify with only the exported public key, not the signing keyring.
    tools "
      export GNUPGHOME=\$(mktemp -d)
      rpm --import /repo/RPM-GPG-KEY-basalt
      bad=0
      for f in /repo/$FEDORA_RELEASE/$ARCH/*.rpm; do rpm -K \"\$f\" | grep -q 'digests signatures OK' || { echo \"BAD \$f\"; bad=1; }; done
      gpg --batch --import /repo/RPM-GPG-KEY-basalt 2>/dev/null
      gpg --batch --verify /repo/$FEDORA_RELEASE/$ARCH/repodata/repomd.xml.asc /repo/$FEDORA_RELEASE/$ARCH/repodata/repomd.xml 2>&1 | grep -E 'Good signature|BAD'
      echo \"packages: \$(ls /repo/$FEDORA_RELEASE/$ARCH/*.rpm | wc -l), signature check failures: \$bad\"
      [ \$bad = 0 ]
    "
    ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac
