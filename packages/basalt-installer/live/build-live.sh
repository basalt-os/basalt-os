#!/usr/bin/env bash
# Build the Basalt OS live installer ISO.
#
#   packages/basalt-installer/live/build-live.sh
#
# 1. mkosi (live/mkosi.conf) installs a Fedora 44 system with the Basalt
#    packages and the installer from the signed repository in REPO_DIR
#    (served to mkosi on the container's loopback; packages and metadata are
#    signature checked) into a directory tree.
# 2. The tree becomes LiveOS/squashfs.img, an EROFS image whose SELinux
#    labels come from the tree's own policy (mkfs.erofs --file-contexts;
#    the build host's labels and policy play no part). dracut's
#    dmsquash-live (initrd made in mkosi.finalize.chroot) copies it to
#    memory (rd.live.ram) and boots it under a tmpfs overlay, like Fedora's
#    live media: the medium can be removed, and the live system runs
#    SELinux enforcing. The build checks a few labels in the image.
# 3. The ISO holds Fedora's signed shim, GRUB and kernel (taken from the
#    tree, unmodified) in an EFI system partition image, so it boots with
#    Secure Boot on; the Basalt repository travels on it too (the "media"
#    repository of the installer). UEFI only, hybrid (CD or USB stick).
#
# LIVE_PROFILE=desktop builds the live desktop instead (live/desktop/: the
# Basalt desktop edition with a live user, the graphical installer as an
# app of the session; it needs basalt-desktop and its packages in REPO_DIR,
# and runs from the medium rather than from memory).
#
# LIVE_SOURCE selects where the Basalt packages come from:
# - local (default): the signed repository in REPO_DIR (a lab or CI build;
#   for the desktop profile it also holds the basalt-testing packages);
# - obpkg: the published repositories on https://obpkg.org, mirrored
#   byte-identical and verified by scripts/release/mirror-published.sh into
#   $BUILD_DIR/obpkg/ (basalt, and basalt-testing for the desktop profile):
#   the image and its media repositories then hold only packages signed with
#   the OpenBasalt release key. The media carries basalt/repo (basalt) and
#   basalt/testing (basalt-testing), which the installer reads for plans
#   with repos.testing.
#
# Variables: REPO_DIR (signed repository, required with LIVE_SOURCE=local),
# LIVE_CMDLINE (extra kernel arguments, e.g. basalt.inst.repo=URL),
# LIVE_PLANS (directory of plan files copied to /basalt/plans on the media;
# the desktop profile defaults to live/desktop/plans), LIVE_NAME (ISO name
# suffix for test variants), LIVE_TIMEOUT (GRUB menu seconds, default 5).
# Version and stage come from scripts/lib.sh (VERSION, BASALT_STAGE,
# BASALT_BUILD; docs/versioning.md).
# Output: $BUILD_DIR/live/basalt-os-<version>-<edition>-<arch>[-<name>].iso
# (edition server for the installer image, desktop for the live desktop)
# and $BUILD_DIR/live/SHA256SUMS over the images of that version.
source "$(dirname "$0")/../../../scripts/lib.sh"
live="$REPO_ROOT/packages/basalt-installer/live"
: "${LIVE_CMDLINE:=}"
: "${LIVE_PLANS:=}"
: "${LIVE_NAME:=}"
: "${LIVE_TIMEOUT:=5}"
: "${LIVE_PROFILE:=}"
: "${LIVE_SOURCE:=local}"
label=BASALT-INST
case "$LIVE_PROFILE" in
  "") edition=server ;;
  desktop) edition=desktop ;;
  *) die "LIVE_PROFILE must be empty or desktop" ;;
esac
# basalt-testing, as its own repository (obpkg); empty when REPO_DIR holds it.
testing_dir=""
case "$LIVE_SOURCE" in
  local) ;;
  obpkg)
    REPO_DIR="$BUILD_DIR/obpkg/basalt"
    OB_REPO=basalt "$REPO_ROOT/scripts/release/mirror-published.sh" "$REPO_DIR"
    if [[ "$LIVE_PROFILE" == desktop ]]; then
      testing_dir="$BUILD_DIR/obpkg/basalt-testing"
      OB_REPO=basalt-testing "$REPO_ROOT/scripts/release/mirror-published.sh" "$testing_dir"
    fi ;;
  *) die "LIVE_SOURCE must be local or obpkg" ;;
esac
if [[ "$LIVE_PROFILE" == desktop ]]; then
  compgen -G "$REPO_DIR/$FEDORA_RELEASE/$ARCH/basalt-desktop-*.rpm" >/dev/null ||
    { [[ -n "$testing_dir" ]] && compgen -G "$testing_dir/$FEDORA_RELEASE/$ARCH/basalt-desktop-*.rpm" >/dev/null; } ||
    die "basalt-desktop is not in $REPO_DIR${testing_dir:+ or $testing_dir} (the desktop profile needs it)"
  [[ -n "$LIVE_PLANS" ]] || LIVE_PLANS="$live/desktop/plans"
