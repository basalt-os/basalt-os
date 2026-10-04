#!/usr/bin/env bash
# Tests for basalt-prompt: color depths and fallbacks, NO_COLOR, symbols,
# root, SSH, containers, agent sessions, non-interactive shells, git states,
# the time box, configuration and the user's own PS1. zsh and fish are
# checked when installed.
#
#   packages/basalt-prompt/tests/prompt-test.sh
set -uo pipefail

here=$(cd "$(dirname "$0")/.." && pwd)
LIB=$here/basalt-prompt.bash
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home" "$tmp/fsroot"
# Hermetic: a regular user, outside any container, whoever runs the tests
# (CI runs them as root in a container).
export BASALT_PROMPT_TEST_UID=${BASALT_PROMPT_TEST_UID:-1000} BASALT_PROMPT_TEST_ROOT=$tmp/fsroot
pass=0 fail=0

ok() { pass=$((pass + 1)); printf 'ok   %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf 'FAIL %s\n' "$1"; [[ -n ${2-} ]] && printf '     got: %q\n' "$2"; }
has() { [[ $2 == *"$3"* ]] && ok "$1" || bad "$1" "$2"; }
hasnt() { [[ $2 != *"$3"* ]] && ok "$1" || bad "$1" "$2"; }

# render [VAR=value...] -- CODE: run CODE in an interactive bash with the
# library loaded, in a clean environment, and print the prompt it built.
render() {
  local -a envs=()
  while [[ $# -gt 0 && $1 != -- ]]; do envs+=("$1"); shift; done
  shift
  env -i HOME="$tmp/home" PATH="${TEST_PATH:-$PATH}" TERM=xterm LANG=C.UTF-8 USER=tester HOSTNAME=box.example \
    BASALT_PROMPT_TEST_UID="$BASALT_PROMPT_TEST_UID" BASALT_PROMPT_TEST_ROOT="$BASALT_PROMPT_TEST_ROOT" "${envs[@]}" bash --norc --noprofile -i -c '. "$1" || exit 9; shift; eval "$1"; _basalt_prompt_cmd; printf "%s" "$_bp_ps1"' \
    _ "$LIB" "${1:-:}" 2>/dev/null
}
plain() { printf '%s' "$1" | sed $'s/\x01[^\x02]*\x02//g'; }

esc=$'\e['

# --- non-interactive shells: nothing is defined or changed -----------------------
out=$(env -i PATH="$PATH" bash --norc --noprofile -c 'PS1="keep$ "; . "$1"; echo "${PS1}|${PROMPT_COMMAND-unset}|$(type -t _basalt_prompt_cmd)"' _ "$LIB")
[[ $out == 'keep$ |unset|' ]] && ok "non-interactive bash: no-op" || bad "non-interactive bash: no-op" "$out"
out=$(env -i PATH="$PATH" sh -c '. "$1"; echo "rc=$? ${PROMPT_COMMAND-unset}"' _ "$here/basalt-prompt.sh" 2>&1)
[[ $out == 'rc=0 unset' ]] && ok "profile.d stub under sh: no-op" || bad "profile.d stub under sh: no-op" "$out"
out=$(printf '. %q\necho "${PROMPT_COMMAND-unset}"\n' "$here/basalt-prompt.sh" | env -i PATH="$PATH" bash --norc --noprofile -s 2>&1)
[[ $out == unset ]] && ok "bash reading a script from stdin: no-op" || bad "bash reading a script from stdin: no-op" "$out"
out=$(env -i PATH="$PATH" BASALT_PROMPT=off bash --norc --noprofile -i -c '. "$1"; echo "${PROMPT_COMMAND-unset}"' _ "$LIB" 2>/dev/null)
[[ $out == unset ]] && ok "BASALT_PROMPT=off: no-op" || bad "BASALT_PROMPT=off: no-op" "$out"
out=$(env -i PATH="$PATH" bash --norc --noprofile -i -c '. "$1"; echo "${PROMPT_COMMAND[0]}"' _ "$LIB" 2>/dev/null)
[[ $out == _basalt_prompt_cmd ]] && ok "interactive bash: hook runs first in PROMPT_COMMAND" || bad "interactive bash: hook first" "$out"
out=$(env -i PATH="$PATH" PROMPT_COMMAND='echo hi' bash --norc --noprofile -i -c '. "$1"; echo "${PROMPT_COMMAND[*]}"' _ "$LIB" 2>/dev/null)
[[ $out == '_basalt_prompt_cmd echo hi' ]] && ok "existing PROMPT_COMMAND kept after the hook" || bad "existing PROMPT_COMMAND kept" "$out"

# --- colors ------------------------------------------------------------------------
p=$(render COLORTERM=truecolor -- '')
has "COLORTERM=truecolor: 24-bit colors" "$p" "${esc}38;2;"
has "truecolor: Basalt accent on the prompt sign" "$p" "${esc}38;2;181;80;47m"
p=$(render TERM=foot -- '')
has "TERM=foot: 24-bit colors" "$p" "${esc}38;2;"
p=$(render TERM=xterm-256color -- '')
has "TERM=xterm-256color: 256 colors" "$p" "${esc}38;5;"
hasnt "256 colors: no 24-bit sequences" "$p" "38;2;"
p=$(render TERM=xterm -- '')
has "TERM=xterm: 16 colors" "$p" "${esc}31m"
hasnt "16 colors: no 256 or 24-bit sequences" "$p" "38;"
p=$(render TERM=dumb -- '')
hasnt "TERM=dumb: no colors" "$p" "$esc"
p=$(render COLORTERM=truecolor NO_COLOR=1 -- '')
hasnt "NO_COLOR: no colors even with truecolor" "$p" "$esc"
p=$(render COLORTERM=truecolor BASALT_PROMPT_COLOR=none -- '')
hasnt "color = none: no colors" "$p" "$esc"
p=$(render BASALT_PROMPT_COLOR=256 -- '')
has "color = 256 forced on TERM=xterm" "$p" "${esc}38;5;"
p=$(render COLORTERM=truecolor COLORFGBG='0;15' -- '')
has "COLORFGBG light background: light palette" "$p" "38;2;163;71;46m"
p=$(render COLORTERM=truecolor BASALT_PROMPT_THEME=dark -- '')
has "theme = dark: dark palette" "$p" "38;2;200;98;63m"
p=$(render COLORTERM=truecolor -- '')
[[ $p == *$'\001'* && $p == *$'\002'* ]] && ok "escapes wrapped for readline (\\001 \\002)" || bad "escapes wrapped" "$p"

# --- symbols -------------------------------------------------------------------------
p=$(plain "$(render -- '')")
has "UTF-8 locale: Unicode prompt sign" "$p" '❯'
p=$(plain "$(render LANG=C -- '')")
has "C locale: ASCII prompt sign" "$p" '> '
hasnt "C locale: no Unicode" "$p" '❯'
p=$(plain "$(render TERM=linux -- '')")
has "Linux console: ASCII even with UTF-8" "$p" '> '
p=$(plain "$(render LANG=C BASALT_PROMPT_GLYPHS=unicode -- '')")
has "glyphs = unicode forced" "$p" '❯'

# --- user, host, root, SSH -----------------------------------------------------------
p=$(plain "$(render WAYLAND_DISPLAY=wayland-0 -- '')")
hasnt "local graphical session: no user@host" "$p" 'tester@box'
p=$(plain "$(render -- '')")
has "no graphical session (server console): user@host" "$p" 'tester@box '
p=$(plain "$(render WAYLAND_DISPLAY=wayland-0 SSH_CONNECTION='10.0.0.1 5 10.0.0.2 22' -- '')")
has "SSH: user@host shown" "$p" 'tester@box '
p=$(plain "$(render WAYLAND_DISPLAY=wayland-0 BASALT_PROMPT_HOST=always -- '')")
has "host = always" "$p" 'tester@box '
p=$(plain "$(render SSH_CONNECTION=x BASALT_PROMPT_HOST=never -- '')")
hasnt "host = never" "$p" 'tester@box'
p=$(render TERM=xterm USER=root BASALT_PROMPT_TEST_UID=0 WAYLAND_DISPLAY=w -- '')
has "root: user@host shown even locally" "$(plain "$p")" 'root@box '
has "root: # prompt sign" "$(plain "$p")" '# '
has "root: danger color, bold" "$p" "${esc}91m"$'\002'$'\001'"${esc}1m"$'\002'"root"
p=$(plain "$(render TERM=xterm USER=root BASALT_PROMPT_TEST_UID=0 LANG=C -- '')")
has "root in ASCII mode: # prompt sign" "$p" '# '

# --- status and jobs -----------------------------------------------------------------
p=$(plain "$(render -- '(exit 3)')")
has "exit status shown" "$p" '✗3'
p=$(plain "$(render -- '(exit 130)')")
has "signal name for 130" "$p" '✗130 INT'
p=$(plain "$(render -- 'true')")
hasnt "status hidden after success" "$p" '✗'
p=$(plain "$(render BASALT_PROMPT_SHOW_STATUS=no -- '(exit 3)')")
hasnt "show_status = no" "$p" '✗'
p=$(plain "$(render -- 'sleep 2 >/dev/null 2>&1 & sleep 2 >/dev/null 2>&1 &')")
has "background jobs counted" "$p" '&2'
p=$(plain "$(render BASALT_PROMPT_SHOW_JOBS=no -- 'sleep 2 >/dev/null 2>&1 &')")
hasnt "show_jobs = no" "$p" '&1'

# --- container and agent -------------------------------------------------------------
p=$(plain "$(render container=oci -- '')")
has "container marker" "$p" '⬢ oci'
p=$(plain "$(render container=oci LANG=C -- '')")
has "container marker, ASCII" "$p" 'ct:oci'
p=$(plain "$(render CONTAINER_ID=mybox -- '')")
has "distrobox name" "$p" '⬢ mybox'
mkdir -p "$tmp/tbx/run" && touch "$tmp/tbx/run/.toolboxenv" && printf 'engine="podman-5"\nname="fedora-toolbox-44"\n' >"$tmp/tbx/run/.containerenv"
p=$(plain "$(render BASALT_PROMPT_TEST_ROOT="$tmp/tbx" WAYLAND_DISPLAY=w -- '')")
has "toolbox: name from /run/.containerenv" "$p" '⬢ fedora-toolbox-44'
has "toolbox: user@host shown" "$p" 'tester@box'
p=$(plain "$(render container=oci BASALT_PROMPT_SHOW_CONTAINER=no -- '')")
hasnt "show_container = no" "$p" '⬢'
p=$(plain "$(render BASALT_AGENT_SESSION=s-0123456789ab -- '')")
has "basalt-agent session marker" "$p" '◆ 012345'
p=$(plain "$(render BASALT_AGENT_SESSION=s-0123456789ab BASALT_PROMPT_SHOW_AGENT=no -- '')")
hasnt "show_agent = no" "$p" '◆'
if [[ -r /proc/self/cgroup ]]; then
  p=$(plain "$(render -- '')")
  hasnt "no agent marker outside a session" "$p" '◆'
fi

# --- working directory ----------------------------------------------------------------
mkdir -p "$tmp/home/a-long-directory-name/another-long-directory/and-one-more/leaf"
p=$(plain "$(render -- "cd \"$tmp/home\"")")
has "home shown as ~" "$p" '~ '
p=$(plain "$(render -- "cd \"$tmp/home/a-long-directory-name/another-long-directory/and-one-more/leaf\"")")
# shellcheck disable=SC2088 # a literal ~ in the expected prompt
has "long path shortened" "$p" '~/…/and-one-more/leaf'
p=$(plain "$(render BASALT_PROMPT_PATH_MAX=0 -- "cd \"$tmp/home/a-long-directory-name/another-long-directory/and-one-more/leaf\"")")
# shellcheck disable=SC2088
has "path_max = 0: full path" "$p" '~/a-long-directory-name/another-long-directory/and-one-more/leaf'
evil="$tmp/home/\$(touch pwned)\\w\`id\`"
mkdir -p "$evil"
out=$(env -i HOME="$tmp/home" PATH="$PATH" TERM=xterm LANG=C.UTF-8 bash --norc --noprofile -i -c \
  '. "$1"; cd "$2"; _basalt_prompt_cmd; printf "%s" "${PS1@P}"' _ "$LIB" "$evil" 2>/dev/null)
[[ ! -e $evil/pwned && $(plain "$out") == *'$(touch pwned)\w`id`'* ]] && ok "directory names are shown, never expanded" || bad "directory names are shown, never expanded" "$out"
out=$(env -i HOME="$tmp/home" PATH="$PATH" TERM=xterm LANG=C.UTF-8 bash --norc --noprofile -i -c \
  '. "$1"; shopt -u promptvars; cd "$2"; _basalt_prompt_cmd; printf "%s" "${PS1@P}"' _ "$LIB" "$evil" 2>/dev/null)
[[ ! -e $evil/pwned && $(plain "$out") == *'$(touch pwned)\w`id`'* ]] && ok "without promptvars: shown literally" || bad "without promptvars: shown literally" "$out"

# --- configuration --------------------------------------------------------------------
mkdir -p "$tmp/home/.config/basalt"
printf '# test\nlines = 2\n host=always \n' >"$tmp/home/.config/basalt/prompt.conf"
p=$(plain "$(render WAYLAND_DISPLAY=w -- '')")
[[ $p == *$'\n❯ ' ]] && ok "user config: two lines" || bad "user config: two lines" "$p"
has "user config: host = always" "$p" 'tester@box'
p=$(plain "$(render WAYLAND_DISPLAY=w BASALT_PROMPT_LINES=1 -- '')")
[[ $p != *$'\n'* ]] && ok "BASALT_PROMPT_LINES overrides the file" || bad "env overrides file" "$p"
printf 'enable = no\n' >"$tmp/home/.config/basalt/prompt.conf"
out=$(env -i HOME="$tmp/home" PATH="$PATH" PS1='mine$ ' bash --norc --noprofile -i -c '. "$1"; _basalt_prompt_cmd; echo "$PS1"' _ "$LIB" 2>/dev/null)
[[ $out == 'mine$ ' ]] && ok "enable = no: PS1 untouched" || bad "enable = no: PS1 untouched" "$out"
rm -f "$tmp/home/.config/basalt/prompt.conf"
HOME=$tmp/home BASALT_PROMPT_LIB=$LIB "$here/basalt-prompt" off >/dev/null
grep -qx 'enable = no' "$tmp/home/.config/basalt/prompt.conf" && ok "basalt-prompt off writes enable = no" || bad "basalt-prompt off"
HOME=$tmp/home BASALT_PROMPT_LIB=$LIB "$here/basalt-prompt" on >/dev/null
[[ $(grep -c enable "$tmp/home/.config/basalt/prompt.conf") == 1 ]] && grep -qx 'enable = yes' "$tmp/home/.config/basalt/prompt.conf" &&
  ok "basalt-prompt on replaces the line" || bad "basalt-prompt on"
rm -f "$tmp/home/.config/basalt/prompt.conf"
out=$(env -i HOME="$tmp/home" PATH="$PATH" bash --norc --noprofile -i -c '. "$1"; _basalt_prompt_cmd; PS1="custom> "; _basalt_prompt_cmd; echo "$PS1"' _ "$LIB" 2>/dev/null)
[[ $out == 'custom> ' ]] && ok "a PS1 set later by the user is kept" || bad "user PS1 kept" "$out"

# --- git ----------------------------------------------------------------------------
if command -v git >/dev/null; then
  g=$tmp/home/repo
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
  gq() { git -C "$g" -c user.name=t -c user.email=t@t -c init.defaultBranch=main "$@" >/dev/null 2>&1; }
  mkdir -p "$g/sub" && gq init && echo a >"$g/a" && gq add a && gq commit -m one
  p=$(plain "$(render -- "cd \"$g/sub\"")")
  has "git: branch from a subdirectory" "$p" ' main '
  hasnt "git: clean repository has no marks" "$p" 'main*'
  echo b >>"$g/a"
  p=$(plain "$(render -- "cd \"$g\"")")
  has "git: modified file marked *" "$p" 'main*'
  gq add a
  p=$(plain "$(render -- "cd \"$g\"")")
  has "git: staged change marked +" "$p" 'main+'
  echo u >"$g/untracked"
  p=$(plain "$(render -- "cd \"$g\"")")
  hasnt "git: untracked files ignored by default" "$p" '?'
  p=$(plain "$(render BASALT_PROMPT_GIT_UNTRACKED=yes -- "cd \"$g\"")")
  has "git_untracked = yes: marked ?" "$p" 'main+?'
  gq commit -m two && rm -f "$g/untracked"
  gq checkout -b feature/x
  p=$(plain "$(render -- "cd \"$g\"")")
  has "git: branch with a slash" "$p" 'feature/x'
  sha=$(git -C "$g" rev-parse HEAD)
  gq checkout --detach
  p=$(plain "$(render -- "cd \"$g\"")")
  has "git: detached HEAD shows the commit" "$p" "@${sha:0:7}"
  gq checkout main
  gq checkout -b side && echo s >"$g/a" && gq commit -am side && gq checkout main && echo m >"$g/a" && gq commit -am main
  gq merge side
  p=$(plain "$(render -- "cd \"$g\"")")
  has "git: merge in progress" "$p" 'main|merge'
  gq merge --abort
  gq worktree add "$tmp/home/wt" -b wt
  p=$(plain "$(render -- "cd \"$tmp/home/wt\"")")
  has "git: linked worktree (.git file)" "$p" ' wt '
  p=$(plain "$(render BASALT_PROMPT_GIT=no -- "cd \"$g\"")")
  hasnt "git = no" "$p" 'main'
  # A slow git: the time box cuts it, the repository is marked ~ and not
  # asked again in this shell.
  mkdir -p "$tmp/slowbin"
  printf '#!/bin/sh\necho call >>%q\nexec sleep 3\n' "$tmp/slowgit.log" >"$tmp/slowbin/git"
  chmod +x "$tmp/slowbin/git"
  start=$(date +%s%N)
  p=$(plain "$(TEST_PATH="$tmp/slowbin:$PATH" render BASALT_PROMPT_GIT_TIMEOUT_MS=150 -- "cd \"$g\"; _basalt_prompt_cmd")")
  ms=$((($(date +%s%N) - start) / 1000000))
  has "slow git: marked ~" "$p" 'main~'
  calls=$(wc -l <"$tmp/slowgit.log")
  ((ms < 2000 && calls == 1)) && ok "slow git: time-boxed (${ms} ms for 2 prompts, 1 git call)" || bad "slow git time box" "$ms ms, $calls calls"
  if [[ -r /proc/self/mounts ]]; then
    out=$(env -i PATH="$PATH" bash --norc --noprofile -i -c '. "$1"; _bp_is_netfs /proc && echo net || echo local' _ "$LIB" 2>/dev/null)
    [[ $out == local ]] && ok "network file system check: /proc is local" || bad "netfs check" "$out"
  fi
else
  echo "skip git tests: git not installed"
fi

# --- speed --------------------------------------------------------------------------
t=$(env -i HOME="$tmp/home" PATH="$PATH" TERM=xterm-256color LANG=C.UTF-8 bash --norc --noprofile -i -c '. "$1"; cd /; _basalt_prompt_cmd
  t0=${EPOCHREALTIME/[.,]/}; for ((i = 0; i < 200; i++)); do _basalt_prompt_cmd; done; t1=${EPOCHREALTIME/[.,]/}
  echo $(((t1 - t0) / 200))' _ "$LIB" 2>/dev/null)
((t < 10000)) && ok "render time outside git: ${t} us per prompt (< 10 ms)" || bad "render time outside git" "$t us"

# --- zsh and fish -------------------------------------------------------------------
if command -v zsh >/dev/null; then
  z() { env -i HOME="$tmp/home" PATH="$PATH" TERM=xterm-256color LANG=C.UTF-8 USER=tester BASALT_PROMPT_TEST_UID=1000 \
    BASALT_PROMPT_TEST_ROOT="$tmp/fsroot" "$@" zsh -f -i -c \
    'source "$1"; (exit 2); _basalt_prompt_precmd; print -rn -- "$PROMPT"' _ "$here/basalt-prompt.zsh" 2>/dev/null; }
  p=$(z)
  has "zsh: Basalt colors" "$p" '%F{#'
  has "zsh: exit status" "$p" '✗2'
  p=$(z NO_COLOR=1)
  hasnt "zsh: NO_COLOR" "$p" '%F{'
  p=$(z BASALT_PROMPT_TEST_UID=0)
  has "zsh: root sign" "$p" '#%b'
  out=$(env -i PATH="$PATH" zsh -f -c 'source "$1"; echo "${precmd_functions-none}"' _ "$here/basalt-prompt.zsh" 2>&1)
  [[ $out == '' || $out == none ]] && ok "zsh non-interactive: no-op" || bad "zsh non-interactive: no-op" "$out"
else
  echo "skip zsh tests: zsh not installed"
fi
if command -v fish >/dev/null; then
  f() { env -i HOME="$tmp/home" PATH="$PATH" TERM=xterm-256color LANG=C.UTF-8 USER=tester BASALT_PROMPT_TEST_UID=1000 \
    BASALT_PROMPT_TEST_ROOT="$tmp/fsroot" "$@" fish --no-config -i -c \
    "source $here/basalt-prompt.fish; false; fish_prompt" 2>/dev/null; }
  p=$(f)
  has "fish: colors" "$p" "$esc"
  has "fish: exit status" "$p" '✗1'
  p=$(f NO_COLOR=1)
  hasnt "fish: NO_COLOR" "$p" "$esc"
  p=$(f BASALT_PROMPT_TEST_UID=0)
  has "fish: root sign" "$p" $'\e[1m#'
  out=$(env -i HOME="$tmp/home" PATH="$PATH" fish --no-config -c "source $here/basalt-prompt.fish; functions -q __bp_path; and echo defined; or echo none" 2>&1)
  [[ $out == none ]] && ok "fish non-interactive: no-op" || bad "fish non-interactive: no-op" "$out"
else
  echo "skip fish tests: fish not installed"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
((fail == 0))
