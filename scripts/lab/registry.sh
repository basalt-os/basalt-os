#!/usr/bin/env bash
# Lab OCI registry: the "distribution" registry in a container, with TLS from a
# private lab CA and htpasswd authentication, bound to a single address.
#
#   scripts/lab/registry.sh up       create CA, certificate, credentials; start
#   scripts/lab/registry.sh status   show the container and the catalog
#   scripts/lab/registry.sh down     stop and remove the container (data kept)
#
# Layout under $LAB_DIR/registry (mode 0700 where it holds secrets):
#   ca/            lab CA key and certificate (never mounted into the container)
#   certs/         server certificate and key (mounted read-only)
#   auth/          htpasswd (mounted read-only) and the plain password (0600)
#   client-certs/  ca.crt for clients: podman/skopeo --cert-dir
#   auth.json      registry credentials for podman/skopeo/cosign (0600)
#   data/          registry storage
#
# This is a lab convenience for a private phase, not a production registry.
source "$(dirname "$0")/../lib.sh"

: "${REGISTRY_BIND:?set REGISTRY_BIND in .env}"
: "${REGISTRY_PORT:=5000}"
: "${REGISTRY_HOSTNAMES:=}"
: "${REGISTRY_USER:=basalt-lab}"
: "${REGISTRY_CONTAINER:=basalt-lab-registry}"
: "${REGISTRY_ENGINE:=docker}"
: "${REGISTRY_IMAGE:=docker.io/library/registry:2}"

dir="$LAB_DIR/registry"
endpoint="${REGISTRY_BIND}:${REGISTRY_PORT}"

make_ca() {
  install -d -m 0700 "$dir/ca"
  [[ -f "$dir/ca/ca.key" ]] && return 0
  log "creating lab CA"
  (umask 077; openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$dir/ca/ca.key" -out "$dir/ca/ca.crt" -days 825 \
    -subj "/CN=Basalt lab registry CA" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null)
}

make_server_cert() {
  install -d -m 0700 "$dir/certs"
  [[ -f "$dir/certs/server.crt" ]] && return 0
  log "issuing server certificate for $endpoint"
  local san="IP:${REGISTRY_BIND}" h
  IFS=',' read -ra hosts <<<"$REGISTRY_HOSTNAMES"
  for h in "${hosts[@]}"; do [[ -n "$h" ]] && san+=",DNS:$h"; done
  (umask 077
   openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
     -keyout "$dir/certs/server.key" -out "$dir/certs/server.csr" \
     -subj "/CN=${REGISTRY_BIND}" 2>/dev/null
   printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\n' "$san" \
     >"$dir/certs/ext.cnf"
   openssl x509 -req -in "$dir/certs/server.csr" -CA "$dir/ca/ca.crt" -CAkey "$dir/ca/ca.key" \
     -CAcreateserial -days 397 -extfile "$dir/certs/ext.cnf" -out "$dir/certs/server.crt" 2>/dev/null)
  rm -f "$dir/certs/server.csr"
  install -d -m 0755 "$dir/client-certs"
  install -m 0644 "$dir/ca/ca.crt" "$dir/client-certs/ca.crt"
}

make_credentials() {
  install -d -m 0700 "$dir/auth"
  [[ -f "$dir/auth/htpasswd" ]] && return 0
  log "creating registry credentials for user $REGISTRY_USER (password stays on this host)"
  local pass
  pass="$(openssl rand -hex 24)"
  (umask 077
   printf '%s' "$pass" >"$dir/auth/password"
   # htpasswd reads the password from stdin (-i), so it never shows in ps.
   printf '%s' "$pass" | htpasswd -Bin "$REGISTRY_USER" >"$dir/auth/htpasswd"
   printf '{"auths":{"%s":{"auth":"%s"}}}\n' "$endpoint" \
     "$(printf '%s:%s' "$REGISTRY_USER" "$pass" | base64 -w0)" >"$dir/auth.json")
}

up() {
  command -v htpasswd >/dev/null || die "htpasswd not found (httpd-tools)"
  install -d -m 0755 "$dir" "$dir/data"
  make_ca
  make_server_cert
  make_credentials
  if $REGISTRY_ENGINE container inspect "$REGISTRY_CONTAINER" >/dev/null 2>&1; then
    log "$REGISTRY_CONTAINER already exists"; $REGISTRY_ENGINE start "$REGISTRY_CONTAINER" >/dev/null
  else
    run $REGISTRY_ENGINE run -d --name "$REGISTRY_CONTAINER" --restart unless-stopped \
      -p "${endpoint}:5000" \
      -v "$dir/data:/var/lib/registry:Z" \
      -v "$dir/certs:/certs:ro,Z" \
      -v "$dir/auth:/auth:ro,Z" \
      -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/server.crt \
      -e REGISTRY_HTTP_TLS_KEY=/certs/server.key \
      -e REGISTRY_AUTH=htpasswd \
      -e REGISTRY_AUTH_HTPASSWD_REALM="Basalt lab registry" \
      -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
      -e REGISTRY_STORAGE_DELETE_ENABLED=true \
      "$REGISTRY_IMAGE" >/dev/null
  fi
  status
}

status() {
  $REGISTRY_ENGINE ps --filter "name=^${REGISTRY_CONTAINER}$" --format '{{.Names}} {{.Status}} {{.Ports}}'
  local user pass
  user="$REGISTRY_USER"; pass="$(cat "$dir/auth/password")"
  # Credentials go through a curl config on stdin, not argv.
  printf 'user = "%s:%s"\n' "$user" "$pass" |
    curl -fsS --cacert "$dir/client-certs/ca.crt" -K - "https://${endpoint}/v2/_catalog" || true
  echo
}

down() { run $REGISTRY_ENGINE rm -f "$REGISTRY_CONTAINER"; }

case "${1:-}" in
  up) up ;;
  status) status ;;
  down) down ;;
  *) die "usage: $0 up|status|down" ;;
esac
