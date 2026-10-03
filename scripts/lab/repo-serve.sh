#!/usr/bin/env bash
# Serve $REPO_DIR over plain HTTP to the lab network (packages and metadata
# are GPG signed, so the transport does not need to be trusted).
#
#   repo-serve.sh up | down | status
#
# A small busybox httpd container bound to the lab network's gateway address
# (${VM_SUBNET}.1:${REPO_PORT}), so only lab VMs can reach it. The libvirt
# network must exist first (vm.sh net-up).
source "$(dirname "$0")/../lib.sh"

: "${VM_SUBNET:=10.150.0}"
: "${REPO_PORT:=8098}"
: "${REPO_CONTAINER:=basalt-lab-repo}"
: "${DOCKER:=docker}"
bind="${VM_SUBNET}.1"

case "${1:-status}" in
  up)
    mkdir -p "$REPO_DIR"
    if $DOCKER inspect "$REPO_CONTAINER" >/dev/null 2>&1; then
      $DOCKER start "$REPO_CONTAINER" >/dev/null
    else
      run $DOCKER run -d --name "$REPO_CONTAINER" --restart unless-stopped \
        --security-opt label=disable -p "$bind:$REPO_PORT:8080" \
        -v "$REPO_DIR:/srv:ro" docker.io/library/busybox:stable \
        httpd -f -p 8080 -h /srv >/dev/null
    fi
    log "repository at http://$bind:$REPO_PORT/"
    ;;
  down) $DOCKER rm -f "$REPO_CONTAINER" >/dev/null && log "stopped $REPO_CONTAINER" ;;
  status)
    $DOCKER ps -a --filter "name=^$REPO_CONTAINER\$" --format '{{.Names}} {{.Status}} {{.Ports}}'
    curl -fsS -o /dev/null -w "repomd: HTTP %{http_code}\n" "http://$bind:$REPO_PORT/$FEDORA_RELEASE/$ARCH/repodata/repomd.xml" || true
    ;;
  *) sed -n '2,10p' "$0"; exit 2 ;;
esac
