# shellcheck shell=sh
# /etc/profile.d/basalt-prompt.sh: the Basalt OS prompt for interactive bash.
# Scripts, `bash -c` and other shells are left alone. To turn it off for
# one user: basalt-prompt off (or enable = no in ~/.config/basalt/prompt.conf).
if [ -n "${BASH_VERSION-}" ]; then
  case $- in
    *i*) [ -r /usr/share/basalt-prompt/basalt-prompt.bash ] && . /usr/share/basalt-prompt/basalt-prompt.bash ;;
  esac
fi
