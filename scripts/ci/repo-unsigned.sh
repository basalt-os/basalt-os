#!/usr/bin/env bash
# Build an UNSIGNED Basalt OS repository from the RPMs in $BUILD_DIR/rpms/<fedora>/
# (lab fixtures excluded): $REPO_DIR/<fedora>/<arch>/ and $REPO_DIR/<fedora>/source/
# with createrepo_c metadata, no package signatures and no repomd.xml.asc.
#
#   scripts/ci/repo-unsigned.sh
#
# This is what CI publishes as a build artifact. Signing is deliberately not
# done here: CI has no keys and never will. The release repository is
# signed on the release signer with the keys from the key ceremony
# (docs/key-ceremony.md, .github/workflows/release.yml). A system that has
# basalt-release from this build carries the placeholder key and dnf
# refuses these packages, which is the intended failure mode.
source "$(dirname "$0")/../lib.sh"

bin="$REPO_DIR/$FEDORA_RELEASE/$ARCH"
src="$REPO_DIR/$FEDORA_RELEASE/source"

mapfile -t rpms < <(find "$RPM_DIR" -maxdepth 1 -name '*.rpm' | sort)
[[ ${#rpms[@]} -gt 0 ]] || die "no RPMs in $RPM_DIR (scripts/build-rpms.sh)"

rm -rf "$bin" "$src"
mkdir -p "$bin" "$src"
for f in "${rpms[@]}"; do
  case "$f" in
    *.src.rpm) cp -f "$f" "$src/" ;;
    *) cp -f "$f" "$bin/" ;;
  esac
done

in_fedora -v "$REPO_DIR:/repo" -e OWNER="$(id -u):$(id -g)" "$FEDORA_IMAGE" bash -euc "
  dnf -q -y install createrepo_c >/dev/null 2>&1 || { echo 'dnf install failed' >&2; exit 1; }
  for d in /repo/$FEDORA_RELEASE/$ARCH /repo/$FEDORA_RELEASE/source; do
    createrepo_c -q \"\$d\"
  done
  # Nothing in this tree may look signed.
  for f in /repo/$FEDORA_RELEASE/$ARCH/*.rpm; do
    if rpm -qp --qf '%{SIGPGP:pgpsig}%{RSAHEADER:pgpsig}%{OPENPGP:pgpsig}\n' \"\$f\" 2>/dev/null | grep -q 'Key ID'; then
      echo \"unexpected signature on \$f\" >&2; exit 1
    fi
  done
  chown -R \"\$OWNER\" /repo"

# scripts/iso.sh puts RPM-GPG-KEY-basalt on the media; here it is the
# placeholder text from basalt-release, not a key.
cp "$REPO_ROOT/packages/basalt-release/RPM-GPG-KEY-basalt" "$REPO_DIR/RPM-GPG-KEY-basalt"
cat >"$REPO_DIR/UNSIGNED.txt" <<EOF
Basalt OS CI repository: UNSIGNED.

Packages and repository metadata in this tree carry no signatures, and
RPM-GPG-KEY-basalt is a placeholder, not a key. Built by CI for testing
only. Release repositories are signed on the release signer with the keys
from the key ceremony (docs/key-ceremony.md); CI holds no signing keys.

Fedora release: $FEDORA_RELEASE, architecture: $ARCH, Basalt OS version: $BASALT_VERSION
EOF
log "unsigned repository: $REPO_DIR/$FEDORA_RELEASE ($(find "$bin" -name '*.rpm' | wc -l) binary, $(find "$src" -name '*.rpm' | wc -l) source packages)"