fi
[[ -d "$REPO_DIR/$FEDORA_RELEASE/$ARCH/repodata" ]] || die "no repository at $REPO_DIR (make repo)"
[[ -f "$REPO_DIR/RPM-GPG-KEY-basalt" ]] || die "no RPM-GPG-KEY-basalt in $REPO_DIR"
ls "$REPO_DIR/$FEDORA_RELEASE/$ARCH"/basalt-installer-gui-*.rpm >/dev/null 2>&1 ||
  die "basalt-installer is not in $REPO_DIR (packages/basalt-installer/build.sh, then make repo)"

out="$BUILD_DIR/live"
name="$(iso_name "$edition" "$LIVE_NAME")"
mkdir -p "$out" "$BUILD_DIR/cache/mkosi"
plans=/dev/null
[[ -n "$LIVE_PLANS" ]] && plans="$LIVE_PLANS"

tools="localhost/basalt-live-tools:$FEDORA_RELEASE-2"
if ! $PODMAN image exists "$tools"; then
  log "building $tools"
  printf 'FROM %s\nRUN dnf -y install mkosi xorriso dosfstools mtools erofs-utils attr python3 dnf5 rpm systemd-container util-linux && dnf clean all\n' "$FEDORA_IMAGE" |
    $PODMAN build --network=host -q -t "$tools" -f - >/dev/null
fi

