# Basalt OS shell prompt: a compact, colored prompt for interactive bash
# (profile.d drop-in), with the same prompt for zsh (opt-in) and fish.
# Pure shell, no runtime dependencies; the tests run in CI
# (packages/basalt-prompt/tests/prompt-test.sh).

Name:           basalt-prompt
Version:        0.1.0
Release:        1%{?dist}
Summary:        Basalt OS shell prompt for bash, zsh and fish
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

Source0:        LICENSE
Source1:        basalt-prompt.sh
Source2:        basalt-prompt.bash
Source3:        basalt-prompt.zsh
Source4:        basalt-prompt.fish
Source5:        basalt-prompt.conf
Source6:        basalt-prompt
Source7:        basalt-prompt.1

Requires:       bash

%description
A compact, colored prompt for interactive bash shells, set from
/etc/profile.d: user and host over SSH, as root and on servers, the working
directory, the git branch with staged, modified and upstream marks, the
exit status of a failed command, background jobs, and markers for
basalt-agent sessions and containers. Colors are the Basalt palette in
truecolor, 256 or 16 colors, none with NO_COLOR; Unicode symbols on UTF-8
terminals and ASCII on the Linux console, with no special font needed.
Scripts and non-interactive shells are never changed. Configured in
/etc/basalt/prompt.conf and ~/.config/basalt/prompt.conf; basalt-prompt off
turns it off for one user. The same prompt for fish (automatic) and zsh
(basalt-prompt zsh).

%prep
%setup -q -c -T
cp -p %{sources} .

%build

%install
install -Dpm 0644 basalt-prompt.sh %{buildroot}%{_sysconfdir}/profile.d/basalt-prompt.sh
install -Dpm 0644 basalt-prompt.conf %{buildroot}%{_sysconfdir}/basalt/prompt.conf
install -Dpm 0644 basalt-prompt.bash %{buildroot}%{_datadir}/basalt-prompt/basalt-prompt.bash
install -Dpm 0644 basalt-prompt.zsh %{buildroot}%{_datadir}/basalt-prompt/basalt-prompt.zsh
install -Dpm 0644 basalt-prompt.fish %{buildroot}%{_datadir}/fish/vendor_conf.d/basalt-prompt.fish
install -Dpm 0755 basalt-prompt %{buildroot}%{_bindir}/basalt-prompt
install -Dpm 0644 basalt-prompt.1 %{buildroot}%{_mandir}/man1/basalt-prompt.1

%check
# Syntax only here; the behavior tests (tests/prompt-test.sh) run in CI.
bash -n basalt-prompt.bash
bash -n basalt-prompt
sh -n basalt-prompt.sh

%files
%license LICENSE
%config(noreplace) %{_sysconfdir}/profile.d/basalt-prompt.sh
%dir %{_sysconfdir}/basalt
%config(noreplace) %{_sysconfdir}/basalt/prompt.conf
%{_datadir}/basalt-prompt/
%dir %{_datadir}/fish
%dir %{_datadir}/fish/vendor_conf.d
%{_datadir}/fish/vendor_conf.d/basalt-prompt.fish
%{_bindir}/basalt-prompt
%{_mandir}/man1/basalt-prompt.1*

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First release: bash prompt from /etc/profile.d (interactive shells only),
  zsh and fish versions, Basalt palette with truecolor, 256, 16 and no-color
  fallbacks, Unicode and ASCII symbols, time-boxed git status, markers for
  root, SSH, containers and basalt-agent sessions, per-user configuration.
