# Shared by packages/cotty/build.sh (RPM) and packages/cotty/build-deb.sh:
# fetch cotty at the pinned commit and make its source archive. Source it
# after scripts/lib.sh, do not execute it.
# shellcheck shell=bash

# shellcheck source=packages/cotty/source.conf
source "$REPO_ROOT/packages/cotty/source.conf"
pin_commit="${COTTY_COMMIT_OVERRIDE:-}"
[[ -n "$pin_commit" ]] && COTTY_COMMIT="$pin_commit"
[[ "$COTTY_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "COTTY_COMMIT must be a full commit id"
[[ "$COTTY_VERSION_BASE" =~ ^[0-9]+(\.[0-9]+)*$ ]] || die "bad COTTY_VERSION_BASE: $COTTY_VERSION_BASE"

# cotty_fetch WORK: fetch the pinned commit into WORK/git, then set
# COTTY_VERSION, COTTY_EPOCH (commit time, for SOURCE_DATE_EPOCH) and write
# WORK/SOURCES/cotty-$COTTY_VERSION.tar.gz (git archive of the commit).
cotty_fetch() {
  local work="$1" when
  mkdir -p "$work/git" "$work/SOURCES"
  git -C "$work/git" init -q
  if [[ -n "${COTTY_SRC:-}" ]]; then
    # Offline override: a clone (a directory) or a git bundle (a file); the
    # pin is checked below.
    local src="$COTTY_SRC"
    [[ -d "$src" ]] && src="$(cd "$src" && pwd)"
    git -C "$work/git" fetch -q "$src" "$COTTY_COMMIT" || die "commit $COTTY_COMMIT not in $COTTY_SRC"
    log "cotty from $COTTY_SRC"
  else
    # Anonymous fetch of a public repository; a credential prompt would hang
    # an unattended build, so it fails instead.
    GIT_TERMINAL_PROMPT=0 git -C "$work/git" fetch -q --depth 1 "$COTTY_REPO_URL" "$COTTY_COMMIT" ||
      die "cannot fetch $COTTY_COMMIT from $COTTY_REPO_URL (offline: set COTTY_SRC to a clone or bundle holding it)"
  fi
  [[ "$(git -C "$work/git" rev-parse FETCH_HEAD)" == "$COTTY_COMMIT" ]] || die "fetched commit differs from the pin"
  COTTY_EPOCH="$(git -C "$work/git" show -s --format=%ct "$COTTY_COMMIT")"
  when="$(TZ=UTC date -u -d "@$COTTY_EPOCH" +%Y%m%d%H%M%S)"
  COTTY_VERSION="$COTTY_VERSION_BASE~git$when.${COTTY_COMMIT:0:12}"
  # tar.umask 022: files 0644 and directories 0755 (git's default 0002 gives
  # group-writable modes that lintian flags in /usr/share/doc).
  git -C "$work/git" -c tar.umask=022 archive --format=tar.gz --prefix="cotty-$COTTY_VERSION/" \
    -o "$work/SOURCES/cotty-$COTTY_VERSION.tar.gz" "$COTTY_COMMIT"
  export COTTY_VERSION COTTY_EPOCH
}
