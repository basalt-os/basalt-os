#!/usr/bin/env bash
# Build the Basalt OS installer ISO: the stock Fedora netinst ISO with the
# Basalt kickstart and the signed Basalt RPM repository embedded (mkksiso
# from lorax). The Fedora boot chain (shim, GRUB, kernel, all signed) is
# reused unchanged, so the ISO boots with Secure Boot on.
#
#   scripts/iso.sh fetch    download the Fedora netinst ISO and verify it
#                           (signed CHECKSUM file, Fedora's published keys)
#   scripts/iso.sh build    write $BUILD_DIR/iso/basalt-os-<version>-server-<arch>-netinst[-<site>].iso
#
# SITE_DIR (optional) holds site.conf and site.ks for unattended installs
# (accounts, encryption choice, end action); the ISO name then gets the
# SITE_NAME suffix. Without them the installer asks for accounts.
source "$(dirname "$0")/lib.sh"

: "${FEDORA_ISO_BASE:=https://dl.fedoraproject.org/pub/fedora/linux/releases/$FEDORA_RELEASE/Everything/$ARCH/iso}"
: "${FEDORA_ISO_NAME:=}"
: "${ISO_CACHE:=$BUILD_DIR/iso-cache}"
: "${SITE_DIR:=}"
: "${SITE_NAME:=site}"

fetch() {
  mkdir -p "$ISO_CACHE"
  cd "$ISO_CACHE" || die "cannot enter $ISO_CACHE"
  local sums
  sums="$(curl -fsSL "$FEDORA_ISO_BASE/" | grep -oE "Fedora-Everything-[0-9.]+-[0-9.]+-$ARCH-CHECKSUM" | head -1)"
  [[ -n "$sums" ]] || die "no CHECKSUM file at $FEDORA_ISO_BASE"
  curl -fsSLO "$FEDORA_ISO_BASE/$sums"
  # Fedora's signing keys, from the project's key page (fetched over HTTPS).
  curl -fsSL -o fedora.gpg https://fedoraproject.org/fedora.gpg
  gpgv --keyring ./fedora.gpg "$sums" 2>&1 | grep -q 'Good signature' || die "bad signature on $sums"
  local iso
  iso="$(sed -n 's/^SHA256 (\(Fedora-Everything-netinst-[^)]*\.iso\)) = .*/\1/p' "$sums")"
  [[ -f "$iso" ]] || curl -fSLO "$FEDORA_ISO_BASE/$iso"
  grep -F "SHA256 ($iso)" "$sums" | sha256sum -c --quiet - 2>/dev/null ||
    (sed -n "s/^SHA256 ($iso) = \(.*\)/\1  $iso/p" "$sums" | sha256sum -c --quiet -) || die "checksum mismatch for $iso"
  log "verified $ISO_CACHE/$iso"
  echo "$iso" >latest
}

build() {
  local iso volid out add
  iso="${FEDORA_ISO_NAME:-$(cat "$ISO_CACHE/latest" 2>/dev/null || true)}"
  [[ -n "$iso" && -f "$ISO_CACHE/$iso" ]] || die "no Fedora ISO in $ISO_CACHE (scripts/iso.sh fetch)"
  [[ -d "$REPO_DIR/$FEDORA_RELEASE/$ARCH/repodata" ]] || die "no repository at $REPO_DIR (make repo)"

  add="$BUILD_DIR/iso/add"
  sudo rm -rf "$add"
  mkdir -p "$add/basalt/repo/$FEDORA_RELEASE"
  cp -a "$REPO_DIR/$FEDORA_RELEASE/$ARCH" "$add/basalt/repo/$FEDORA_RELEASE/"
  cp "$REPO_DIR/RPM-GPG-KEY-basalt" "$add/basalt/"
  name="$(iso_name server netinst)"
  if [[ -n "$SITE_DIR" ]]; then
    [[ -f "$SITE_DIR/site.conf" || -f "$SITE_DIR/site.ks" ]] || die "SITE_DIR=$SITE_DIR has no site.conf or site.ks"
    cp "$SITE_DIR"/site.conf "$SITE_DIR"/site.ks "$add/basalt/" 2>/dev/null || true
    name+="-$SITE_NAME"
  fi
  volid="BASALT-${BASALT_VERSION//./-}-${ARCH}"
  out="$BUILD_DIR/iso/$name.iso"
  mkdir -p "$BUILD_DIR/iso"
  rm -f "$out"

  # Tool image with lorax (mkksiso) and xorriso, built once per Fedora release.
  local tools="localhost/basalt-iso-tools:$FEDORA_RELEASE"
  if ! $PODMAN image exists "$tools"; then
    log "building $tools"
    printf 'FROM %s\nRUN dnf -y install lorax xorriso && dnf clean all\n' "$FEDORA_IMAGE" |
      $PODMAN build --network=host -q -t "$tools" -f - >/dev/null
  fi
  log "building $out from $iso"
  # mkksiso needs root (it rebuilds the EFI boot image on a loop device).
  $PODMAN run --rm --privileged --network=host --security-opt label=disable \
    -v "$ISO_CACHE:/in:ro" -v "$BUILD_DIR/iso:/out" -v "$REPO_ROOT/kickstart:/ks:ro" \
    "$tools" bash -euc "
      mkksiso --ks /ks/basalt-server.ks \
        -a /out/add/basalt \
        -c 'console=tty0 console=ttyS0,115200n8 inst.text' \
        -r 'quiet rhgb rd.live.check' \
        -V '$volid' \
        -R 'Fedora $FEDORA_RELEASE' 'Basalt OS $BASALT_FULL_VERSION (Fedora $FEDORA_RELEASE base)' \
        -R 'set default=\"1\"' 'set default=\"0\"' \
        -R 'set timeout=60' 'set timeout=5' \
        /in/$iso /out/$name.iso
    "
  sudo chown "$(id -u):$(id -g)" "$out"
  sudo rm -rf "$add"
  (cd "$BUILD_DIR/iso" && sha256sum "$name.iso" >"$name.iso.sha256")
  log "ISO: $out ($(du -m "$out" | cut -f1) MiB)"
}

case "${1:-}" in
  fetch) fetch ;;
  build) build ;;
  *) sed -n '2,14p' "$0"; exit 2 ;;
esac
