# Basalt OS shell prompt for interactive fish (basalt-prompt(1)): the same
# segments, colors and configuration as the bash prompt. Installed in
# fish's vendor configuration directory; a prompt of the user's own
# (~/.config/fish/functions/fish_prompt.fish, fish_config) takes precedence.

status is-interactive; or exit 0
set -q __basalt_prompt_loaded; and exit 0
contains -- "$BASALT_PROMPT" off no 0 false; and exit 0
test -e $__fish_config_dir/functions/fish_prompt.fish; and exit 0
set -g __basalt_prompt_loaded 1

set -g __bp_keys enable glyphs color theme lines host path_max git git_dirty git_untracked git_timeout_ms show_status show_jobs show_agent show_container
set -g __bp_vals yes auto auto auto 1 auto 40 yes yes no 200 yes yes yes yes

function __bp_read_conf -a file
    test -f $file -a -r $file; or return 0
    while read -l line
        set line (string replace -r '#.*' '' -- $line)
        string match -q '*=*' -- $line; or continue
        set -l kv (string split -m 1 '=' -- $line)
        set -l key (string trim -- $kv[1])
        set -l val (string trim -c ' \t"\'' -- $kv[2])
        set -l i (contains -i -- $key $__bp_keys); and set __bp_vals[$i] $val
    end <$file
end
__bp_read_conf /etc/basalt/prompt.conf
set -q XDG_CONFIG_HOME; and __bp_read_conf $XDG_CONFIG_HOME/basalt/prompt.conf; or __bp_read_conf ~/.config/basalt/prompt.conf
for i in (seq (count $__bp_keys))
    set -l var BASALT_PROMPT_(string upper -- $__bp_keys[$i])
    set -q $var; and test -n "$$var"; and set __bp_vals[$i] $$var
end

function __bp_get -a key
    echo $__bp_vals[(contains -i -- $key $__bp_keys)]
end
function __bp_yes -a key
    contains -- (__bp_get $key) yes on true 1
end

if not __bp_yes enable
    functions -e __bp_read_conf __bp_get __bp_yes
    exit 0
end

# Colors: fish's set_color takes #RRGGBB and maps it to 256 colors when the
# terminal lacks truecolor; 16 colors and none are handled here.
set -l depth (__bp_get color)
if set -q NO_COLOR; and test -n "$NO_COLOR"; or contains -- $depth none off no
    set depth none
else if not contains -- $depth truecolor 256 16
    switch "$TERM"
        case dumb '' unknown vt100 vt102 vt220
            set depth none
        case 'foot*' xterm-kitty 'alacritty*' wezterm contour xterm-ghostty '*-direct'
            set depth truecolor
        case '*256color*' 'screen*' 'tmux*'
            set depth 256
        case '*'
            contains -- "$COLORTERM" truecolor 24bit; and set depth truecolor; or set depth 16
    end
end
test $depth = truecolor; and set -g fish_term24bit 1
set -l theme (__bp_get theme)
if test $theme = auto; and set -q COLORFGBG
    contains -- (string split ';' -- $COLORFGBG)[-1] 7 15; and set theme light; or set theme dark
end
switch $theme
    case dark
        set -g __bp_hex c8623f a0a6ae d9a23a e0605a
    case light
        set -g __bp_hex a3472e 676c74 a86b00 b3261e
    case '*'
        set -g __bp_hex b5502f 8a9099 b8801c d0453e
end
if test $depth = none
    set -g __bp_c_accent ''
    set -g __bp_c_muted ''
    set -g __bp_c_warn ''
    set -g __bp_c_danger ''
    set -g __bp_c_badge ''
    set -g __bp_c_bold ''
    set -g __bp_c_reset ''
else if test $depth = 16
    set -g __bp_c_accent (set_color red)
    set -g __bp_c_muted (set_color brblack)
    set -g __bp_c_warn (set_color yellow)
    set -g __bp_c_danger (set_color brred)
    set -g __bp_c_badge (set_color -o -b red brwhite)
    set -g __bp_c_bold (set_color -o)
    set -g __bp_c_reset (set_color normal)
else
    set -g __bp_c_accent (set_color $__bp_hex[1])
    set -g __bp_c_muted (set_color $__bp_hex[2])
    set -g __bp_c_warn (set_color $__bp_hex[3])
    set -g __bp_c_danger (set_color $__bp_hex[4])
    set -g __bp_c_badge (set_color -o -b a3472e ffffff)
    set -g __bp_c_bold (set_color -o)
    set -g __bp_c_reset (set_color normal)
end

set -l glyphs (__bp_get glyphs)
if not contains -- $glyphs unicode ascii
    set glyphs ascii
    if not string match -q -r '^(linux|vt.*|dumb|ansi.*|cons25|)$' -- "$TERM"
        set -l loc $LC_ALL
        test -z "$loc"; and set loc $LC_CTYPE
        test -z "$loc"; and set loc $LANG
        string match -q -i -r 'utf-?8' -- "$loc"; and set glyphs unicode
    end
