#!/usr/bin/env bash
# Development Secure Boot keys for the lab (never for release).
#
#   scripts/lab/sb-keys.sh        create missing keys under $LAB_DIR/sb-keys (0700)
#
#   module-ca.{key,pem,der}       Basalt kernel module CA: the certificate enrolled
#                                 as a MOK (shim's MokList). X.509 CA with keyCertSign
#                                 and no digitalSignature, as Fedora kernels require
#                                 for the .machine keyring (INTEGRITY_CA_MACHINE_KEYRING_MAX).
#   module-signing.{key,pem,der}  module signing certificate issued by that CA
#                                 (digitalSignature, code signing); signs .ko files
#   PK, KEK, db .{key,pem,der}    a full UEFI key set for custom db mode
#   guid                          owner GUID for the UEFI signature lists
#
# Private keys are 0600 and never printed. A release key set is created
# offline in a key ceremony (docs/secure-boot.md), never on a build host.
source "$(dirname "$0")/../lib.sh"

dir="$LAB_DIR/sb-keys"
umask 077
install -d -m 0700 "$dir"
cd "$dir" || die "cannot enter $dir"

ossl() { openssl "$@" 2>/dev/null; }

[[ -s guid ]] || uuidgen >guid

# Self-signed certificate: name, CN, bits, days, extensions (openssl -addext).
selfsigned() {
  local name="$1" cn="$2" bits="$3" days="$4"; shift 4
  [[ -s "$name.key" ]] && return 0
  log "creating $name ($cn)"
  local ext=()
  for e in "$@"; do ext+=(-addext "$e"); done
  ossl req -x509 -new -newkey "rsa:$bits" -nodes -sha256 -days "$days" -subj "/CN=$cn/O=Basalt OS lab/" \
    -keyout "$name.key" -out "$name.pem" "${ext[@]}"
  ossl x509 -in "$name.pem" -outform DER -out "$name.der"
}

# UEFI keys: RSA 2048, the size every firmware supports.
selfsigned PK  "Basalt OS lab platform key (development)" 2048 3650
selfsigned KEK "Basalt OS lab key exchange key (development)" 2048 3650
selfsigned db  "Basalt OS lab signature database key (development)" 2048 3650

# Module CA (enrolled as MOK).
selfsigned module-ca "Basalt OS lab kernel module CA (development)" 4096 3650 \
  "basicConstraints=critical,CA:TRUE" "keyUsage=critical,keyCertSign,cRLSign" \
  "subjectKeyIdentifier=hash"

# Module signing certificate, issued by the module CA.
if [[ ! -s module-signing.key ]]; then
  log "creating module-signing (issued by module-ca)"
  ossl req -new -newkey rsa:4096 -nodes -sha256 -subj "/CN=Basalt OS lab kernel module signing (development)/O=Basalt OS lab/" \
    -keyout module-signing.key -out module-signing.csr
  cat >module-signing.ext <<'EOF'
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=codeSigning
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid
EOF
  ossl x509 -req -in module-signing.csr -CA module-ca.pem -CAkey module-ca.key -CAcreateserial \
    -sha256 -days 1825 -extfile module-signing.ext -out module-signing.pem
  ossl x509 -in module-signing.pem -outform DER -out module-signing.der
  rm -f module-signing.csr
fi

chmod 0600 ./*.key
chmod 0644 ./*.pem ./*.der guid
for c in PK KEK db module-ca module-signing; do
  printf '%-15s %s\n' "$c" "$(openssl x509 -in "$c.pem" -noout -subject -enddate | paste -sd' ' -)"
done
find "$dir" -maxdepth 1 -name '*.key' -printf '%m %p\n' | sort
