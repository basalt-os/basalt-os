#!/usr/bin/env bash
# Release build of the basalt-nonfree repository: the NVIDIA driver for
# Turing and newer GPUs (docs/nvidia.md). Runs on a build host, which never
# holds a key; module signing happens on the release signer in between.
#
#   scripts/release/build-nonfree.sh modules MODULES_DIR
#       1. builds the open GPU kernel modules for every kernel of the Fedora
#          release (unsigned), in a build image made for this version, with
#          no network; MODULES_DIR gets <kernel>/*.ko, KERNELS, LATEST,
#          BUILD-IMAGE and SHA256SUMS for the signer
#   scripts/release/sign-modules.sh --op|--key-file|--test-key MODULES_DIR SIG_DIR
#       2. (release signer) signs them: only signatures come back
#   scripts/release/build-nonfree.sh packages SIG_DIR OUT_DIR
#       3. builds every package: kmod-nvidia-open-<kernel> for each kernel
#          (the same build again, in the same image: each module must be
#          byte-identical to the one signed, then the signature is
#          appended), kmod-nvidia-open (follows the newest stable kernel and
#          holds newer ones), nvidia-driver and its subpackages from the
#          official .run (byte-identical files), nvidia-modprobe and
#          nvidia-persistenced from source, basalt-nvidia (with the module
#          signing certificate); then the packaging checks
#          (packages/nvidia/check-identical.sh) and rpmlint. OUT_DIR gets the
#          binary and source RPMs, BUILD-INFO.txt and SHA256SUMS: the input of
#          OB_REPO=basalt-nonfree scripts/release/sign.sh.
#
# OB_REPO names the repository the build is for: basalt-nonfree (default) or
# basalt-nonfree-testing, where every new build goes first (docs/publishing.md).
# The packages are the same; BUILD-INFO.txt records the repository, and the
# same OB_REPO goes to merge-published.sh, sign.sh, client-test.sh.
#
# Kernels: every kernel-core of the release's fedora and updates
# repositories (and updates-testing with NONFREE_TESTING_KERNELS=1, to have
# the module ready before a kernel reaches updates), or NONFREE_KERNELS
# ("K1 K2", uname -r form). LATEST, the kernel kmod-nvidia-open follows, is
# the newest of fedora and updates (NONFREE_LATEST overrides).
#
# Sources are pinned in packages/nvidia/source.conf (SHA-256 of the .run,
# of its LICENSE, of every source archive) and cached in
# $BUILD_DIR/nonfree/cache. NONFREE_RUN=FILE uses a local copy of the .run
# (still checked).
#
# A release build needs a clean checkout and signatures made with a
# certificate the OpenBasalt kernel module CA issued
# (packages/basalt-security/basalt-module-ca.der). NONFREE_LAB=1 allows a
# lab build: local changes, signatures from --test-key or from a lab key,
# with NONFREE_MODULE_CA naming the lab CA certificate. A lab build says so
# in BUILD-INFO.txt and must never be published.
source "$(dirname "$0")/../lib.sh"

nv="$REPO_ROOT/packages/nvidia"
# shellcheck source=packages/nvidia/source.conf
source "$nv/source.conf"
: "${NONFREE_LAB:=0}"
: "${NONFREE_TESTING_KERNELS:=0}"
: "${OB_REPO:=basalt-nonfree}"
case "$OB_REPO" in basalt-nonfree | basalt-nonfree-testing) ;; *) die "OB_REPO must be basalt-nonfree or basalt-nonfree-testing" ;; esac
cache="$BUILD_DIR/nonfree/cache"
KBUILD_IMAGE="localhost/basalt-kmod-build:$FEDORA_RELEASE-$NVIDIA_VERSION"

usage() { sed -n '2,29p' "$0" >&2; exit 2; }
stage="${1:-}"; [[ $# -gt 0 ]] && shift
case "$stage" in modules | packages) ;; *) usage ;; esac

