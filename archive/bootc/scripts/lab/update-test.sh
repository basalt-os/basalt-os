#!/usr/bin/env bash
# Update and rollback test on the lab VM.
#
#   NEW_VERSION=0.0.2 scripts/lab/update-test.sh
#
# 1. build, sign and publish NEW_VERSION; the channel tag now points at it
# 2. bootc upgrade in the VM (timed), reboot, check the version and denials
# 3. negative test: point the channel tag at an unsigned image; bootc upgrade
#    must refuse it; the tag is restored afterwards
# 4. bootc rollback, reboot, check the old version is back and denials
source "$(dirname "$0")/../lib.sh"
lab_registry_opts
L="$REPO_ROOT/scripts/lab"
vm() { "$L/vm.sh" ssh "$@"; }
: "${NEW_VERSION:?set NEW_VERSION}"
: "${SKIP_BUILD:=0}"

denials() { vm 'ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot 2>/dev/null | grep -c "^type=" || true'; }
version() { vm '. /etc/os-release; echo "$VERSION_ID"'; }

reboot_vm() {
  local t0; t0=$(date +%s)
  vm 'systemctl reboot' || true
  sleep 10
  "$L/vm.sh" wait-ssh
  log "reboot to SSH: $(($(date +%s) - t0))s"
  vm 'systemd-analyze | head -1'
}

old="$(version)"
log "VM runs $old; denials since boot: $(denials)"

if [[ "$SKIP_BUILD" != 1 ]]; then
  log "building and publishing $NEW_VERSION"
  BASALT_VERSION="$NEW_VERSION" "$REPO_ROOT/scripts/build.sh"
  BASALT_VERSION="$NEW_VERSION" "$REPO_ROOT/scripts/publish.sh"
  "$REPO_ROOT/scripts/verify.sh"
fi
new_digest="$(skopeo inspect "${SKOPEO_REG_OPTS[@]}" --format '{{.Digest}}' "docker://$IMAGE:$CHANNEL_TAG")"

log "bootc upgrade (download and stage)"
t0=$(date +%s)
vm 'bootc upgrade'
log "bootc upgrade took $(($(date +%s) - t0))s"
vm 'bootc status --format=humanreadable'
reboot_vm
now="$(version)"
log "after upgrade: $now (expected $NEW_VERSION); denials since boot: $(denials)"
[[ "$now" == "$NEW_VERSION" ]] || die "upgrade did not boot $NEW_VERSION"
vm 'grep -h "Basalt OS" /usr/lib/motd.d/10-basalt'

if skopeo inspect "${SKOPEO_REG_OPTS[@]}" "docker://$IMAGE:unsigned-test" >/dev/null 2>&1; then
  log "negative test: $CHANNEL_TAG -> unsigned image"
  skopeo copy --quiet --preserve-digests "${SKOPEO_COPY_OPTS[@]}" \
    "docker://$IMAGE:unsigned-test" "docker://$IMAGE:$CHANNEL_TAG"
  # Capture instead of tee: a log redirected to a file must not be reopened.
  out="$(vm 'bootc upgrade' 2>&1)" && rc=0 || rc=$?
  printf '%s\n' "$out" >&2
  if [[ $rc != 0 ]] && grep -qiE 'signature' <<<"$out"; then
    log "bootc upgrade refused the unsigned image (exit $rc)"
  else
    die "bootc upgrade did not refuse the unsigned image (exit $rc)"
  fi
  skopeo copy --quiet --preserve-digests "${SKOPEO_COPY_OPTS[@]}" \
    "docker://$IMAGE@$new_digest" "docker://$IMAGE:$CHANNEL_TAG"
  log "$CHANNEL_TAG restored to $new_digest"
fi

log "bootc rollback"
vm 'bootc rollback'
reboot_vm
back="$(version)"
log "after rollback: $back (expected $old); denials since boot: $(denials)"
[[ "$back" == "$old" ]] || die "rollback did not return to $old"
vm 'bootc status --format=humanreadable'
