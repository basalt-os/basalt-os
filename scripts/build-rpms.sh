#!/usr/bin/env bash
# Build the Basalt OS RPMs in a Fedora container.
#
#   scripts/build-rpms.sh            basalt-release, basalt-nonfree-release, basalt-logos, basalt-snapshots,
#                                    basalt-security, basalt-prompt, basalt-models, basalt-assistant (Go, own script:
#                                    packages/basalt-assistant/build.sh)
#   scripts/build-rpms.sh --lab      also the lab canary package (versions 1, 2, 3)
#
# Output: $BUILD_DIR/rpms/<fedora>/ (binary and source RPMs); lab fixtures in
# $BUILD_DIR/rpms/<fedora>/lab/. FEDORA_RELEASE selects the base (default 44).
# BASALT_GPG_PUBKEY, when set, is the repository key shipped in basalt-release
# (otherwise basalt-release ships the OpenBasalt release key from packages/basalt-release).
# BASALT_DEFAULT_REPO_URL, BASALT_DEFAULT_TOOLS_URL and BASALT_DEFAULT_TESTING_URL,
# when set, replace the default repository URLs basalt-release ships
# (https://obpkg.org/basalt, /basalt-tools, /basalt-testing), for a lab or a mirror;
# BASALT_DEFAULT_NONFREE_URL likewise for basalt-nonfree-release
# (https://obpkg.org/basalt-nonfree).
# BASALT_MODULE_CA_CERT and BASALT_MODULE_SIGNING_CERT (DER or PEM), when set,
# replace the kernel module CA (the MOK) and signing certificate shipped in
# basalt-security (a lab override; otherwise the OpenBasalt certificates in
# packages/basalt-security are shipped).
source "$(dirname "$0")/lib.sh"

lab=0
[[ "${1:-}" == --lab ]] && lab=1

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$work/lab"

# Sources: every file next to each spec (subdirectories such as tests/ stay out).
for pkg in basalt-release basalt-nonfree-release basalt-snapshots basalt-security basalt-prompt basalt-models; do
  find "$REPO_ROOT/packages/$pkg" -maxdepth 1 -type f -exec cp -p {} "$work/SOURCES/" \;
  mv "$work/SOURCES/$pkg.spec" "$work/SPECS/"
done
if [[ -n "$BASALT_GPG_PUBKEY" ]]; then
  [[ -f "$BASALT_GPG_PUBKEY" ]] || die "BASALT_GPG_PUBKEY=$BASALT_GPG_PUBKEY not found"
  grep -q 'BEGIN PGP PUBLIC KEY BLOCK' "$BASALT_GPG_PUBKEY" || die "$BASALT_GPG_PUBKEY is not an armored public key"
  cp "$BASALT_GPG_PUBKEY" "$work/SOURCES/RPM-GPG-KEY-basalt"
  log "basalt-release ships the key from $BASALT_GPG_PUBKEY"
else
  log "basalt-release ships the OpenBasalt release key (set BASALT_GPG_PUBKEY for a repository signed with another key)"
fi

