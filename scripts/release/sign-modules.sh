#!/usr/bin/env bash
# Release signer: sign the NVIDIA open kernel modules of a basalt-nonfree
# build (scripts/release/build-nonfree.sh modules). Only signatures leave
# this step; build-nonfree.sh packages appends them to the modules it
# builds again, after checking that each module is the one signed here.
#
#   scripts/release/sign-modules.sh --op       MODULES_DIR SIG_DIR   key from 1Password (op), the real run
#   scripts/release/sign-modules.sh --key-file MODULES_DIR SIG_DIR   key from a file (OB_MODULE_KEY_FILE)
#   scripts/release/sign-modules.sh --test-key MODULES_DIR SIG_DIR   throwaway CA and key made here (dry run, never published)
#
# Key material (docs/key-ceremony.md): the basalt-nonfree module signing
# key, a certificate the OpenBasalt kernel module CA issued for it (RSA
# 4096, digitalSignature, code signing), kept on the release signer like the
# packages subkey and separate from the key of Basalt's own modules, so it
# can be revoked alone.
# - --op reads the private key (PEM) from OB_OP_MODULE_KEY_REF (op://) and,
#   if it is encrypted, its passphrase from OB_OP_MODULE_PASSPHRASE_REF,
#   with --out-file into a 0700 directory on a tmpfs.
# - --key-file takes OB_MODULE_KEY_FILE (and OB_MODULE_KEY_PASSPHRASE_FILE
#   for an encrypted key): files of mode 0600, owned by the caller, on a tmpfs.
# - The certificate is public: OB_MODULE_CERT_FILE (DER or PEM), default
#   packages/nvidia/basalt-nonfree-module-signing.der.
# - Signing runs in a container started with --network none; the key lives
#   only in the tmpfs and is shredded before verification. Nothing secret
#   is passed on a command line or printed.
#
# The signature is what the kernel's scripts/sign-file appends: a detached
# CMS (PKCS #7) signature over the module, SHA-512, no certificates and no
# signed attributes, then the module_signature trailer and
# "~Module signature appended~". It is verified in a second container
# with the certificate only.
#
# SIG_DIR gets <kernel>/<module>.ko.sig and <kernel>/SIGNATURES ("module
# NAME SHA256-OF-UNSIGNED SHA256-OF-SIG"), the certificate as
# basalt-nonfree-module-signing.der, KERNELS, LATEST and BUILD-IMAGE from
# MODULES_DIR, SHA256SUMS and SIGNED-MODULES-OK. --test-key also writes the
# throwaway CA certificate to SIG_DIR.test-ca.der (lab builds:
# NONFREE_LAB=1 build-nonfree.sh packages).
source "$(dirname "$0")/../lib.sh"

: "${OP_ACCOUNT:=my.1password.com}"
: "${SIGN_PODMAN:=podman}"
: "${OB_MODULE_CERT_FILE:=$REPO_ROOT/packages/nvidia/basalt-nonfree-module-signing.der}"
MSIGNER_IMAGE="localhost/basalt-module-signer:$FEDORA_RELEASE"
export OP_ACCOUNT

mode="${1:-}"
case "$mode" in --op | --key-file | --test-key) shift ;; *) sed -n '2,44p' "$0"; exit 2 ;; esac
in="${1:?MODULES_DIR}"
out="${2:?SIG_DIR}"
in="$(cd "$in" && pwd)" || die "no $in"
[[ ! -e "$out" ]] || die "$out exists; sign into a new directory"

# 1. Input: every module listed in SHA256SUMS, checksums good, none signed.
[[ -f "$in/SHA256SUMS" && -f "$in/KERNELS" && -f "$in/LATEST" && -f "$in/BUILD-IMAGE" ]] ||
  die "$in is not the output of build-nonfree.sh modules"
