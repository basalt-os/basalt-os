#!/usr/bin/env bash
# Tang server for lab VMs (network-bound disk encryption with Clevis).
#
#   tang-serve.sh up | down | status | thp
#
# A Fedora container with tang, served by socat on the lab network's
# gateway address (${VM_SUBNET}.1:${TANG_PORT}), so only lab VMs reach it.
# Its keys live in $LAB_DIR/tang (0700) and survive the container. `thp`
# prints the signing key thumbprint that clients pin (basalt.tang-thp=).
source "$(dirname "$0")/../lib.sh"

: "${VM_SUBNET:=10.150.0}"
: "${TANG_PORT:=7500}"
: "${TANG_CONTAINER:=basalt-lab-tang}"
IMAGE=localhost/basalt-lab-tang
bind="${VM_SUBNET}.1"
db="$LAB_DIR/tang"

image() {
  $PODMAN image exists "$IMAGE" && return 0
  log "building $IMAGE"
  printf 'FROM registry.fedoraproject.org/fedora:44\nRUN dnf -y install tang socat jose && dnf clean all\n' |
    $PODMAN build --network=host -q -t "$IMAGE" -f - >/dev/null
}

case "${1:-status}" in
  up)
    image
    install -d -m 0700 "$db"
    if [[ -z "$(ls -A "$db")" ]]; then
      $PODMAN run --rm --security-opt label=disable -v "$db:/db" "$IMAGE" /usr/libexec/tangd-keygen /db
      sudo chown -R "$(id -u):$(id -g)" "$db"; chmod 0600 "$db"/*
      log "generated Tang keys in $db"
    fi
    if $PODMAN container exists "$TANG_CONTAINER"; then
      $PODMAN start "$TANG_CONTAINER" >/dev/null
    else
      run $PODMAN run -d --name "$TANG_CONTAINER" --security-opt label=disable \
        -p "$bind:$TANG_PORT:$TANG_PORT" -v "$db:/db:ro" "$IMAGE" \
        socat "TCP-LISTEN:$TANG_PORT,reuseaddr,fork" "EXEC:/usr/libexec/tangd /db" >/dev/null
    fi
    log "Tang at http://$bind:$TANG_PORT"
    ;;
  down) $PODMAN rm -f "$TANG_CONTAINER" >/dev/null 2>&1 && log "stopped $TANG_CONTAINER" || true ;;
  status)
    $PODMAN ps -a --filter "name=^$TANG_CONTAINER\$" --format '{{.Names}} {{.Status}}'
    curl -fsS -o /dev/null -w "advertisement: HTTP %{http_code}\n" "http://$bind:$TANG_PORT/adv" || true ;;
  thp)
    image
    $PODMAN run --rm --security-opt label=disable -v "$db:/db:ro" "$IMAGE" bash -c '
      for f in /db/*.jwk; do grep -q "\"verify\"" "$f" && jose jwk thp -i "$f"; done' ;;
  *) sed -n '2,9p' "$0"; exit 2 ;;
esac