# Default repository URLs in basalt-release (/etc/dnf/vars): https://obpkg.org
# unless a lab or a mirror build overrides them.
for pair in "BASALT_DEFAULT_REPO_URL:basalt_repo_url" "BASALT_DEFAULT_TOOLS_URL:basalt_tools_url" \
  "BASALT_DEFAULT_TESTING_URL:basalt_testing_url" "BASALT_DEFAULT_NONFREE_URL:basalt_nonfree_url"; do
  var="${pair%%:*}" dst="${pair#*:}"
  url="${!var:-}"
  [[ -n "$url" ]] || continue
  [[ "$url" =~ ^(https?|file)://[^[:space:]]+$ ]] || die "$var=$url is not an http(s) or file URL"
  printf '%s\n' "${url%/}" >"$work/SOURCES/$dst"
  log "release package: $dst = ${url%/} (override of the obpkg.org default)"
done

# Module certificates for basalt-security, converted to DER.
for pair in "BASALT_MODULE_CA_CERT:basalt-module-ca.der" "BASALT_MODULE_SIGNING_CERT:basalt-module-signing.der"; do
  var="${pair%%:*}" dst="${pair#*:}"
  src="${!var:-}"
  [[ -n "$src" ]] || { log "basalt-security ships the OpenBasalt $dst"; continue; }
  [[ -f "$src" ]] || die "$var=$src not found"
  if grep -q 'BEGIN CERTIFICATE' "$src"; then
    openssl x509 -in "$src" -outform DER -out "$work/SOURCES/$dst"
  else
    openssl x509 -inform DER -in "$src" -noout || die "$src is not a certificate"
    cp "$src" "$work/SOURCES/$dst"
  fi
  log "basalt-security ships $dst from $src (override of the OpenBasalt certificate)"
done

# basalt-logos: the artwork tree travels as a tarball.
logos="$REPO_ROOT/packages/basalt-logos"
lver="$(awk '/^Version:/ {print $2}' "$logos/basalt-logos.spec")"
tar -C "$logos" --owner=0 --group=0 --sort=name -czf "$work/SOURCES/basalt-logos-$lver.tar.gz" tree
cp -p "$logos/basalt-theme.cfg" "$logos/06_basalt_theme" "$work/SOURCES/"
cp -p "$logos/basalt-logos.spec" "$work/SPECS/"

[[ $lab == 1 ]] && cp -p "$REPO_ROOT/packages/lab/basalt-canary/basalt-canary.spec" "$work/lab/"

log "building in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" -e LAB="$lab" -e BASALT_VERSION="$BASALT_VERSION" \
  -e BASALT_STAGE="$BASALT_STAGE" -e BASALT_BUILD="$BASALT_BUILD" -e BASALT_BUILD_ID="$BASALT_BUILD_ID" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build systemd-rpm-macros >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  for spec in /rpmbuild/SPECS/*.spec; do
    rpmbuild --define "_topdir /rpmbuild" --define "basalt_version $BASALT_VERSION" \
      --define "basalt_stage $BASALT_STAGE" --define "basalt_build $BASALT_BUILD" \
      --define "basalt_build_id $BASALT_BUILD_ID" -ba "$spec" >/rpmbuild/$(basename "$spec").log 2>&1 ||
      { tail -40 /rpmbuild/$(basename "$spec").log; exit 1; }
  done
  if [ "$LAB" = 1 ]; then
    for v in 1 2 3; do
      rpmbuild --define "_topdir /rpmbuild/lab/build" --define "canary_version $v" \
        -bb /rpmbuild/lab/basalt-canary.spec >/rpmbuild/lab/canary-$v.log 2>&1 ||
        { tail -40 /rpmbuild/lab/canary-$v.log; exit 1; }
    done
  fi
'

mkdir -p "$RPM_DIR"
sudo find "$work/RPMS" "$work/SRPMS" -name "*.rpm" -exec cp {} "$RPM_DIR/" \;
if [[ $lab == 1 ]]; then
  mkdir -p "$RPM_DIR/lab"
  sudo find "$work/lab/build/RPMS" -name "*.rpm" -exec cp {} "$RPM_DIR/lab/" \;
fi
sudo chown -R "$(id -u):$(id -g)" "$RPM_DIR"
find "$RPM_DIR" -name "*.rpm" -printf "%P  %s bytes\n" | sort

# basalt-assistant, basalt-agent, basalt-resolver and basalt-ledger (Go
# and SELinux modules) build in their own container runs.
"$REPO_ROOT/packages/basalt-assistant/build.sh"
"$REPO_ROOT/packages/basalt-agent/build.sh"
"$REPO_ROOT/packages/basalt-resolver/build.sh"
"$REPO_ROOT/packages/basalt-ledger/build.sh"
# basalt-installer (Go with the upstream toolchain named in its go.mod).
"$REPO_ROOT/packages/basalt-installer/build.sh"
