# shellcheck shell=bash
# Basalt OS shell prompt for interactive bash.
#
# Sourced by /etc/profile.d/basalt-prompt.sh. Pure bash (4.4 or later): the
# prompt is built by builtins only, except one time-boxed `git status` when
# the working directory is inside a git work tree on a local file system.
#
# Configuration, later lines win:
#   /etc/basalt/prompt.conf, then ~/.config/basalt/prompt.conf
#   (key = value; see basalt-prompt(1)), then BASALT_PROMPT_<KEY> variables.
# BASALT_PROMPT=off (or enable = no) leaves the prompt alone.
#
# The prompt never changes non-interactive shells or scripts: everything
# below returns before defining anything unless the shell is interactive.

[ -n "${BASH_VERSION-}" ] || return 0
case $- in *i*) ;; *) return 0 ;; esac
[ -z "${_basalt_prompt_loaded-}" ] || return 0
((BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4))) || return 0
case "${BASALT_PROMPT-}" in off | no | 0 | false) return 0 ;; esac
_basalt_prompt_loaded=1

# PS1 points at this variable; PROMPT_COMMAND rebuilds it before each prompt.
# Dynamic text (directory names, branch names) only ever reaches the
# terminal through ${_bp_ps1}: the result of a parameter expansion in PS1 is
# not expanded again, so a directory called $(cmd) is shown, never run.
_bp_ps1=''
_bp_template='${_bp_ps1}'
_bp_ready=0
_bp_off=0
declare -gA _bp_cfg=() _bp_slow=()
# Colors, set by _bp_palette (printf -v).
_bp_c_accent='' _bp_c_muted='' _bp_c_warn='' _bp_c_danger='' _bp_c_ok='' _bp_c_badge_bg='' _bp_c_badge_fg=''
_bp_c_bold='' _bp_c_reset=''

# --- configuration -------------------------------------------------------------------

_bp_defaults() {
  _bp_cfg=([enable]=yes [glyphs]=auto [color]=auto [theme]=auto [lines]=1 [host]=auto
    [path_max]=40 [git]=yes [git_dirty]=yes [git_untracked]=no [git_timeout_ms]=200
    [show_status]=yes [show_jobs]=yes [show_agent]=yes [show_container]=yes)
}

# _bp_read_conf FILE: "key = value" lines, '#' starts a comment.
_bp_read_conf() {
  [[ -r $1 && -f $1 ]] || return 0
  local line key val
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%%#*}
    [[ $line == *=* ]] || continue
    key=${line%%=*} val=${line#*=}
    key=${key//[[:space:]]/}
    val=${val#"${val%%[![:space:]]*}"}
    val=${val%"${val##*[![:space:]]}"}
    val=${val#[\"\']} val=${val%[\"\']}
    [[ -n ${_bp_cfg[$key]+set} ]] && _bp_cfg[$key]=$val
  done <"$1"
}

_bp_yes() { case "${_bp_cfg[$1]}" in yes | on | true | 1) return 0 ;; esac; return 1; }

_bp_load_conf() {
  _bp_defaults
  _bp_read_conf /etc/basalt/prompt.conf
  _bp_read_conf "${XDG_CONFIG_HOME:-$HOME/.config}/basalt/prompt.conf"
  local key var
  for key in "${!_bp_cfg[@]}"; do
    var=BASALT_PROMPT_${key^^}
    [[ -n ${!var-} ]] && _bp_cfg[$key]=${!var}
  done
  [[ ${_bp_cfg[path_max]} =~ ^[0-9]+$ ]] || _bp_cfg[path_max]=40
  [[ ${_bp_cfg[git_timeout_ms]} =~ ^[0-9]+$ ]] || _bp_cfg[git_timeout_ms]=200
}

# --- terminal capabilities -------------------------------------------------------------

# Color depth: none, 16, 256 or truecolor.
_bp_color_depth() {
  local want=${_bp_cfg[color]}
  if [[ -n ${NO_COLOR-} ]] || [[ $want == none || $want == off || $want == no ]]; then
    echo none
    return
  fi
  case $want in truecolor | 24bit | 256 | 16)
    echo "${want/24bit/truecolor}"
    return
    ;;
  esac
  case ${TERM-dumb} in dumb | '' | unknown) echo none; return ;; esac
  case ${COLORTERM-} in truecolor | 24bit) echo truecolor; return ;; esac
  case $TERM in
    foot* | xterm-kitty | alacritty* | wezterm | contour | xterm-ghostty | *-direct) echo truecolor ;;
    *256color* | screen* | tmux*) echo 256 ;;
    vt100 | vt102 | vt220 | ansi.sys) echo none ;;
    *) echo 16 ;;
  esac
}