end
if test $glyphs = unicode
    set -g __bp_g '❯' '…' '✗' '⬢ ' '◆' '↑' '↓'
else
    set -g __bp_g '>' '...' x 'ct:' 'agent:' '^' v
end

# Session facts.
set -g __bp_uid (set -q BASALT_PROMPT_TEST_UID; and echo $BASALT_PROMPT_TEST_UID; or id -u)
set -g __bp_ssh 0
set -q SSH_CONNECTION; or set -q SSH_CLIENT; or set -q SSH_TTY; and set __bp_ssh 1
set -g __bp_container ''
set -l r "$BASALT_PROMPT_TEST_ROOT"
if test -e $r/run/.toolboxenv
    set __bp_container toolbox
else if test -e $r/run/.containerenv
    set __bp_container podman
else if test -e $r/.dockerenv
    set __bp_container docker
else if set -q container
    set __bp_container $container
else if test -r $r/run/systemd/container
    read __bp_container <$r/run/systemd/container
end
if test -r $r/run/.containerenv
    set -l name (string match -r -g '^name="?([^"]*)"?$' <$r/run/.containerenv)
    test -n "$name"; and set __bp_container $name
end
set -q CONTAINER_ID; and set __bp_container $CONTAINER_ID
set -g __bp_agent ''
if set -q BASALT_AGENT_SESSION
    set __bp_agent (string replace -r '^s-' '' -- $BASALT_AGENT_SESSION)
else if test -r /proc/self/cgroup
    set -l id (string match -r -g '/basaltagent-([0-9a-f]+)\.slice' </proc/self/cgroup)[1]
    test -n "$id"; and set __bp_agent $id
end
if test -z "$__bp_agent" -a -r /proc/self/attr/current
    string match -q '*:basalt_agent*' -- (string split0 </proc/self/attr/current); and set __bp_agent session
end
set -g __bp_show_host 1
switch (__bp_get host)
    case never
        set __bp_show_host 0
    case always
    case '*'
        if test $__bp_ssh = 0 -a "$__bp_uid" != 0 -a -z "$__bp_container"
            set -q WAYLAND_DISPLAY; or set -q DISPLAY; and set __bp_show_host 0
        end
end
set -g __bp_have_git 0
__bp_yes git; and command -q git; and set __bp_have_git 1
set -g __bp_timeout (command -s timeout)
set -g __bp_slow

function __bp_path
    set -l p $PWD
    if test -n "$HOME" -a "$HOME" != /
        if test "$p" = "$HOME"
            set p '~'
        else if string match -q -- "$HOME/*" $p
            set p '~'(string sub -s (math (string length -- $HOME) + 1) -- $p)
        end
    end
    set -l max (__bp_get path_max)
    if string match -q -r '^[0-9]+$' -- $max; and test $max -gt 0 -a (string length -- $p) -gt $max
        set -l parts (string split / -- $p)
        set -l head $parts[1]
        set -l tail $parts[-1]
        set -l i (math (count $parts) - 1)
        while test $i -gt 1
            set -l cand $parts[$i]/$tail
            test (math (string length -- $head) + (string length -- $__bp_g[2]) + (string length -- $cand) + 2) -le $max; or break
            set tail $cand
            set i (math $i - 1)
        end
        test $i -gt 1; and set p $head/$__bp_g[2]/$tail
    end
    echo -n $p
end

function __bp_netfs -a dir
    set -l best ''
    set -l bestfs ''
    while read -l dev mp fs rest
        set mp (string replace -a '\\040' ' ' -- $mp)
        if test "$dir" = "$mp"; or string match -q -- "$mp/*" $dir; or test "$mp" = /
            if test (string length -- $mp) -ge (string length -- $best)
                set best $mp
                set bestfs $fs
            end
        end
    end </proc/self/mounts
    string match -q -r '^(nfs.*|cifs|smb.*|ncpfs|afs|ceph|glusterfs|gfs2|ocfs2|9p|lustre|beegfs|gpfs|fuse\.(sshfs|rclone|s3fs|gcsfuse|davfs|glusterfs)|davfs)$' -- $bestfs
end

