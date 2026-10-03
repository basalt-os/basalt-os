#!/usr/bin/env bash
# Build the Basalt OS image into local container storage.
#   BASALT_VERSION=0.0.2 scripts/build.sh
# Tags: localhost/basalt-os:<version> and <registry>/<repo>:<version>.
source "$(dirname "$0")/lib.sh"

"$REPO_ROOT/scripts/site-overlay.sh"

: "${BUILD_NETWORK:=host}"
start=$(date +%s)
log "building Basalt OS $BASALT_VERSION from $BASE_IMAGE"
run $PODMAN build \
  --network="$BUILD_NETWORK" \
  --pull=newer \
  --build-arg BASE_IMAGE="$BASE_IMAGE" \
  --build-arg BASALT_VERSION="$BASALT_VERSION" \
  -t "$LOCAL_IMAGE:$BASALT_VERSION" \
  -t "$IMAGE:$BASALT_VERSION" \
  -f "$REPO_ROOT/Containerfile" "$REPO_ROOT"
end=$(date +%s)

size=$($PODMAN image inspect "$LOCAL_IMAGE:$BASALT_VERSION" --format '{{.Size}}')
log "built $IMAGE:$BASALT_VERSION in $((end - start))s, uncompressed size $((size / 1024 / 1024)) MiB"