# Glyph set: unicode when the locale is UTF-8 and the terminal is not the
# Linux console or a serial line (their fonts lack most symbols).
_bp_glyph_set() {
  case ${_bp_cfg[glyphs]} in unicode | ascii) echo "${_bp_cfg[glyphs]}"; return ;; esac
  case ${TERM-dumb} in linux | vt* | dumb | ansi* | cons25 | '') echo ascii; return ;; esac
  case "${LC_ALL:-${LC_CTYPE:-${LANG-}}}" in
    *[Uu][Tt][Ff]-8* | *[Uu][Tt][Ff]8*) echo unicode ;;
    *) echo ascii ;;
  esac
}

# _bp_lvl V: index of the xterm cube level nearest to V (0..5), in _bp_n.
_bp_lvl() { if (($1 < 48)); then _bp_n=0; elif (($1 < 115)); then _bp_n=1; else _bp_n=$((($1 - 35) / 40)); fi; }

# _bp_256 R G B: nearest xterm 256-color index (cube or gray ramp), in _bp_n.
_bp_256() {
  local r=$1 g=$2 b=$3 ri gi bi dc avg gray gv dg
  local -a lv=(0 95 135 175 215 255)
  _bp_lvl "$r"; ri=$_bp_n
  _bp_lvl "$g"; gi=$_bp_n
  _bp_lvl "$b"; bi=$_bp_n
  dc=$(((r - lv[ri]) ** 2 + (g - lv[gi]) ** 2 + (b - lv[bi]) ** 2))
  avg=$(((r + g + b) / 3))
  if ((avg > 238)); then gray=23; elif ((avg < 3)); then gray=0; else gray=$(((avg - 3) / 10)); fi
  gv=$((8 + 10 * gray))
  dg=$(((r - gv) ** 2 + (g - gv) ** 2 + (b - gv) ** 2))
  if ((dg < dc)); then _bp_n=$((232 + gray)); else _bp_n=$((16 + 36 * ri + 6 * gi + bi)); fi
}

# _bp_sgr ROLE HEX CODE16 [fg|bg]: an SGR sequence for the current depth.
_bp_sgr() {
  local hex=${2#\#} kind=${4:-fg} r g b
  r=$((16#${hex:0:2})) g=$((16#${hex:2:2})) b=$((16#${hex:4:2}))
  local base=38
  [[ $kind == bg ]] && base=48
  case $_bp_depth in
    truecolor) printf -v "_bp_c_$1" '\001\033[%s;2;%d;%d;%dm\002' "$base" "$r" "$g" "$b" ;;
    256)
      _bp_256 "$r" "$g" "$b"
      printf -v "_bp_c_$1" '\001\033[%s;5;%dm\002' "$base" "$_bp_n"
      ;;
    16) printf -v "_bp_c_$1" '\001\033[%sm\002' "$3" ;;
    *) printf -v "_bp_c_$1" '%s' '' ;;
  esac
}

