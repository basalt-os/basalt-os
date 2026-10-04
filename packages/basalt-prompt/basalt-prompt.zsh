# Basalt OS shell prompt for interactive zsh: the same segments, colors and
# configuration as the bash prompt (basalt-prompt(1)), in plain zsh.
#
# zsh has no system drop-in directory for interactive shells, so this file
# is opt-in: `basalt-prompt zsh` adds this line to ~/.zshrc:
#   [[ -r /usr/share/basalt-prompt/basalt-prompt.zsh ]] && source /usr/share/basalt-prompt/basalt-prompt.zsh

[[ -o interactive ]] || return 0
[[ -z ${_basalt_prompt_loaded-} ]] || return 0
case ${BASALT_PROMPT-} in off | no | 0 | false) return 0 ;; esac
typeset -g _basalt_prompt_loaded=1

typeset -gA _bpz_cfg _bpz_c _bpz_slow
_bpz_cfg=(enable yes glyphs auto color auto theme auto lines 1 host auto path_max 40 git yes git_dirty yes
  git_untracked no git_timeout_ms 200 show_status yes show_jobs yes show_agent yes show_container yes)

_bpz_read_conf() {
  emulate -L zsh -o extended_glob
  [[ -r $1 && -f $1 ]] || return 0
  local line key val
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%%\#*}
    [[ $line == *=* ]] || continue
    key=${${line%%=*}//[[:space:]]/}
    val=${${line#*=}##[[:space:]]#}
    val=${val%%[[:space:]]#}
    val=${${val#[\"\']}%[\"\']}
    (( ${+_bpz_cfg[$key]} )) && _bpz_cfg[$key]=$val
  done <"$1"
}
_bpz_read_conf /etc/basalt/prompt.conf
_bpz_read_conf ${XDG_CONFIG_HOME:-$HOME/.config}/basalt/prompt.conf
() {
  emulate -L zsh
  local key var
  for key in ${(k)_bpz_cfg}; do
    var=BASALT_PROMPT_${(U)key}
    [[ -n ${(P)var-} ]] && _bpz_cfg[$key]=${(P)var}
  done
}
_bpz_yes() { emulate -L zsh; [[ ${_bpz_cfg[$1]} == (yes|on|true|1) ]] }
if ! _bpz_yes enable; then
  unfunction _bpz_read_conf _bpz_yes
  return 0
fi

# Colors and symbols (same rules as the bash prompt).
() {
  emulate -L zsh -o extended_glob
  local depth=${_bpz_cfg[color]} theme=${_bpz_cfg[theme]}
  if [[ -n ${NO_COLOR-} || $depth == (none|off|no) ]]; then
    depth=none
  elif [[ $depth != (truecolor|256|16) ]]; then
    case ${TERM-dumb} in
      dumb | '' | unknown | vt100 | vt102 | vt220) depth=none ;;
      *)
        if [[ ${COLORTERM-} == (truecolor|24bit) || $TERM == (foot*|xterm-kitty|alacritty*|wezterm|contour|xterm-ghostty|*-direct) ]]; then
          depth=truecolor
        elif [[ $TERM == (*256color*|screen*|tmux*) ]]; then
          depth=256
        else
          depth=16
        fi
        ;;
    esac
  fi
  if [[ $theme == auto && -n ${COLORFGBG-} ]]; then
    [[ ${COLORFGBG##*;} == (7|15) ]] && theme=light || theme=dark
  fi
  local -A hex
  case $theme in
    dark) hex=(accent c8623f muted a0a6ae warn d9a23a danger e0605a) ;;
    light) hex=(accent a3472e muted 676c74 warn a86b00 danger b3261e) ;;
    *) hex=(accent b5502f muted 8a9099 warn b8801c danger d0453e) ;;
  esac
  local -A c16=(accent 1 muted 8 warn 3 danger 9)
  local role
  # zsh/nearcolor maps #RRGGBB to the nearest of 256 colors.
  [[ $depth == 256 ]] && zmodload zsh/nearcolor 2>/dev/null
  for role in accent muted warn danger; do
    case $depth in
      truecolor | 256) _bpz_c[$role]="%F{#${hex[$role]}}" ;;
      16) _bpz_c[$role]="%F{${c16[$role]}}" ;;
      *) _bpz_c[$role]='' ;;
    esac
  done
  if [[ $depth == none ]]; then
    _bpz_c[badge]='' _bpz_c[bold]='' _bpz_c[reset]=''
  else
    case $depth in
      16) _bpz_c[badge]='%K{1}%F{15}%B' ;;
      *) _bpz_c[badge]='%K{#a3472e}%F{#ffffff}%B' ;;
    esac
    _bpz_c[bold]='%B' _bpz_c[reset]='%b%f%k'
  fi
  local glyphs=${_bpz_cfg[glyphs]}
  if [[ $glyphs != (unicode|ascii) ]]; then
    if [[ ${TERM-dumb} == (linux|vt*|dumb|ansi*|cons25|) ]]; then
      glyphs=ascii
    elif [[ ${LC_ALL:-${LC_CTYPE:-${LANG-}}} == (#i)*utf(-|)8* ]]; then
      glyphs=unicode
    else
      glyphs=ascii
    fi
  fi
  typeset -gA _bpz_g
  if [[ $glyphs == unicode ]]; then
    _bpz_g=(prompt '❯' ell '…' err '✗' ct '⬢ ' agent '◆' up '↑' down '↓')
  else
    _bpz_g=(prompt '>' ell '...' err 'x' ct 'ct:' agent 'agent:' up '^' down 'v')
  fi
}

# Session facts.
() {
  emulate -L zsh
  local line
  typeset -g _bpz_ssh=0 _bpz_container='' _bpz_agent='' _bpz_show_host=1 _bpz_uid=${BASALT_PROMPT_TEST_UID:-$EUID}
  [[ -n ${SSH_CONNECTION-}${SSH_CLIENT-}${SSH_TTY-} ]] && _bpz_ssh=1
  local r=${BASALT_PROMPT_TEST_ROOT-}
  if [[ -e $r/run/.toolboxenv ]]; then _bpz_container=toolbox
  elif [[ -e $r/run/.containerenv ]]; then _bpz_container=podman
  elif [[ -e $r/.dockerenv ]]; then _bpz_container=docker
  elif [[ -n ${container-} ]]; then _bpz_container=$container
  elif [[ -r $r/run/systemd/container ]]; then read -r _bpz_container <$r/run/systemd/container
  fi
  if [[ -r $r/run/.containerenv ]]; then
    while IFS= read -r line; do
      [[ $line == name=* ]] && line=${${line#name=}//\"/} && [[ -n $line ]] && _bpz_container=$line
    done <$r/run/.containerenv
  fi
  [[ -n ${CONTAINER_ID-} ]] && _bpz_container=$CONTAINER_ID
  if [[ -n ${BASALT_AGENT_SESSION-} ]]; then
    _bpz_agent=${BASALT_AGENT_SESSION#s-}
  elif [[ -r /proc/self/cgroup ]]; then
    while IFS= read -r line; do
      if [[ $line =~ '/basaltagent-([0-9a-f]+)\.slice' ]]; then _bpz_agent=$match[1]; break; fi
    done </proc/self/cgroup
  fi
  if [[ -z $_bpz_agent && -r /proc/self/attr/current ]]; then
    read -r -d '' line </proc/self/attr/current
    [[ $line == *:basalt_agent* ]] && _bpz_agent=session
  fi
  case ${_bpz_cfg[host]} in
    always) _bpz_show_host=1 ;;
    never) _bpz_show_host=0 ;;
    *) ((_bpz_ssh == 0)) && [[ $_bpz_uid != 0 && -z $_bpz_container && -n ${WAYLAND_DISPLAY-}${DISPLAY-} ]] && _bpz_show_host=0 ;;
  esac
  typeset -g _bpz_have_git=0 _bpz_timeout=''
  if _bpz_yes git && (( ${+commands[git]} )); then
    _bpz_have_git=1
    (( ${+commands[timeout]} )) && _bpz_timeout=${commands[timeout]}
  fi
}

# _bpz_esc TEXT: TEXT with % doubled, safe inside a prompt.
_bpz_esc() { REPLY=${1//\%/%%} }

_bpz_path() {
  emulate -L zsh
  local p=$PWD max=${_bpz_cfg[path_max]}
  [[ -n ${HOME-} && $HOME != / ]] && { [[ $p == $HOME ]] && p='~' || [[ $p == $HOME/* ]] && p="~${p#$HOME}"; }
  if [[ $max == <-> ]] && ((max > 0 && ${#p} > max)); then
    local head=${p%%/*} rest tail
    rest=${p#$head/} tail=$rest
    while ((${#head} + ${#_bpz_g[ell]} + ${#tail} + 2 > max)) && [[ $tail == */* ]]; do tail=${tail#*/}; done
    [[ $tail != $rest ]] && p="$head/${_bpz_g[ell]}/$tail"
  fi
  REPLY=$p
}

_bpz_netfs() {
  emulate -L zsh
  local dir=$1 dev mp fs rest best='' bestfs=''
  [[ -r /proc/self/mounts ]] || return 1
  while read -r dev mp fs rest; do
    mp=${mp//\\040/ }
    if [[ $dir == $mp || $dir == $mp/* || $mp == / ]] && ((${#mp} >= ${#best})); then best=$mp bestfs=$fs; fi
  done </proc/self/mounts
  [[ $bestfs == (nfs*|cifs|smb*|ncpfs|afs|ceph|glusterfs|gfs2|ocfs2|9p|lustre|beegfs|gpfs|fuse.sshfs|fuse.rclone|fuse.s3fs|fuse.gcsfuse|fuse.davfs|davfs|fuse.glusterfs) ]]
}

_bpz_git() {
  emulate -L zsh
  REPLY=''
  _bpz_yes git || return
  local d=$PWD gitdir='' line
  while :; do
    if [[ -d $d/.git ]]; then gitdir=$d/.git; break
    elif [[ -f $d/.git ]]; then
      read -r line <"$d/.git"; line=${line#gitdir: }; [[ $line != /* ]] && line=$d/$line; gitdir=$line; break
    fi
    [[ -z $d || $d == / ]] && break
    d=${d%/*}
  done
  [[ -n $gitdir && -r $gitdir/HEAD ]] || return
  local root=${d:-/} head branch state='' marks=''
  read -r head <"$gitdir/HEAD"
  if [[ $head == 'ref: refs/heads/'* ]]; then branch=${head#ref: refs/heads/}
  elif [[ $head =~ '^[0-9a-f]{7,}' ]]; then branch="@${head[1,7]}"
  else branch=${head#ref: }; fi
  if [[ -d $gitdir/rebase-merge || -d $gitdir/rebase-apply ]]; then state=rebase
  elif [[ -f $gitdir/MERGE_HEAD ]]; then state=merge
  elif [[ -f $gitdir/CHERRY_PICK_HEAD ]]; then state=pick
  elif [[ -f $gitdir/REVERT_HEAD ]]; then state=revert
  elif [[ -f $gitdir/BISECT_LOG ]]; then state=bisect; fi
  if _bpz_yes git_dirty && ((_bpz_have_git)); then
    if [[ -n ${_bpz_slow[$root]-} ]] || _bpz_netfs $root; then
      marks='~'
    else
      local ms=${_bpz_cfg[git_timeout_ms]} out rc u=-uno
      _bpz_yes git_untracked && u=-unormal
      local -a cmd=(git -C $root --no-optional-locks status --porcelain=v1 -b $u --ignore-submodules=dirty)
      [[ -n $_bpz_timeout && $ms == <1-> ]] && cmd=($_bpz_timeout -k 1 $((ms / 1000)).${(l:3::0:)$((ms % 1000))} $cmd)
      out=$(GIT_OPTIONAL_LOCKS=0 LC_ALL=C $cmd 2>/dev/null)
      rc=$?
      if ((rc == 124 || rc == 137)); then
        _bpz_slow[$root]=1 marks='~'
      elif ((rc == 0)); then
        local l s=0 w=0 q=0 ahead='' behind=''
        for l in ${(f)out}; do
          if [[ $l == '## '* ]]; then
            [[ $l =~ 'ahead ([0-9]+)' ]] && ahead=$match[1]
            [[ $l =~ 'behind ([0-9]+)' ]] && behind=$match[1]
            continue
          fi
          [[ ${l[1,2]} == '??' ]] && { q=1; continue; }
          [[ ${l[1]} != ' ' ]] && s=1
          [[ ${l[2]} != ' ' ]] && w=1
        done
        ((s)) && marks+='+'
        ((w)) && marks+='*'
        ((q)) && marks+='?'
        [[ -n $ahead ]] && marks+="${_bpz_g[up]}$ahead"
        [[ -n $behind ]] && marks+="${_bpz_g[down]}$behind"
      fi
    fi
  fi
  _bpz_esc $branch
  local out="${_bpz_c[accent]}$REPLY${_bpz_c[reset]}"
  [[ -n $state ]] && out+="${_bpz_c[warn]}|$state${_bpz_c[reset]}"
  [[ -n $marks ]] && out+="${_bpz_c[warn]}$marks${_bpz_c[reset]}"
  REPLY=$out
}

_basalt_prompt_precmd() {
  local st=$? subst=0
  [[ -o prompt_subst ]] && subst=1
  # Someone set their own prompt after us (~/.zshrc, a theme): keep it.
  if [[ -n ${_bpz_last-} && $PROMPT != $_bpz_last ]]; then
    precmd_functions=(${precmd_functions:#_basalt_prompt_precmd})
    return $st
  fi
  emulate -L zsh
  local s='' sig=''
  local -A C=("${(@kv)_bpz_c}")
  if _bpz_yes show_agent && [[ -n $_bpz_agent ]]; then
    s+="${C[badge]} ${_bpz_g[agent]}"
    [[ $_bpz_agent != session ]] && s+=" ${_bpz_agent[1,6]}"
    s+=" ${C[reset]} "
  fi
  if _bpz_yes show_container && [[ -n $_bpz_container ]]; then
    _bpz_esc $_bpz_container
    s+="${C[warn]}${_bpz_g[ct]}$REPLY${C[reset]} "
  fi
  if ((_bpz_show_host)); then
    if [[ $_bpz_uid == 0 ]]; then
      s+="${C[danger]}${C[bold]}%n${C[reset]}${C[muted]}@${C[reset]}${C[danger]}%m${C[reset]} "
    else
      s+="${C[muted]}%n@${C[reset]}${C[accent]}%m${C[reset]} "
    fi
  fi
  _bpz_path
  _bpz_esc $REPLY
  s+="${C[bold]}$REPLY${C[reset]}"
  _bpz_git
  [[ -n $REPLY ]] && s+=" $REPLY"
  if _bpz_yes show_status && ((st != 0)); then
    case $st in
      129) sig=HUP ;; 130) sig=INT ;; 131) sig=QUIT ;; 134) sig=ABRT ;; 137) sig=KILL ;;
      139) sig=SEGV ;; 141) sig=PIPE ;; 143) sig=TERM ;; 148) sig=TSTP ;;
    esac
    s+=" ${C[danger]}${_bpz_g[err]}$st${sig:+ $sig}${C[reset]}"
  fi
  if _bpz_yes show_jobs && ((${#jobstates} > 0)); then
    s+=" ${C[muted]}&${#jobstates}${C[reset]}"
  fi
  [[ ${_bpz_cfg[lines]} == 2 ]] && s+=$'\n' || s+=' '
  if [[ $_bpz_uid == 0 ]]; then
    s+="${C[danger]}${C[bold]}#${C[reset]} "
  elif ((st != 0)); then
    s+="${C[danger]}${_bpz_g[prompt]}${C[reset]} "
  else
    s+="${C[accent]}${_bpz_g[prompt]}${C[reset]} "
  fi
  # Dynamic text was %-escaped; with prompt_subst set the variable's value
  # is not expanded again, so a directory called $(cmd) is only shown.
  if ((subst)); then
    typeset -g _bpz_ps1=$s
    PROMPT='${_bpz_ps1}'
  else
    PROMPT=$s
  fi
  typeset -g _bpz_last=$PROMPT
  return $st
}

typeset -ga precmd_functions
precmd_functions=(_basalt_prompt_precmd ${precmd_functions:#_basalt_prompt_precmd})