log "building $name.iso"
$PODMAN run --rm --privileged --network=host --security-opt label=disable \
  -v "$live:/live:ro" -v "$REPO_DIR:/repo:ro" -v "$out:/out" -v "$BUILD_DIR/cache/mkosi:/cache" \
  -v "$plans:/plans:ro" -v "${testing_dir:-/dev/null}:/testing:ro" \
  -e NAME="$name" -e LABEL="$label" -e VERSION="$BASALT_FULL_VERSION" -e RELEASE="$FEDORA_RELEASE" \
  -e CMDLINE_EXTRA="$LIVE_CMDLINE" -e TIMEOUT="$LIVE_TIMEOUT" -e PROFILE="$LIVE_PROFILE" \
  "$tools" bash -euo pipefail -c '
    # A free port: other builds on the same host share the network namespace.
    port=$(python3 -c "import socket; s=socket.socket(); s.bind((\"127.0.0.1\", 0)); print(s.getsockname()[1])")
    # /basalt is the Basalt repository, /testing basalt-testing when it is a
    # repository of its own (LIVE_SOURCE=obpkg).
    mkdir -p /tmp/www && ln -s /repo /tmp/www/basalt
    [ -d /testing ] && ln -s /testing /tmp/www/testing
    python3 -m http.server "$port" --bind 127.0.0.1 --directory /tmp/www >/tmp/http.log 2>&1 &
    trap "kill $! 2>/dev/null || true" EXIT

    # Repositories for mkosi: Fedora (from this container) and Basalt.
    sb=/tmp/sandbox
    mkdir -p "$sb/etc/yum.repos.d" "$sb/etc/pki/rpm-gpg"
    cp /etc/yum.repos.d/fedora.repo /etc/yum.repos.d/fedora-updates.repo "$sb/etc/yum.repos.d/"
    cp /etc/pki/rpm-gpg/RPM-GPG-KEY-fedora-* "$sb/etc/pki/rpm-gpg/"
    cp /repo/RPM-GPG-KEY-basalt "$sb/etc/pki/rpm-gpg/RPM-GPG-KEY-basalt"
    printf "[basalt]\nname=Basalt OS\nbaseurl=http://127.0.0.1:%s/basalt/\$releasever/\$basearch/\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-basalt\n" "$port" \
      >"$sb/etc/yum.repos.d/basalt.repo"
    if [ -d /testing ]; then
      # Same key: one packages subkey signs every Basalt repository.
      cmp -s /testing/RPM-GPG-KEY-basalt /repo/RPM-GPG-KEY-basalt || { echo "basalt-testing has another key" >&2; exit 1; }
      printf "[basalt-testing]\nname=Basalt OS testing\nbaseurl=http://127.0.0.1:%s/testing/\$releasever/\$basearch/\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-basalt\n" "$port" \
        >>"$sb/etc/yum.repos.d/basalt.repo"
    fi

    # The Basalt repository changes between builds (re-signed packages):
    # never reuse its cached metadata or packages.
    find /cache -path "*libdnf5/basalt*" -prune -exec rm -rf {} + 2>/dev/null || true
    rm -rf /tmp/build && mkdir -p /tmp/build && cp -a /live/. /tmp/build/ && cd /tmp/build
    rm -rf /tmp/build/desktop
    if [ "$PROFILE" = desktop ]; then
      # The desktop profile: more packages (mkosi.conf.d) and files on top
      # of the installer image (finalize-desktop runs from mkosi.finalize.chroot).
      mkdir -p mkosi.conf.d && cp -a /live/desktop/mkosi.conf.d/. mkosi.conf.d/
      cp -a /live/desktop/mkosi.extra/. mkosi.extra/
    fi
    mkosi --sandbox-tree="$sb" --output-directory=/tmp/out --cache-directory=/cache --workspace-directory=/tmp \
      --force build >/tmp/mkosi.log 2>&1 || { tail -60 /tmp/mkosi.log; cp /tmp/mkosi.log "/out/$NAME.mkosi.log"; exit 1; }
    cp /tmp/mkosi.log "/out/$NAME.mkosi.log"
    tree=/tmp/out/basalt-live
    cp /tmp/out/basalt-live.manifest /out/$NAME.manifest.json 2>/dev/null || true

    kver="$(ls "$tree/usr/lib/modules" | sort -V | tail -1)"
    iso=/tmp/iso
    rm -rf "$iso" && mkdir -p "$iso/images" "$iso/LiveOS" "$iso/basalt/repo"
    cp "$tree/usr/lib/modules/$kver/vmlinuz" "$iso/images/vmlinuz"
    [ -s "$tree/boot/initrd-live.img" ] || { echo "no live initrd (mkosi.finalize.chroot)" >&2; exit 1; }
    cp "$tree/boot/initrd-live.img" "$iso/images/initrd.img"

    # Fedora signed boot chain, unmodified: shim as the removable-media
    # loader, GRUB and MokManager next to it, our menu.
    efisrc="$tree/boot/efi/EFI"
    [ -f "$efisrc/BOOT/BOOTX64.EFI" ] && [ -f "$efisrc/fedora/grubx64.efi" ] || { echo "signed shim/GRUB not found in the tree" >&2; exit 1; }
    # enforcing=1: a live system that cannot load the policy does not boot.
    # The desktop runs from the medium (a desktop image is too large to
    # copy to memory on modest machines); the installer image from memory.
    live_args="rd.live.ram=1" menu=/live/grub.cfg.in
    [ "$PROFILE" = desktop ] && live_args="" menu=/live/desktop/grub.cfg.in
    cmdline="root=live:CDLABEL=$LABEL quiet rd.live.image $live_args rd.live.overlay.overlayfs=1 enforcing=1 systemd.firstboot=off systemd.getty_auto=0 console=tty0 console=ttyS0,115200n8 $CMDLINE_EXTRA"
    cmdline="$(echo "$cmdline" | tr -s " ")"
    # The GRUB theme from the tree (basalt-grub2-theme) goes on the ISO, and
    # its block into the menu: GRUB reads it from the ISO file system (root).
    # basalt-grub2-theme 0.2.0 or later: its basalt-theme.cfg is a template
    # with @THEME_DIR@ and draws its text with the font inside Fedora'"'"'s
    # signed GRUB. An older one (0.1.0 looked for the theme next to GRUB on
    # the EFI image and loaded font files, which Secure Boot refuses) left
    # the live menu as plain text, so the build stops instead.
    themes="$tree/usr/share/basalt/grub2"
    [ -f "$themes/basalt-theme.cfg" ] && [ -f "$themes/themes/basalt/theme.txt" ] ||
      { echo "the GRUB theme is missing from the image (basalt-grub2-theme)" >&2; exit 1; }
    grep -q "@THEME_DIR@" "$themes/basalt-theme.cfg" ||
      { echo "basalt-grub2-theme in the repository is older than 0.2.0: the live menu would have no theme" >&2; exit 1; }
    mkdir -p "$iso/boot/grub2/themes"
    cp -r "$themes/themes/basalt" "$iso/boot/grub2/themes/"
    sed -e "s|@THEME_DIR@|(\$root)/boot/grub2/themes/basalt|" "$themes/basalt-theme.cfg" | grep -v "^#" >/tmp/theme.cfg
    sed -e "s|@LABEL@|$LABEL|" -e "s|@VERSION@|$VERSION|g" -e "s|@CMDLINE@|$cmdline|g" -e "s|@TIMEOUT@|$TIMEOUT|" \
      -e "/^@THEME@\$/{r /tmp/theme.cfg" -e "d}" "$menu" >/tmp/grub.cfg
    img="$iso/images/efiboot.img"
    mkfs.vfat -C -n BASALTEFI "$img" 8192 >/dev/null
    mmd -i "$img" ::/EFI ::/EFI/BOOT
    mcopy -i "$img" "$efisrc/BOOT/BOOTX64.EFI" ::/EFI/BOOT/BOOTX64.EFI
    mcopy -i "$img" "$efisrc/fedora/grubx64.efi" ::/EFI/BOOT/grubx64.efi
    [ -f "$efisrc/fedora/mmx64.efi" ] && mcopy -i "$img" "$efisrc/fedora/mmx64.efi" ::/EFI/BOOT/mmx64.efi
    mcopy -i "$img" /tmp/grub.cfg ::/EFI/BOOT/grub.cfg
    mkdir -p "$iso/EFI/BOOT" && cp /tmp/grub.cfg "$iso/EFI/BOOT/grub.cfg"

    # The live system: everything but what only the ISO needs, labeled
    # with the policy of the image itself (file_contexts plus the live-only
    # file_contexts.local: the installer runs in install_t).
    rm -rf "$tree/boot/efi"/* "$tree/boot/initrd-live.img" "$tree/usr/lib/modules/$kver/vmlinuz" \
      "$tree/var/cache/"* "$tree/var/lib/dnf" 2>/dev/null || true
    fc="$tree/etc/selinux/targeted/contexts/files"
    [ -f "$tree/etc/selinux/targeted/policy/policy.$(ls "$tree/etc/selinux/targeted/policy" | sed -n "s/^policy\.//p" | sort -n | tail -1)" ] ||
      { echo "no compiled SELinux policy in the tree" >&2; exit 1; }
    # file_contexts.homedirs: the home of the live desktop user.
    cat "$fc/file_contexts" "$fc/file_contexts.local" $([ -f "$fc/file_contexts.homedirs" ] && echo "$fc/file_contexts.homedirs") >/tmp/file_contexts
    mkfs.erofs --quiet -zlzma -C1048576 --all-root --file-contexts=/tmp/file_contexts \
      "$iso/LiveOS/squashfs.img" "$tree" >/tmp/erofs.log 2>&1 || { cat /tmp/erofs.log >&2; exit 1; }

    # The labels the live system depends on, read back from the image.
    # Read through the build host kernel, so only types the host policy
    # knows (Fedora ones) read back as written; a type of a Basalt module
    # shows as unlabeled_t here and is checked on the booted image instead.
    mkdir -p /tmp/check && mount -t erofs -o ro,loop "$iso/LiveOS/squashfs.img" /tmp/check
    bad=0
    for want in /:root_t /usr/lib/systemd/systemd:init_exec_t /usr/bin/bash:shell_exec_t \
                /etc/shadow:shadow_t /usr/bin/basalt-installer:install_exec_t /usr/bin/cage:bin_t \
                $([ "$PROFILE" = desktop ] && echo /home/basalt:user_home_dir_t /usr/bin/sway:bin_t); do
      f="${want%%:*}" t="${want##*:}"
      got="$(getfattr --absolute-names --only-values -h -n security.selinux "/tmp/check$f" 2>&1 | tr -d "\\0")"
      case "$got" in *":$t:"*) echo "label $f: $got" ;; *) echo "label $f: $got (expected $t)" >&2; bad=1 ;; esac
    done
    umount /tmp/check
    [ "$bad" = 0 ] || { echo "SELinux labels missing in the live image" >&2; exit 1; }

    # The Basalt repository (the installer reads it as "media") and plans.
    cp -a "/repo/$RELEASE" "$iso/basalt/repo/"
    cp /repo/RPM-GPG-KEY-basalt "$iso/basalt/"
    if [ -d /testing ]; then mkdir -p "$iso/basalt/testing" && cp -a "/testing/$RELEASE" "$iso/basalt/testing/"; fi
    if [ -d /plans ] && [ -n "$(ls -A /plans 2>/dev/null)" ]; then mkdir -p "$iso/basalt/plans" && cp /plans/* "$iso/basalt/plans/"; fi

    xorriso -as mkisofs -quiet -iso-level 3 -full-iso9660-filenames -joliet -joliet-long -rational-rock \
      -volid "$LABEL" -partition_offset 16 \
      -append_partition 2 C12A7328-F81F-11D2-BA4B-00A0C93EC93B "$img" -appended_part_as_gpt \
      -e --interval:appended_partition_2:all:: -no-emul-boot \
      -output "/out/$NAME.iso" "$iso"
    du -m "$iso/LiveOS/squashfs.img" "$iso/images/initrd.img" "$iso/images/vmlinuz" | sed "s|$iso/||"
  '
sudo chown "$(id -u):$(id -g)" "$out/$name".* 2>/dev/null || true
# SHA256SUMS over every image of this version in the output directory
# (docs/versioning.md; the release signer signs that file).
(cd "$out" && find . -maxdepth 1 -name "basalt-os-$BASALT_FULL_VERSION-*.iso" -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
log "ISO: $out/$name.iso ($(du -m "$out/$name.iso" | cut -f1) MiB), listed in $out/SHA256SUMS"