# Basalt palette (basalt-shell theme tokens). The terminal background is
# usually unknown, so "auto" uses mid tones readable on both the dark
# (#14171B) and the light (#FBF9F6) background; COLORFGBG, when the
# terminal sets it, picks the exact light or dark set.
_bp_palette() {
  local theme=${_bp_cfg[theme]}
  if [[ $theme == auto && -n ${COLORFGBG-} ]]; then
    case ${COLORFGBG##*;} in 7 | 15) theme=light ;; *) theme=dark ;; esac
  fi
  local accent muted warn danger ok
  case $theme in
    dark) accent=c8623f muted=a0a6ae warn=d9a23a danger=e0605a ok=5fae7b ;;
    light) accent=a3472e muted=676c74 warn=a86b00 danger=b3261e ok=2e7d4f ;;
    *) accent=b5502f muted=8a9099 warn=b8801c danger=d0453e ok=3f9160 ;;
  esac
  _bp_sgr accent "$accent" 31
  _bp_sgr muted "$muted" 90
  _bp_sgr warn "$warn" 33
  _bp_sgr danger "$danger" 91
  _bp_sgr ok "$ok" 32
  # The agent badge: white on terra roxa in every theme.
  _bp_sgr badge_bg a3472e 41 bg
  _bp_sgr badge_fg ffffff 97
  if [[ $_bp_depth == none ]]; then
    _bp_c_bold='' _bp_c_reset=''
  else
    _bp_c_bold=$'\001\033[1m\002' _bp_c_reset=$'\001\033[0m\002'
  fi
}

# --- session facts (fixed for the life of the shell) ----------------------------------

_bp_detect_session() {
  # SSH, also through sudo when the variables were kept.
  _bp_ssh=0
  [[ -n ${SSH_CONNECTION-}${SSH_CLIENT-}${SSH_TTY-} ]] && _bp_ssh=1

  _bp_uid=${BASALT_PROMPT_TEST_UID:-$EUID}
  _bp_user=${USER:-${LOGNAME-}}
  [[ -n $_bp_user ]] || { [[ $_bp_uid == 0 ]] && _bp_user=root; }
  _bp_host=${HOSTNAME%%.*}

  # Container or toolbox: the markers podman, toolbox, docker and
  # systemd-nspawn leave inside the container. (The tests point
  # BASALT_PROMPT_TEST_ROOT at a directory of their own.)
  local line r=${BASALT_PROMPT_TEST_ROOT-}
  _bp_container=''
  if [[ -e $r/run/.toolboxenv ]]; then
    _bp_container=toolbox
  elif [[ -e $r/run/.containerenv ]]; then
    _bp_container=podman
  elif [[ -e $r/.dockerenv ]]; then
    _bp_container=docker
  elif [[ -n ${container-} ]]; then
    _bp_container=$container
  elif [[ -r $r/run/systemd/container ]]; then
    read -r _bp_container <"$r/run/systemd/container" || :
  fi
  if [[ -r $r/run/.containerenv ]]; then
    while IFS= read -r line; do
      [[ $line == name=* ]] || continue
      line=${line#name=} line=${line//\"/}
      [[ -n $line ]] && _bp_container=$line
    done <"$r/run/.containerenv"
  fi
  [[ -n ${CONTAINER_ID-} ]] && _bp_container=$CONTAINER_ID

  # basalt-agent session: the session's cgroup slice
  # (basaltagent.slice/basaltagent-HEX.slice), the agent's SELinux domain
  # (basalt_agent_*), or BASALT_AGENT_SESSION when the launcher sets it.
  _bp_agent=''
  if [[ -n ${BASALT_AGENT_SESSION-} ]]; then
    _bp_agent=${BASALT_AGENT_SESSION#s-}
  elif [[ -r /proc/self/cgroup ]]; then
    while IFS= read -r line; do
      if [[ $line =~ /basaltagent-([0-9a-f]+)\.slice ]]; then
        _bp_agent=${BASH_REMATCH[1]}
        break
      fi
    done </proc/self/cgroup
  fi
  if [[ -z $_bp_agent && -r /proc/self/attr/current ]]; then
    read -r -d '' line </proc/self/attr/current || :
    [[ $line == *:basalt_agent* ]] && _bp_agent=session
  fi

  # user@host: always over SSH, as root and in containers; hidden in a local
  # graphical session, where the window already belongs to this machine.
  case ${_bp_cfg[host]} in
    always) _bp_show_host=1 ;;
    never) _bp_show_host=0 ;;
    *)
      _bp_show_host=1
      if ((_bp_ssh == 0)) && [[ $_bp_uid != 0 && -z $_bp_container && -n ${WAYLAND_DISPLAY-}${DISPLAY-} ]]; then
        _bp_show_host=0
      fi
      ;;
  esac
  _bp_have_git=0 _bp_timeout=''
  if _bp_yes git && type -P git >/dev/null 2>&1; then
    _bp_have_git=1
    _bp_timeout=$(type -P timeout 2>/dev/null) || _bp_timeout=''
  fi
}