function __bp_git
    __bp_yes git; or return
    set -l d $PWD
    set -l gitdir ''
    while true
        if test -d $d/.git
            set gitdir $d/.git
            break
        else if test -f $d/.git
            read -l line <$d/.git
            set line (string replace 'gitdir: ' '' -- $line)
            string match -q '/*' -- $line; or set line $d/$line
            set gitdir $line
            break
        end
        test -z "$d" -o "$d" = /; and break
        set d (string replace -r '/[^/]*$' '' -- $d)
    end
    test -n "$gitdir" -a -r "$gitdir/HEAD"; or return
    test -n "$d"; or set d /
    read -l head <$gitdir/HEAD
    set -l branch
    if string match -q 'ref: refs/heads/*' -- $head
        set branch (string replace 'ref: refs/heads/' '' -- $head)
    else if string match -q -r '^[0-9a-f]{7,}' -- $head
        set branch @(string sub -l 7 -- $head)
    else
        set branch (string replace 'ref: ' '' -- $head)
    end
    set -l state ''
    if test -d $gitdir/rebase-merge -o -d $gitdir/rebase-apply
        set state rebase
    else if test -f $gitdir/MERGE_HEAD
        set state merge
    else if test -f $gitdir/CHERRY_PICK_HEAD
        set state pick
    else if test -f $gitdir/REVERT_HEAD
        set state revert
    else if test -f $gitdir/BISECT_LOG
        set state bisect
    end
    set -l marks ''
    if __bp_yes git_dirty; and test $__bp_have_git = 1
        if contains -- $d $__bp_slow; or __bp_netfs $d
            set marks '~'
        else
            set -l u -uno
            __bp_yes git_untracked; and set u -unormal
            set -l cmd git -C $d --no-optional-locks status --porcelain=v1 -b $u --ignore-submodules=dirty
            set -l ms (__bp_get git_timeout_ms)
            if test -n "$__bp_timeout"; and string match -q -r '^[1-9][0-9]*$' -- $ms
                set cmd $__bp_timeout -k 1 (math -s3 $ms / 1000) $cmd
            end
            set -l out (env GIT_OPTIONAL_LOCKS=0 LC_ALL=C $cmd 2>/dev/null)
            set -l rc $status
            if test $rc -eq 124 -o $rc -eq 137
                set -ga __bp_slow $d
                set marks '~'
            else if test $rc -eq 0
                set -l s 0
                set -l w 0
                set -l q 0
                for l in $out
                    if string match -q '## *' -- $l
                        set -l a (string match -r -g 'ahead ([0-9]+)' -- $l)
                        set -l b (string match -r -g 'behind ([0-9]+)' -- $l)
                        test -n "$a"; and set -f ahead $a
                        test -n "$b"; and set -f behind $b
                        continue
                    end
                    if string match -q '\?\?*' -- $l
                        set q 1
                        continue
                    end
                    string match -q -r '^[^ ]' -- $l; and set s 1
                    string match -q -r '^.[^ ]' -- $l; and set w 1
                end
                test $s = 1; and set marks "$marks+"
                test $w = 1; and set marks "$marks*"
                test $q = 1; and set marks "$marks?"
                set -q ahead; and set marks "$marks$__bp_g[6]$ahead"
                set -q behind; and set marks "$marks$__bp_g[7]$behind"
            end
        end
    end
    echo -n "$__bp_c_accent$branch$__bp_c_reset"
    test -n "$state"; and echo -n "$__bp_c_warn|$state$__bp_c_reset"
    test -n "$marks"; and echo -n "$__bp_c_warn$marks$__bp_c_reset"
end

function fish_prompt
    set -l st $status
    set -l s ''
    if __bp_yes show_agent; and test -n "$__bp_agent"
        set s "$__bp_c_badge $__bp_g[5]"
        test "$__bp_agent" != session; and set s "$s "(string sub -l 6 -- $__bp_agent)
        set s "$s $__bp_c_reset "
    end
    if __bp_yes show_container; and test -n "$__bp_container"
        set s "$s$__bp_c_warn$__bp_g[4]$__bp_container$__bp_c_reset "
    end
    if test $__bp_show_host = 1
        set -l host (prompt_hostname)
        if test "$__bp_uid" = 0
            set s "$s$__bp_c_danger$__bp_c_bold$USER$__bp_c_reset$__bp_c_muted@$__bp_c_reset$__bp_c_danger$host$__bp_c_reset "
        else
            set s "$s$__bp_c_muted$USER@$__bp_c_reset$__bp_c_accent$host$__bp_c_reset "
        end
    end
    set -l path (__bp_path)
    set s "$s$__bp_c_bold$path$__bp_c_reset"
    set -l git (__bp_git)
    test -n "$git"; and set s "$s $git"
    if __bp_yes show_status; and test $st -ne 0
        set -l sig ''
        switch $st
            case 129; set sig HUP
            case 130; set sig INT
            case 131; set sig QUIT
            case 134; set sig ABRT
            case 137; set sig KILL
            case 139; set sig SEGV
            case 141; set sig PIPE
            case 143; set sig TERM
        end
        test -n "$sig"; and set sig " $sig"
        set s "$s $__bp_c_danger$__bp_g[3]$st$sig$__bp_c_reset"
    end
    if __bp_yes show_jobs
        set -l n (count (jobs -p))
        test $n -gt 0; and set s "$s $__bp_c_muted&$n$__bp_c_reset"
    end
    if test (__bp_get lines) = 2
        set s "$s"\n
    else
        set s "$s "
    end
    if test "$__bp_uid" = 0
        set s "$s$__bp_c_danger$__bp_c_bold#$__bp_c_reset "
    else if test $st -ne 0
        set s "$s$__bp_c_danger$__bp_g[1]$__bp_c_reset "
    else
        set s "$s$__bp_c_accent$__bp_g[1]$__bp_c_reset "
    end
    echo -n $s
end
