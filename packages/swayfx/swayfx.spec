# SwayFX for Basalt OS: sway with rounded corners, shadows, blur and
# dimming of inactive windows (scenefx), built from upstream against
# Fedora's wlroots and scenefx. The Basalt desktop session runs on it; the
# shell turns the effects off on weak hardware.
#
# Same binaries, paths and IPC as sway, so it takes sway's place: it
# Provides sway (packages that require sway, like basalt-shell, keep
# working) and Conflicts with Fedora's sway. No Obsoletes on purpose: a
# system that has Fedora's sway keeps it on dnf upgrade, and Fedora's sway
# never replaces an installed swayfx; switching is explicit, in either
# direction (dnf swap sway swayfx, dnf swap swayfx sway). The desktop
# edition installs swayfx directly.
%global tag 0.6
# The sway release this SwayFX version is based on.
%global sway_base 1.11

Name:           swayfx
Version:        %{tag}
Release:        1%{?dist}
Summary:        Sway with eye candy: rounded corners, shadows, blur, dimming
License:        MIT
URL:            https://github.com/WillPower3309/swayfx
Source0:        %{url}/archive/refs/tags/%{tag}/%{name}-%{tag}.tar.gz

BuildRequires:  gcc
BuildRequires:  meson >= 1.3
BuildRequires:  pkgconfig(cairo)
BuildRequires:  pkgconfig(gdk-pixbuf-2.0)
BuildRequires:  pkgconfig(json-c) >= 0.13
BuildRequires:  pkgconfig(libdrm)
BuildRequires:  pkgconfig(libevdev)
BuildRequires:  pkgconfig(libinput) >= 1.26.0
BuildRequires:  pkgconfig(libpcre2-8)
BuildRequires:  pkgconfig(libsystemd) >= 239
BuildRequires:  pkgconfig(libudev)
BuildRequires:  pkgconfig(pango)
BuildRequires:  pkgconfig(pangocairo)
BuildRequires:  pkgconfig(pixman-1)
BuildRequires:  pkgconfig(scdoc)
BuildRequires:  pkgconfig(scenefx-0.5)
BuildRequires:  pkgconfig(wayland-client)
BuildRequires:  pkgconfig(wayland-cursor)
BuildRequires:  pkgconfig(wayland-server) >= 1.21.0
BuildRequires:  pkgconfig(wayland-protocols) >= 1.41
BuildRequires:  pkgconfig(wlroots-0.20)
BuildRequires:  pkgconfig(xcb)
BuildRequires:  pkgconfig(xcb-icccm)
BuildRequires:  pkgconfig(xkbcommon) >= 1.5.0

Provides:       sway = %{sway_base}-%{release}
Conflicts:      sway
# A session needs a GPU driver stack and polkit; X11 apps need XWayland.
Requires:       mesa-dri-drivers
Requires:       polkit
Recommends:     xorg-x11-server-Xwayland
Recommends:     (qt6-qtwayland if qt6-qtbase-gui)
Recommends:     (qt5-qtwayland if qt5-qtbase-gui)

%description
SwayFX is a fork of the sway Wayland compositor (i3-compatible) that adds
rounded corners, window shadows, background blur and dimming of inactive
windows, rendered with the scenefx library. Configuration and IPC are
sway's, plus the SwayFX commands (corner_radius, shadows, blur,
default_dim_inactive and others). It replaces the sway package.

%prep
%autosetup -n %{name}-%{tag}

%build
%meson \
    -Dsd-bus-provider=libsystemd \
    -Dwerror=false \
    -Ddefault-wallpaper=false
%meson_build

%install
%meson_install
install -d -m755 %{buildroot}%{_sysconfdir}/sway/config.d
# Fedora's sway packages its session file and default config in
# sway-config-upstream; the Basalt session has its own, so leave the
# generic "Sway" entry out of the login screen.
rm -f %{buildroot}%{_datadir}/wayland-sessions/sway.desktop

%files
%license LICENSE
%doc README.md
%dir %{_sysconfdir}/sway
%dir %{_sysconfdir}/sway/config.d
%config(noreplace) %{_sysconfdir}/sway/config
%{_mandir}/man1/sway*
%{_mandir}/man5/*
%{_mandir}/man7/*
%caps(cap_sys_nice=ep) %{_bindir}/sway
%{_bindir}/swaybar
%{_bindir}/swaymsg
%{_bindir}/swaynag
%{bash_completions_dir}/sway*
%{fish_completions_dir}/sway*.fish
%{zsh_completions_dir}/_sway*

%changelog
* Sun Oct 04 2026 Basalt OS developers - 0.6-1
- SwayFX 0.6 (sway 1.11 base) on Fedora's wlroots 0.20 and scenefx 0.5.