(cd "$in" && sha256sum -c --quiet SHA256SUMS) || die "checksum mismatch in $in"
mapfile -t mods < <(awk '{print $2}' "$in/SHA256SUMS")
[[ ${#mods[@]} -gt 0 ]] || die "no modules in $in"
for m in "${mods[@]}"; do
  [[ "$m" =~ ^[0-9][0-9A-Za-z._-]*\.x86_64/nvidia(-drm|-modeset|-uvm|-peermem)?\.ko$ ]] || die "unexpected file $m"
  tail -c 28 "$in/$m" | grep -q '~Module signature appended~' && die "$m is already signed"
done

# Secret work directory on a tmpfs, removed (files shredded) on exit.
base="${XDG_RUNTIME_DIR:-/dev/shm}"
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || base=/dev/shm
[[ "$(stat -f -c %T "$base")" == tmpfs ]] || die "no tmpfs for the key material"
umask 077
secret="$(mktemp -d "$base/basalt-modsign.XXXXXX")"
cleanup() {
  local f
  for f in "$secret"/*; do [[ -f "$f" ]] && shred -u "$f" 2>/dev/null; done
  rm -rf "$secret"
}
trap cleanup EXIT
check_secret_file() {
  local f="$1"
  [[ -f "$f" && ! -L "$f" ]] || die "$f: not a regular file"
  [[ "$(stat -f -c %T "$f")" == tmpfs ]] || die "$f: not on a tmpfs"
  [[ "$(stat -c '%a %u' "$f")" == "600 $(id -u)" ]] || die "$f: must be mode 0600 and owned by $(id -un)"
}

# 2. Tool image, built before any key material exists (host network: rootless
# podman cannot always reach the network from its build sandbox).
if ! $SIGN_PODMAN image exists "$MSIGNER_IMAGE"; then
  log "building $MSIGNER_IMAGE"
  printf 'FROM %s\nRUN dnf -q -y install openssl coreutils && dnf clean all\n' "$FEDORA_IMAGE" |
    $SIGN_PODMAN build --network=host -q -t "$MSIGNER_IMAGE" -f - >/dev/null 2>&1 || die "cannot build $MSIGNER_IMAGE"
fi
signer() { $SIGN_PODMAN run --rm -i --network none --security-opt label=disable "$@"; }

# 3. Key material into the tmpfs; the public certificate next to it.
: >"$secret/passphrase"
case "$mode" in
  --op)
    command -v op >/dev/null || die "op (1Password CLI) not installed"
    [[ "${OB_OP_MODULE_KEY_REF:-}" == op://* ]] || die "set OB_OP_MODULE_KEY_REF to an op:// reference"
    log "reading the module signing key from 1Password ($OP_ACCOUNT); approve the request"
    op read --no-newline --out-file "$secret/key.pem" "$OB_OP_MODULE_KEY_REF" >/dev/null || die "op read of the key failed"
    if [[ "${OB_OP_MODULE_PASSPHRASE_REF:-}" == op://* ]]; then
      op read --no-newline --out-file "$secret/passphrase" "$OB_OP_MODULE_PASSPHRASE_REF" >/dev/null || die "op read of the passphrase failed"
    fi
    chmod 600 "$secret/key.pem" "$secret/passphrase"
    ;;
  --key-file)
    check_secret_file "${OB_MODULE_KEY_FILE:?set OB_MODULE_KEY_FILE}"
    cp "$OB_MODULE_KEY_FILE" "$secret/key.pem"
    if [[ -n "${OB_MODULE_KEY_PASSPHRASE_FILE:-}" ]]; then
      check_secret_file "$OB_MODULE_KEY_PASSPHRASE_FILE"
      cp "$OB_MODULE_KEY_PASSPHRASE_FILE" "$secret/passphrase"
    fi
    ;;
  --test-key)
    log "generating a throwaway module CA and signing key (dry run only)"
    signer -v "$secret:/secret" "$MSIGNER_IMAGE" bash -euc '
      cd /secret
      openssl req -x509 -new -newkey rsa:4096 -sha256 -days 2 -nodes -subj "/CN=Basalt OS dry run module CA (THROWAWAY)/" \
        -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
        -addext "subjectKeyIdentifier=hash" -keyout ca.key -out ca.pem 2>/dev/null
      openssl req -new -newkey rsa:4096 -sha256 -nodes -subj "/CN=Basalt OS dry run nonfree module signing (THROWAWAY)/" \
        -keyout key.pem -out req.pem 2>/dev/null
      printf "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=codeSigning\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid\n" >ext.cnf
      openssl x509 -req -in req.pem -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 -sha256 -extfile ext.cnf -out cert.pem 2>/dev/null
      openssl x509 -in ca.pem -outform DER -out ca.der
      rm -f ca.key req.pem ext.cnf ca.srl'
    OB_MODULE_CERT_FILE="$secret/cert.pem"
    ;;
esac
grep -q 'PRIVATE KEY' "$secret/key.pem" || die "the key material is not a PEM private key"
[[ -f "$OB_MODULE_CERT_FILE" ]] || die "no module signing certificate $OB_MODULE_CERT_FILE (the key ceremony issues it; see docs/key-ceremony.md)"
if grep -q 'BEGIN CERTIFICATE' "$OB_MODULE_CERT_FILE"; then
  openssl x509 -in "$OB_MODULE_CERT_FILE" -out "$secret/cert.pem.pub" || die "cannot read $OB_MODULE_CERT_FILE"
else
  openssl x509 -inform DER -in "$OB_MODULE_CERT_FILE" -out "$secret/cert.pem.pub" || die "cannot read $OB_MODULE_CERT_FILE"
fi
chmod 644 "$secret/cert.pem.pub"

# 4. Sign every module: a copy of the unsigned modules goes into the
# container, signatures come out.
mkdir -p "$out"
out="$(cd "$out" && pwd)"
cp -a "$in"/*.x86_64 "$out/"
rm -rf "$out"/unsigned-rpms
log "signing ${#mods[@]} modules"
signer -v "$secret:/secret:ro" -v "$out:/out" "$MSIGNER_IMAGE" bash -euc '
  pass=""
  [ -s /secret/passphrase ] && pass="-passin file:/secret/passphrase"
  # The key must belong to the certificate.
  [ "$(openssl pkey $pass -in /secret/key.pem -pubout 2>/dev/null | sha256sum)" = "$(openssl x509 -in /secret/cert.pem.pub -noout -pubkey | sha256sum)" ] ||
    { echo "the key does not match the certificate" >&2; exit 1; }
  for ko in /out/*/*.ko; do
    openssl cms -sign -binary -noattr -nocerts -nosmimecap -md sha512 -outform DER \
      -signer /secret/cert.pem.pub -inkey /secret/key.pem $pass -in "$ko" -out "$ko.p7s" 2>/dev/null ||
      { echo "signing $(basename "$ko") failed" >&2; exit 1; }
    len=$(stat -c %s "$ko.p7s")
    # struct module_signature: algo, hash, id_type (2 = PKCS #7), signer_len,
    # key_id_len, 3 bytes padding, big-endian signature length.
    { cat "$ko.p7s"
      printf "\000\000\002\000\000\000\000\000"
      for s in 24 16 8 0; do printf "\\$(printf %03o $((len >> s & 255)))"; done
      printf "~Module signature appended~\n"
    } >"$ko.sig"
    rm -f "$ko.p7s"
  done'

# 5. Shred the key material before verification.
if [[ "$mode" == --test-key ]]; then cp "$secret/ca.der" "$out.test-ca.der"; fi
cp "$secret/cert.pem.pub" "$out/.cert.pem"
cleanup
trap - EXIT
log "key material shredded"

# 6. Verify with the certificate only, in a container that never saw a key.
signer -v "$out:/out" "$MSIGNER_IMAGE" bash -euc '
  n=0
  for ko in /out/*/*.ko; do
    sig="$ko.sig"
    tail -c 28 "$sig" | grep -q "~Module signature appended~" || { echo "bad trailer: $sig" >&2; exit 1; }
    total=$(stat -c %s "$sig")
    # The big-endian length sits right before the 28-byte marker.
    len=$(tail -c 32 "$sig" | head -c 4 | od -An -tu1 | awk "{print \$1*16777216 + \$2*65536 + \$3*256 + \$4}")
    [ $((len + 12 + 28)) -eq "$total" ] || { echo "bad length in $sig" >&2; exit 1; }
    head -c "$len" "$sig" >/tmp/p7s
    openssl cms -verify -binary -inform DER -in /tmp/p7s -content "$ko" -certfile /out/.cert.pem -noverify -out /dev/null 2>/dev/null ||
      { echo "signature does not verify: $ko" >&2; exit 1; }
    n=$((n + 1))
  done
  echo "verified $n module signatures"'
openssl x509 -in "$out/.cert.pem" -outform DER -out "$out/basalt-nonfree-module-signing.der"
rm -f "$out/.cert.pem"

# 7. Signatures and their manifest; the unsigned modules leave the output.
for d in "$out"/*.x86_64; do
  : >"$d/SIGNATURES"
  for ko in "$d"/*.ko; do
    printf 'module %s %s %s\n' "$(basename "$ko")" "$(sha256sum <"$ko" | cut -d' ' -f1)" "$(sha256sum <"$ko.sig" | cut -d' ' -f1)" >>"$d/SIGNATURES"
    rm -f "$ko"
  done
done
cp -p "$in/KERNELS" "$in/LATEST" "$in/BUILD-IMAGE" "$out/"
(cd "$out" && find . -type f ! -name SHA256SUMS -printf '%P\n' | sort | xargs -d '\n' sha256sum >SHA256SUMS)
{
  echo "signed and verified $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "mode: ${mode#--}"
  echo "certificate: $(openssl x509 -inform DER -in "$out/basalt-nonfree-module-signing.der" -noout -subject)"
  echo "fingerprint: $(openssl x509 -inform DER -in "$out/basalt-nonfree-module-signing.der" -noout -fingerprint -sha256 | cut -d= -f2)"
  echo "modules: ${#mods[@]}"
} >"$out/SIGNED-MODULES-OK"
log "signatures in $out; next: scripts/release/build-nonfree.sh packages $out OUT_DIR on the build host"
