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
# Variables: REPO_DIR (signed repository, required), LIVE_CMDLINE (extra
# kernel arguments, e.g. basalt.inst.repo=URL), LIVE_PLANS (directory of
# plan files copied to /basalt/plans on the media), LIVE_NAME (ISO name
# suffix), LIVE_TIMEOUT (GRUB menu seconds, default 5).
# Output: $BUILD_DIR/live/basalt-os-<version>-<arch>-live[-<name>].iso
source "$(dirname "$0")/../../../scripts/lib.sh"
live="$REPO_ROOT/packages/basalt-installer/live"
: "${LIVE_CMDLINE:=}"
: "${LIVE_PLANS:=}"
: "${LIVE_NAME:=}"
: "${LIVE_TIMEOUT:=5}"
label=BASALT-INST
[[ -d "$REPO_DIR/$FEDORA_RELEASE/$ARCH/repodata" ]] || die "no repository at $REPO_DIR (make repo)"
[[ -f "$REPO_DIR/RPM-GPG-KEY-basalt" ]] || die "no RPM-GPG-KEY-basalt in $REPO_DIR"
ls "$REPO_DIR/$FEDORA_RELEASE/$ARCH"/basalt-installer-gui-*.rpm >/dev/null 2>&1 ||
  die "basalt-installer is not in $REPO_DIR (packages/basalt-installer/build.sh, then make repo)"

out="$BUILD_DIR/live"
name="basalt-os-$BASALT_VERSION-$ARCH-live${LIVE_NAME:+-$LIVE_NAME}"
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
  -v "$plans:/plans:ro" \
  -e NAME="$name" -e LABEL="$label" -e VERSION="$BASALT_VERSION" -e RELEASE="$FEDORA_RELEASE" \
  -e CMDLINE_EXTRA="$LIVE_CMDLINE" -e TIMEOUT="$LIVE_TIMEOUT" \
  "$tools" bash -euo pipefail -c '
    port=8197
    python3 -m http.server "$port" --bind 127.0.0.1 --directory /repo >/tmp/http.log 2>&1 &
    trap "kill $! 2>/dev/null || true" EXIT

    # Repositories for mkosi: Fedora (from this container) and Basalt.
    sb=/tmp/sandbox
    mkdir -p "$sb/etc/yum.repos.d" "$sb/etc/pki/rpm-gpg"
    cp /etc/yum.repos.d/fedora.repo /etc/yum.repos.d/fedora-updates.repo "$sb/etc/yum.repos.d/"
    cp /etc/pki/rpm-gpg/RPM-GPG-KEY-fedora-* "$sb/etc/pki/rpm-gpg/"
    cp /repo/RPM-GPG-KEY-basalt "$sb/etc/pki/rpm-gpg/RPM-GPG-KEY-basalt"
    printf "[basalt]\nname=Basalt OS\nbaseurl=http://127.0.0.1:%s/\$releasever/\$basearch/\ngpgcheck=1\nrepo_gpgcheck=1\ngpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-basalt\n" "$port" \
      >"$sb/etc/yum.repos.d/basalt.repo"

    # The Basalt repository changes between builds (re-signed packages):
    # never reuse its cached metadata or packages.
    find /cache -path "*libdnf5/basalt-*" -prune -exec rm -rf {} + 2>/dev/null || true
    rm -rf /tmp/build && mkdir -p /tmp/build && cp -a /live/. /tmp/build/ && cd /tmp/build
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
    cmdline="root=live:CDLABEL=$LABEL rd.live.image rd.live.ram=1 rd.live.overlay.overlayfs=1 enforcing=1 systemd.firstboot=off systemd.getty_auto=0 console=tty0 console=ttyS0,115200n8 $CMDLINE_EXTRA"
    sed -e "s|@LABEL@|$LABEL|" -e "s|@VERSION@|$VERSION|g" -e "s|@CMDLINE@|$cmdline|g" -e "s|@TIMEOUT@|$TIMEOUT|" /live/grub.cfg.in >/tmp/grub.cfg
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
    cat "$fc/file_contexts" "$fc/file_contexts.local" >/tmp/file_contexts
    mkfs.erofs --quiet -zlzma -C1048576 --all-root --file-contexts=/tmp/file_contexts \
      "$iso/LiveOS/squashfs.img" "$tree" >/tmp/erofs.log 2>&1 || { cat /tmp/erofs.log >&2; exit 1; }

    # The labels the live system depends on, read back from the image.
    mkdir -p /tmp/check && mount -t erofs -o ro,loop "$iso/LiveOS/squashfs.img" /tmp/check
    bad=0
    for want in /:root_t /usr/lib/systemd/systemd:init_exec_t /usr/bin/bash:shell_exec_t \
                /etc/shadow:shadow_t /usr/bin/basalt-installer:install_exec_t /usr/bin/cage:bin_t; do
      f="${want%%:*}" t="${want##*:}"
      got="$(getfattr --absolute-names --only-values -h -n security.selinux "/tmp/check$f" 2>&1 | tr -d "\\0")"
      case "$got" in *":$t:"*) echo "label $f: $got" ;; *) echo "label $f: $got (expected $t)" >&2; bad=1 ;; esac
    done
    umount /tmp/check
    [ "$bad" = 0 ] || { echo "SELinux labels missing in the live image" >&2; exit 1; }

    # The Basalt repository (the installer reads it as "media") and plans.
    cp -a "/repo/$RELEASE" "$iso/basalt/repo/"
    cp /repo/RPM-GPG-KEY-basalt "$iso/basalt/"
    if [ -d /plans ] && [ -n "$(ls -A /plans 2>/dev/null)" ]; then mkdir -p "$iso/basalt/plans" && cp /plans/* "$iso/basalt/plans/"; fi

    xorriso -as mkisofs -quiet -iso-level 3 -full-iso9660-filenames -joliet -joliet-long -rational-rock \
      -volid "$LABEL" -partition_offset 16 \
      -append_partition 2 C12A7328-F81F-11D2-BA4B-00A0C93EC93B "$img" -appended_part_as_gpt \
      -e --interval:appended_partition_2:all:: -no-emul-boot \
      -output "/out/$NAME.iso" "$iso"
    du -m "$iso/LiveOS/squashfs.img" "$iso/images/initrd.img" "$iso/images/vmlinuz" | sed "s|$iso/||"
  '
sudo chown "$(id -u):$(id -g)" "$out/$name".* 2>/dev/null || true
(cd "$out" && sha256sum "$name.iso" >"$name.iso.sha256")
log "ISO: $out/$name.iso ($(du -m "$out/$name.iso" | cut -f1) MiB)"