_bp_init() {
  _bp_load_conf
  if ! _bp_yes enable; then
    _bp_off=1
    return
  fi
  _bp_depth=$(_bp_color_depth)
  _bp_glyphs=$(_bp_glyph_set)
  _bp_palette
  if [[ $_bp_glyphs == unicode ]]; then
    _bp_g_prompt='❯' _bp_g_ell='…' _bp_g_err='✗' _bp_g_ct='⬢ ' _bp_g_agent='◆' _bp_g_up='↑' _bp_g_down='↓'
  else
    _bp_g_prompt='>' _bp_g_ell='...' _bp_g_err='x' _bp_g_ct='ct:' _bp_g_agent='agent:' _bp_g_up='^' _bp_g_down='v'
  fi
  _bp_detect_session
  _bp_ready=1
}

# --- per prompt -----------------------------------------------------------------------

# Working directory: ~ for $HOME, and only the last components when longer
# than path_max ("~/…/basalt-os/packages").
_bp_path() {
  local p=$PWD max=${_bp_cfg[path_max]}
  if [[ -n ${HOME-} && $HOME != / ]]; then
    if [[ $p == "$HOME" ]]; then
      p='~'
    elif [[ $p == "$HOME"/* ]]; then
      p="~${p:${#HOME}}"
    fi
  fi
  if ((max > 0 && ${#p} > max)); then
    local head=${p%%/*} rest tail
    rest=${p#"$head"/}
    tail=$rest
    while ((${#head} + ${#_bp_g_ell} + ${#tail} + 2 > max)) && [[ $tail == */* ]]; do
      tail=${tail#*/}
    done
    [[ $tail != "$rest" ]] && p="$head/$_bp_g_ell/$tail"
  fi
  _bp_out_path=$p
}

# Network file systems: git status there can take seconds.
_bp_is_netfs() {
  local dir=$1 mp fs best='' bestfs=''
  [[ -r /proc/self/mounts ]] || return 1
  while read -r _ mp fs _; do
    mp=${mp//\\040/ }
    if [[ $dir == "$mp" || $dir == "$mp"/* || $mp == / ]] && ((${#mp} >= ${#best})); then
      best=$mp bestfs=$fs
    fi
  done </proc/self/mounts
  case $bestfs in
    nfs* | cifs | smb* | ncpfs | afs | ceph | glusterfs | gfs2 | ocfs2 | 9p | lustre | beegfs | gpfs | \
      fuse.sshfs | fuse.rclone | fuse.s3fs | fuse.gcsfuse | fuse.davfs | davfs | fuse.glusterfs) return 0 ;;
  esac
  return 1
}

# Git: find the work tree by walking up from $PWD (builtins only), read the
# branch from HEAD, and run one `git status` for the dirty and upstream
# marks, time-boxed; a repository that hits the time box is not asked again
# in this shell, and nothing is run on network file systems.
_bp_git_find() {
  _bp_git_dir='' _bp_git_root='' _bp_git_net=0
  local d=$PWD line
  while :; do
    if [[ -d $d/.git ]]; then
      _bp_git_dir=$d/.git
      break
    elif [[ -f $d/.git ]]; then
      read -r line <"$d/.git" || :
      line=${line#gitdir: }
      [[ $line != /* ]] && line=$d/$line
      _bp_git_dir=$line
      break
    fi
    [[ -z $d || $d == / ]] && break
    d=${d%/*}
  done
  [[ -n $_bp_git_dir ]] || return
  _bp_git_root=${d:-/}
  _bp_is_netfs "$_bp_git_root" && _bp_git_net=1
  return 0
}

_bp_git() {
  _bp_out_git=''
  _bp_yes git || return
  if [[ $PWD != "${_bp_git_pwd-}" ]]; then
    _bp_git_pwd=$PWD
    _bp_git_find
  fi
  [[ -n $_bp_git_dir && -r $_bp_git_dir/HEAD ]] || return

  local head branch='' state='' marks='' common=$_bp_git_dir
  read -r head <"$_bp_git_dir/HEAD" || :
  if [[ $head == 'ref: refs/heads/'* ]]; then
    branch=${head#ref: refs/heads/}
  elif [[ $head =~ ^[0-9a-f]{7,} ]]; then
    branch="@${head:0:7}"
  else
    branch=${head#ref: }
  fi
  [[ -f $_bp_git_dir/commondir ]] && { read -r common <"$_bp_git_dir/commondir" || :; [[ $common != /* ]] && common=$_bp_git_dir/$common; }

  if [[ -d $_bp_git_dir/rebase-merge || -d $_bp_git_dir/rebase-apply ]]; then
    state=rebase
  elif [[ -f $_bp_git_dir/MERGE_HEAD ]]; then
    state=merge
  elif [[ -f $_bp_git_dir/CHERRY_PICK_HEAD ]]; then
    state=pick
  elif [[ -f $_bp_git_dir/REVERT_HEAD ]]; then
    state=revert
  elif [[ -f $_bp_git_dir/BISECT_LOG || -f $common/BISECT_LOG ]]; then
    state=bisect
  fi

  if _bp_yes git_dirty && ((_bp_have_git)); then
    if ((_bp_git_net)) || [[ -n ${_bp_slow[$_bp_git_root]-} ]]; then
      marks='~'
    else
      local out rc ms=${_bp_cfg[git_timeout_ms]} untracked=-uno
      _bp_yes git_untracked && untracked=-unormal
      local -a cmd=(git -C "$_bp_git_root" --no-optional-locks status --porcelain=v1 -b "$untracked" --ignore-submodules=dirty)
      if [[ -n $_bp_timeout ]] && ((ms > 0)); then
        cmd=("$_bp_timeout" -k 1 "$((ms / 1000)).$(printf '%03d' $((ms % 1000)))" "${cmd[@]}")
      fi
      out=$(GIT_OPTIONAL_LOCKS=0 LC_ALL=C "${cmd[@]}" 2>/dev/null)
      rc=$?
      if ((rc == 124 || rc == 137)); then
        _bp_slow[$_bp_git_root]=1
        marks='~'
      elif ((rc == 0)); then
        local l x y ahead='' behind='' staged=0 unstaged=0 untr=0
        while IFS= read -r l; do
          if [[ $l == '## '* ]]; then
            [[ $l =~ ahead\ ([0-9]+) ]] && ahead=${BASH_REMATCH[1]}
            [[ $l =~ behind\ ([0-9]+) ]] && behind=${BASH_REMATCH[1]}
            # reftable repositories keep HEAD as a stub; the status line
            # has the real branch.
            if [[ $branch == .invalid || $branch == refs/heads/.invalid ]]; then
              branch=${l#'## '} branch=${branch%%...*} branch=${branch%% *}
            fi
            continue
          fi
          x=${l:0:1} y=${l:1:1}
          if [[ $x$y == '??' ]]; then
            untr=1
            continue
          fi
          [[ $x != ' ' ]] && staged=1
          [[ $y != ' ' ]] && unstaged=1
        done <<<"$out"
        ((staged)) && marks+='+'
        ((unstaged)) && marks+='*'
        ((untr)) && marks+='?'
        [[ -n $ahead ]] && marks+="$_bp_g_up$ahead"
        [[ -n $behind ]] && marks+="$_bp_g_down$behind"
      fi
    fi
  fi
  _bp_out_git=$_bp_c_accent$branch$_bp_c_reset
  [[ -n $state ]] && _bp_out_git+="$_bp_c_warn|$state$_bp_c_reset"
  [[ -n $marks ]] && _bp_out_git+="$_bp_c_warn$marks$_bp_c_reset"
}

_bp_signal_name() {
  case $1 in
    129) _bp_out_sig=HUP ;; 130) _bp_out_sig=INT ;; 131) _bp_out_sig=QUIT ;; 134) _bp_out_sig=ABRT ;;
    137) _bp_out_sig=KILL ;; 139) _bp_out_sig=SEGV ;; 141) _bp_out_sig=PIPE ;; 143) _bp_out_sig=TERM ;;
    148) _bp_out_sig=TSTP ;; *) _bp_out_sig='' ;;
  esac
}

_basalt_prompt_cmd() {
  local st=$?
  ((_bp_off)) && return $st
  if ((!_bp_ready)); then
    _bp_init
    if ((_bp_off)); then return $st; fi
    PS1=$_bp_template
  fi
  # Someone set their own PS1 after us (~/.bashrc, a tool): keep it.
  if [[ $PS1 != "$_bp_template" && $PS1 != "${_bp_literal-}" ]]; then
    _bp_off=1
    return $st
  fi

  local s='' jobs
  if _bp_yes show_agent && [[ -n $_bp_agent ]]; then
    s+="$_bp_c_badge_bg$_bp_c_badge_fg$_bp_c_bold $_bp_g_agent"
    [[ $_bp_agent != session ]] && s+=" ${_bp_agent:0:6}"
    s+=" $_bp_c_reset "
  fi
  if _bp_yes show_container && [[ -n $_bp_container ]]; then
    s+="$_bp_c_warn$_bp_g_ct$_bp_container$_bp_c_reset "
  fi
  if ((_bp_show_host)); then
    if [[ $_bp_uid == 0 ]]; then
      s+="$_bp_c_danger$_bp_c_bold$_bp_user$_bp_c_reset$_bp_c_muted@$_bp_c_reset$_bp_c_danger$_bp_host$_bp_c_reset "
    else
      s+="$_bp_c_muted$_bp_user@$_bp_c_reset$_bp_c_accent$_bp_host$_bp_c_reset "
    fi
  fi
  _bp_path
  s+="$_bp_c_bold$_bp_out_path$_bp_c_reset"
  _bp_git
  [[ -n $_bp_out_git ]] && s+=" $_bp_out_git"
  if _bp_yes show_status && ((st != 0)); then
    _bp_signal_name "$st"
    s+=" $_bp_c_danger$_bp_g_err$st${_bp_out_sig:+ $_bp_out_sig}$_bp_c_reset"
  fi
  if _bp_yes show_jobs; then
    jobs='\j'
    jobs=${jobs@P}
    ((jobs > 0)) && s+=" $_bp_c_muted&$jobs$_bp_c_reset"
  fi
  if [[ ${_bp_cfg[lines]} == 2 ]]; then
    s+=$'\n'
  else
    s+=' '
  fi
  if [[ $_bp_uid == 0 ]]; then
    s+="$_bp_c_danger$_bp_c_bold#$_bp_c_reset "
  elif ((st != 0)); then
    s+="$_bp_c_danger$_bp_g_prompt$_bp_c_reset "
  else
    s+="$_bp_c_accent$_bp_g_prompt$_bp_c_reset "
  fi
  _bp_ps1=$s
  # Without promptvars, PS1 cannot reference the variable: use the text,
  # with backslashes doubled so prompt decoding leaves it unchanged.
  if ! shopt -q promptvars; then
    _bp_literal=${s//\\/\\\\}
    PS1=$_bp_literal
  elif [[ $PS1 != "$_bp_template" ]]; then
    PS1=$_bp_template
  fi
  return $st
}

# Run first in PROMPT_COMMAND, so the exit status it shows is the command's.
# PROMPT_COMMAND is a string or (bash 5.1 and later) an array.
# shellcheck disable=SC2128,SC2178
if ((BASH_VERSINFO[0] > 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] >= 1))); then
  if [[ $(declare -p PROMPT_COMMAND 2>/dev/null) == 'declare -a'* ]]; then
    PROMPT_COMMAND=(_basalt_prompt_cmd "${PROMPT_COMMAND[@]}")
  else
    PROMPT_COMMAND=(_basalt_prompt_cmd ${PROMPT_COMMAND:+"$PROMPT_COMMAND"})
  fi
else
  PROMPT_COMMAND="_basalt_prompt_cmd${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
fi