if [[ "$NONFREE_LAB" != 1 ]]; then
  [[ -z "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]] || die "the checkout has local changes; release builds come from a clean commit (NONFREE_LAB=1 for a lab build)"
  [[ -z "${NONFREE_MODULE_CA:-}" ]] || die "NONFREE_MODULE_CA is a lab override (NONFREE_LAB=1)"
fi
commit="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"

# Specs and source.conf must agree on the version.
for spec in nvidia-driver.spec nvidia-open-kmod.spec kmod-nvidia-open.spec nvidia-modprobe.spec nvidia-persistenced.spec; do
  [[ "$(awk '/^Version:/ {print $2; exit}' "$nv/$spec")" == "$NVIDIA_VERSION" ]] || die "$spec is not version $NVIDIA_VERSION"
done

# fetch NAME URL SHA256: a pinned source in the cache, checked.
fetch() {
  local name=$1 url=$2 sum=$3 f="$cache/$1"
  mkdir -p "$cache"
  if [[ ! -f "$f" ]] || ! echo "$sum  $f" | sha256sum -c --quiet - 2>/dev/null; then
    log "downloading $name"
    curl -fsSL --proto '=https' -o "$f.part" "$url" || die "download of $url failed"
    mv -f "$f.part" "$f"
  fi
  echo "$sum  $f" | sha256sum -c --quiet - || die "$name: checksum mismatch (expected $sum)"
  echo "$f"
}

kernels_from_repos() {
  local repos="--enablerepo=fedora --enablerepo=updates"
  [[ "$1" == all && "$NONFREE_TESTING_KERNELS" == 1 ]] && repos+=" --enablerepo=updates-testing"
  in_fedora "$FEDORA_IMAGE" bash -euc "dnf -q repoquery --disablerepo='*' $repos --arch x86_64 \
    --qf '%{version}-%{release}.%{arch}\n' kernel-core 2>/dev/null" | grep -E '^[0-9][0-9A-Za-z._-]*\.x86_64$' | sort -uV
}

if [[ "$stage" == modules ]]; then
  out="${1:?usage: $0 modules MODULES_DIR}"
  [[ ! -e "$out" ]] || die "$out exists; build into a new directory"
  if [[ -n "${NONFREE_KERNELS:-}" ]]; then
    read -r -a kernels <<<"$NONFREE_KERNELS"
  else
    mapfile -t kernels < <(kernels_from_repos all)
  fi
  [[ ${#kernels[@]} -gt 0 ]] || die "no kernels found for Fedora $FEDORA_RELEASE"
  for k in "${kernels[@]}"; do [[ "$k" =~ ^[0-9][0-9A-Za-z._-]*\.fc$FEDORA_RELEASE\.x86_64$ ]] || die "kernel $k is not a Fedora $FEDORA_RELEASE x86_64 kernel release"; done
  latest="${NONFREE_LATEST:-$(kernels_from_repos stable | tail -1)}"
  printf '%s\n' "${kernels[@]}" | grep -qxF "$latest" || die "the latest stable kernel $latest is not in the build list"
  log "kernels: ${kernels[*]} (kmod-nvidia-open follows $latest)"
  open_tar="$(fetch "open-gpu-kernel-modules-$NVIDIA_VERSION.tar.gz" "$NVIDIA_OPEN_URL" "$NVIDIA_OPEN_SHA256")"

  # The build image: the toolchain and every kernel's kernel-devel, made
  # once for this version. Stage 3 builds again in exactly this image.
  log "build image $KBUILD_IMAGE"
  {
    printf 'FROM %s\n' "$FEDORA_IMAGE"
    printf 'RUN dnf -q -y install rpm-build gcc gcc-c++ make binutils elfutils-libelf-devel coreutils kmod openssl'
    for k in "${kernels[@]}"; do
      v="${k%-*}" r="${k##*-}"; r="${r%.x86_64}"
      # Kernels that left the mirrors stay on Koji.
      printf ' && (dnf -q -y install kernel-devel-%s || dnf -q -y install https://kojipkgs.fedoraproject.org/packages/kernel/%s/%s/x86_64/kernel-devel-%s.rpm)' "$k" "$v" "$r" "$k"
    done
    printf ' && dnf clean all\n'
  } | $PODMAN build --network=host -q -t "$KBUILD_IMAGE" -f - >/dev/null || die "cannot build $KBUILD_IMAGE"
  image_id="$($PODMAN image inspect --format '{{.Id}}' "$KBUILD_IMAGE")"

  work="$(mktemp -d)"
  trap 'sudo rm -rf "$work"' EXIT
  mkdir -p "$work/SOURCES" "$work/SPECS" "$out"
  cp -p "$open_tar" "$work/SOURCES/"
  cp -p "$nv/nvidia-open-kmod.spec" "$work/SPECS/"
  for k in "${kernels[@]}"; do
    log "kmod for $k (unsigned, no network)"
    $PODMAN run --rm --network=none --security-opt label=disable -v "$work:/rpmbuild" -e KVER="$k" "$image_id" bash -euc '
      rpmbuild --define "_topdir /rpmbuild" --define "kver $KVER" --define "_smp_mflags -j$(nproc)" \
        -bb /rpmbuild/SPECS/nvidia-open-kmod.spec >/rpmbuild/kmod-$KVER.log 2>&1 || { tail -60 /rpmbuild/kmod-$KVER.log; exit 1; }' ||
      die "kmod build for $k failed"
    rpm="$(sudo find "$work/RPMS" -name "kmod-nvidia-open-$k-$NVIDIA_VERSION-*.rpm" | head -1)"
    [[ -n "$rpm" ]] || die "no RPM for $k"
    mkdir -p "$out/$k" "$out/unsigned-rpms"
    sudo cp "$rpm" "$out/unsigned-rpms/"
    # cpio's status decides: rpm2cpio may die of SIGPIPE after the trailer
    # (packages/nvidia/check-identical.sh).
    (set +o pipefail; cd "$out/$k" && rpm2cpio "$rpm" | cpio -idm --quiet "./usr/lib/modules/$k/extra/nvidia-open/*.ko" &&
      mv "usr/lib/modules/$k/extra/nvidia-open/"*.ko . && rm -rf usr)
  done
  sudo chown -R "$(id -u):$(id -g)" "$out"
  printf '%s\n' "${kernels[@]}" >"$out/KERNELS"
  echo "$latest" >"$out/LATEST"
  {
    echo "image: $KBUILD_IMAGE"
    echo "id: $image_id"
    echo "nvidia: $NVIDIA_VERSION"
    echo "commit: $commit"
    echo "lab: $NONFREE_LAB"
    $PODMAN run --rm --network=none "$image_id" bash -c 'echo "gcc: $(gcc -dumpfullversion)"; echo "binutils: $(rpm -q binutils)"'
  } >"$out/BUILD-IMAGE"
  (cd "$out" && find . -name '*.ko' -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
  log "unsigned modules in $out ($(find "$out" -name '*.ko' | wc -l) files); next: scripts/release/sign-modules.sh on the release signer"
  log "unsigned kmod RPMs (lab tests only, never published) in $out/unsigned-rpms"
  exit 0
fi

# --- stage 3: packages ----------------------------------------------------------
sigs="${1:?usage: $0 packages SIG_DIR OUT_DIR}"
out="${2:?usage: $0 packages SIG_DIR OUT_DIR}"
sigs="$(cd "$sigs" && pwd)" || die "no $sigs"
[[ -f "$sigs/SIGNED-MODULES-OK" ]] || die "$sigs/SIGNED-MODULES-OK missing (scripts/release/sign-modules.sh)"
(cd "$sigs" && sha256sum -c --quiet SHA256SUMS) || die "checksum mismatch in $sigs"
mode="$(sed -n 's/^mode: //p' "$sigs/SIGNED-MODULES-OK")"
cert="$sigs/basalt-nonfree-module-signing.der"
[[ -f "$cert" ]] || die "no signing certificate in $sigs"
if [[ "$NONFREE_LAB" != 1 ]]; then
  [[ "$mode" != test-key ]] || die "$sigs was signed with a throwaway test key (NONFREE_LAB=1 for a lab build)"
  ca="$REPO_ROOT/packages/basalt-security/basalt-module-ca.der"
else
  ca="${NONFREE_MODULE_CA:-}"
  [[ -n "$ca" ]] || { [[ "$mode" == test-key ]] && ca="$sigs.test-ca.der"; }
  [[ -f "$ca" ]] || die "lab build: set NONFREE_MODULE_CA to the CA certificate that issued $cert"
  log "LAB build: modules signed by $(openssl x509 -inform DER -in "$cert" -noout -subject), CA $ca"
fi
# The signing certificate must come from the module CA (DER or PEM).
cafmt=DER; grep -q 'BEGIN CERTIFICATE' "$ca" && cafmt=PEM
openssl x509 -inform "$cafmt" -in "$ca" -out "$sigs/.ca.pem" 2>/dev/null || die "cannot read $ca"
openssl x509 -inform DER -in "$cert" -out "$sigs/.cert.pem" || die "cannot read $cert"
openssl verify -CAfile "$sigs/.ca.pem" -purpose any "$sigs/.cert.pem" >/dev/null || die "$cert was not issued by $ca"
rm -f "$sigs/.ca.pem" "$sigs/.cert.pem"
image_id="$(sed -n 's/^id: //p' "$sigs/BUILD-IMAGE")"
$PODMAN image exists "$image_id" || die "the kmod build image $image_id of stage 1 is gone; run the modules stage again (and sign again)"
mapfile -t kernels <"$sigs/KERNELS"
latest="$(cat "$sigs/LATEST")"
[[ ! -e "$out" ]] || die "$out exists; build into a new directory"

run_file="${NONFREE_RUN:-}"
if [[ -n "$run_file" ]]; then
  echo "$NVIDIA_RUN_SHA256  $run_file" | sha256sum -c --quiet - || die "$run_file is not NVIDIA $NVIDIA_VERSION (checksum)"
else
  run_file="$(fetch "NVIDIA-Linux-x86_64-$NVIDIA_VERSION.run" "$NVIDIA_RUN_URL" "$NVIDIA_RUN_SHA256")"
fi
open_tar="$(fetch "open-gpu-kernel-modules-$NVIDIA_VERSION.tar.gz" "$NVIDIA_OPEN_URL" "$NVIDIA_OPEN_SHA256")"
modprobe_tar="$(fetch "nvidia-modprobe-$NVIDIA_VERSION.tar.gz" "$NVIDIA_MODPROBE_URL" "$NVIDIA_MODPROBE_SHA256")"
persist_tar="$(fetch "nvidia-persistenced-$NVIDIA_VERSION.tar.gz" "$NVIDIA_PERSISTENCED_URL" "$NVIDIA_PERSISTENCED_SHA256")"

work="$(mktemp -d)"
trap 'sudo rm -rf "$work"' EXIT
mkdir -p "$work/SOURCES" "$work/SPECS" "$out"

# 3a. Kernel modules, signed: the stage 1 build again, in the same image.
cp -p "$open_tar" "$work/SOURCES/"
cp -p "$nv/nvidia-open-kmod.spec" "$nv/kmod-nvidia-open.spec" "$work/SPECS/"
for k in "${kernels[@]}"; do
  [[ -f "$sigs/$k/SIGNATURES" ]] || die "no signatures for $k"
  tar -C "$sigs/$k" --owner=0 --group=0 --sort=name -cf "$work/SOURCES/nvidia-open-signatures-$NVIDIA_VERSION-$k.tar" .
  log "kmod for $k (signed)"
  $PODMAN run --rm --network=none --security-opt label=disable -v "$work:/rpmbuild" -e KVER="$k" "$image_id" bash -euc '
    rpmbuild --define "_topdir /rpmbuild" --define "kver $KVER" --define "signatures 1" --define "_smp_mflags -j$(nproc)" \
      -ba /rpmbuild/SPECS/nvidia-open-kmod.spec >/rpmbuild/kmod-$KVER.log 2>&1 || { tail -60 /rpmbuild/kmod-$KVER.log; exit 1; }' ||
    die "signed kmod build for $k failed (a module that differs from the signed one means the build is not reproducible)"
done
log "kmod-nvidia-open follows $latest"
$PODMAN run --rm --network=none --security-opt label=disable -v "$work:/rpmbuild" -e KVER="$latest" "$image_id" bash -euc '
  rpmbuild --define "_topdir /rpmbuild" --define "kver $KVER" -ba /rpmbuild/SPECS/kmod-nvidia-open.spec >/rpmbuild/meta.log 2>&1 ||
    { tail -40 /rpmbuild/meta.log; exit 1; }' || die "kmod-nvidia-open build failed"

# 3b. Userspace from the .run, the open tools, basalt-nvidia.
cp -p "$run_file" "$modprobe_tar" "$persist_tar" "$nv/files.list" "$nv/NOTICE.nvidia" "$nv/70-nvidia-power.preset" "$work/SOURCES/"
cp -p "$nv/nvidia-driver.spec" "$nv/nvidia-modprobe.spec" "$nv/nvidia-persistenced.spec" "$work/SPECS/"
find "$nv/basalt-nvidia" -maxdepth 1 -type f ! -name '*.spec' -exec cp -p {} "$work/SOURCES/" \;
cp -p "$nv/basalt-nvidia/basalt-nvidia.spec" "$work/SPECS/"
install -m 0644 "$cert" "$work/SOURCES/basalt-nonfree-module-signing.der"
log "userspace, open tools and basalt-nvidia in $FEDORA_IMAGE"
in_fedora -v "$work:/rpmbuild" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpm-build dnf5-plugins >/dev/null 2>&1 || { echo "dnf install failed"; exit 1; }
  for s in nvidia-modprobe nvidia-persistenced; do
    dnf -q -y builddep /rpmbuild/SPECS/$s.spec >/dev/null 2>&1 || { echo "builddep $s failed"; exit 1; }
  done
  dnf -q -y install systemd-rpm-macros gawk tar zstd >/dev/null 2>&1
  for s in nvidia-driver nvidia-modprobe nvidia-persistenced basalt-nvidia; do
    rpmbuild --define "_topdir /rpmbuild" -ba /rpmbuild/SPECS/$s.spec >/rpmbuild/$s.log 2>&1 || { tail -60 /rpmbuild/$s.log; exit 1; }
  done' || die "package build failed"

sudo find "$work/RPMS" "$work/SRPMS" -name '*.rpm' -exec cp {} "$out/" \;
sudo chown -R "$(id -u):$(id -g)" "$out"

# 3c. Packaging rules and lint.
log "check: byte-identical NVIDIA files, licenses, signed modules"
in_fedora -v "$REPO_ROOT/packages/nvidia:/nv:ro" -v "$out:/rpms:ro" -v "$run_file:/run.run:ro" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install cpio tar zstd >/dev/null 2>&1
  /nv/check-identical.sh /run.run /rpms' || die "the packaging checks failed"
log "rpmlint (scripts/ci/rpmlint-nonfree.toml)"
in_fedora -v "$out:/rpms:ro" -v "$REPO_ROOT/scripts/ci:/ci:ro" "$FEDORA_IMAGE" bash -euc '
  dnf -q -y install rpmlint >/dev/null 2>&1 || { echo "dnf install failed" >&2; exit 1; }
  rpmlint -c /ci/rpmlint-nonfree.toml /rpms/*.rpm' || die "rpmlint found errors in the built packages"

cat >"$out/BUILD-INFO.txt" <<EOF
Basalt OS non-free build (UNSIGNED packages, input for OB_REPO=$OB_REPO scripts/release/sign.sh)
repository:     $OB_REPO
$([[ "$NONFREE_LAB" == 1 ]] && echo "LAB BUILD:      never publish (signatures: $mode)")
fedora release: $FEDORA_RELEASE
architecture:   $ARCH
commit:         $commit
nvidia:         $NVIDIA_VERSION (.run $NVIDIA_RUN_SHA256)
kernels:        ${kernels[*]}
latest:         $latest
module signer:  $(openssl x509 -inform DER -in "$cert" -noout -subject -fingerprint -sha256 | tr '\n' ' ')
built:          $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
(cd "$out" && find . -maxdepth 1 -type f -name '*.rpm' -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
log "$OB_REPO build in $out:"
find "$out" -maxdepth 1 -name '*.rpm' -printf '  %P  %s bytes\n' | sort >&2
