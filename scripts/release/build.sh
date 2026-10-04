#!/usr/bin/env bash
# Release build: every Basalt OS package, unsigned, for the release signer.
#
#   scripts/release/build.sh OUT_DIR
#
# Runs on a build host (never the signer). It builds from a clean checkout
# with no site configuration: lab overrides (development repository key,
# lab module certificates, lab repository URLs) are refused, so the
# packages carry the OpenBasalt release key and https://obpkg.org.
#
# Packages: scripts/build-rpms.sh (basalt-release, -logos, -snapshots,
# -security, -assistant, -agent, -resolver, -ledger, -installer),
# basalt-llm, and the data packages basalt-knowledge and basalt-vsm-planner
# (BASALT_ARTIFACTS_DIR or BASALT_ARTIFACTS_URL, see scripts/data-package.sh).
#
# OUT_DIR receives the binary and source RPMs, BUILD-INFO.txt and
# SHA256SUMS. scripts/release/sign.sh takes that directory as input.
ENV_FILE=/dev/null
export ENV_FILE
source "$(dirname "$0")/../lib.sh"

out="${1:?usage: $0 OUT_DIR}"
for var in BASALT_GPG_PUBKEY BASALT_DEFAULT_REPO_URL BASALT_DEFAULT_TOOLS_URL BASALT_DEFAULT_TESTING_URL \
  BASALT_MODULE_CA_CERT BASALT_MODULE_SIGNING_CERT; do
  [[ -z "${!var:-}" ]] || die "$var is set: a release build ships the OpenBasalt defaults, unset it"
done
[[ -z "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]] || die "the checkout has local changes; release builds come from a clean commit"
commit="$(git -C "$REPO_ROOT" rev-parse HEAD)" || die "not a git checkout"

rm -rf "$RPM_DIR"
"$REPO_ROOT/scripts/build-rpms.sh"
"$REPO_ROOT/packages/basalt-llm/build.sh"
"$REPO_ROOT/scripts/data-package.sh" basalt-knowledge
"$REPO_ROOT/scripts/data-package.sh" basalt-vsm-planner

log "rpmlint on the built packages"
in_fedora -v "$RPM_DIR:/rpms:ro" -v "$REPO_ROOT/scripts/ci:/ci:ro" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpmlint >/dev/null 2>&1 || { echo "dnf install failed" >&2; exit 1; }
  rpmlint -c /ci/rpmlint.toml /rpms/*.rpm' || die "rpmlint found errors in the built packages"

mkdir -p "$out"
find "$out" -maxdepth 1 -name '*.rpm' -delete
find "$RPM_DIR" -maxdepth 1 -name '*.rpm' -exec cp -p {} "$out/" \;

# basalt-release must carry the release key and the obpkg.org URLs.
rel="$(find "$out" -maxdepth 1 -name 'basalt-release-[0-9]*.noarch.rpm' | head -1)"
[[ -n "$rel" ]] || die "basalt-release not built"
chk="$(mktemp -d)"
trap 'rm -rf "$chk"' EXIT
(cd "$chk" && rpm2cpio "$rel" | cpio -idm --quiet ./etc/pki/rpm-gpg/RPM-GPG-KEY-basalt ./etc/dnf/vars/basalt_repo_url)
cmp -s "$chk/etc/pki/rpm-gpg/RPM-GPG-KEY-basalt" "$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt" ||
  die "basalt-release does not ship the OpenBasalt release key"
[[ "$(cat "$chk/etc/dnf/vars/basalt_repo_url")" == https://obpkg.org/basalt ]] || die "basalt-release does not point at https://obpkg.org/basalt"

cat >"$out/BUILD-INFO.txt" <<EOF
Basalt OS release build (UNSIGNED, input for scripts/release/sign.sh)
version:        $BASALT_VERSION
fedora release: $FEDORA_RELEASE
architecture:   $ARCH
commit:         $commit
build id:       $BASALT_BUILD_ID
built:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
(cd "$out" && find . -maxdepth 1 -type f -name '*.rpm' -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
log "release build in $out:"
find "$out" -maxdepth 1 -name '*.rpm' -printf '  %P  %s bytes\n' | sort >&2
