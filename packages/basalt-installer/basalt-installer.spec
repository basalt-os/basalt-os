# Basalt OS installer: one engine (declarative plan, exact preview, rollback,
# hash-chained install log) with a text frontend (tui-kit), a local JSON API
# and a graphical frontend (Quickshell) in the -gui subpackage.

Name:           basalt-installer
Version:        0.1.3
Release:        1%{?dist}
Summary:        Basalt OS installer: plan engine, text and graphical frontends

# Apache-2.0: this package. Bundled Go modules: tui-kit, Bubble Tea, Bubbles,
# Lip Gloss and their dependencies (MIT, BSD-3-Clause), go-yaml (Apache-2.0
# AND MIT).
License:        Apache-2.0 AND MIT AND BSD-3-Clause
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{name}-%{version}.tar.gz
# `go mod vendor` output, made by packages/basalt-installer/build.sh from
# go.mod and go.sum (checksums verified); never committed.
Source1:        %{name}-vendor-%{version}.tar.gz

ExclusiveArch:  x86_64
# Built with the upstream Go toolchain named in go.mod (toolchain go1.27.1):
# tui-kit needs Go 1.27 and Fedora 44 ships 1.26. build.sh puts a
# checksum-verified official Go first in PATH; nothing is downloaded here.
# gcc: a position-independent, dynamically linked binary (Fedora policy).
BuildRequires:  gcc

Requires:       util-linux
Requires:       gdisk
Requires:       dosfstools
Requires:       e2fsprogs
Requires:       btrfs-progs
Requires:       cryptsetup
Requires:       systemd
Requires:       dnf5
Requires:       efibootmgr
Recommends:     tpm2-tss
Recommends:     clevis-luks

Provides:       bundled(golang(github.com/tui-tools/tui-kit)) = 0.4.4
Provides:       bundled(golang(github.com/charmbracelet/bubbletea)) = 1.3.10
Provides:       bundled(golang(github.com/charmbracelet/bubbles)) = 1.0.0
Provides:       bundled(golang(github.com/charmbracelet/lipgloss)) = 1.1.0
Provides:       bundled(golang(go.yaml.in/yaml/v3)) = 3.0.4

%global debug_package %{nil}

%description
basalt-installer installs Basalt OS on one disk from a declarative plan:
GPT with an EFI system partition, /boot and btrfs with the Basalt subvolume
layout on LUKS2, a TPM2 key sealed to PCR 7 and a recovery key shown once,
packages with dnf from the signed repositories, boot loader setup for
Secure Boot with Fedora's signed shim, snapper, and a full SELinux relabel.
Every command is listed in a preview before anything is written, the
person types the disk name to confirm, and a hash-chained install log is
kept. Frontends: a text wizard (console and serial), a local JSON API for
the graphical frontend, and non-interactive installs from a plan file.

%package gui
Summary:        Graphical frontend of the Basalt OS installer (Quickshell)
BuildArch:      noarch
Requires:       %{name} = %{version}-%{release}
Requires:       quickshell
Requires:       cage

%description gui
The graphical Basalt OS installer: QML for Quickshell, run in the cage
kiosk compositor on the live image. It talks to `basalt-installer serve`
over a Unix socket and shows the same plan, preview, confirmation,
progress and recovery key as the text installer.

%prep
%setup -q -c -n %{name}-%{version}
tar -xzf %{SOURCE1}

%build
export GOFLAGS="-mod=vendor -trimpath" GOTOOLCHAIN=local GOPROXY=off
go build -buildmode=pie -ldflags "-s -w -linkmode=external -X main.Version=%{version}" -o basalt-installer ./cmd/basalt-installer

%check
export GOFLAGS="-mod=vendor" GOTOOLCHAIN=local GOPROXY=off
go vet ./...
go test ./...

%install
install -Dpm 0755 basalt-installer %{buildroot}%{_bindir}/basalt-installer
install -d %{buildroot}%{_datadir}/basalt-installer/gui
install -pm 0644 gui/*.qml %{buildroot}%{_datadir}/basalt-installer/gui/
install -Dpm 0644 examples/server.yaml %{buildroot}%{_datadir}/basalt-installer/examples/server.yaml
install -d licenses
install -pm 0644 LICENSE licenses/LICENSE
for m in vendor/github.com/tui-tools/tui-kit vendor/github.com/charmbracelet/bubbletea \
         vendor/github.com/charmbracelet/bubbles vendor/github.com/charmbracelet/lipgloss vendor/go.yaml.in/yaml/v3; do
  install -pm 0644 "$m/LICENSE" "licenses/$(echo "${m#vendor/}" | tr / _).LICENSE"
done

%files
%license licenses/*
%{_bindir}/basalt-installer
%dir %{_datadir}/basalt-installer
%{_datadir}/basalt-installer/examples/

%files gui
%{_datadir}/basalt-installer/gui/

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.3-1
- The welcome text names the server and desktop editions, through a
  translation catalog (gettext in the text installer, qsTr in the
  graphical one).

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.2-1
- Installs basalt-prompt (the shell prompt) on both editions, as the
  kickstart does.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.1-1
- Installs basalt-resolver and basalt-ledger (with their SELinux policy)
  and enables both services, as the kickstart does.
- basalt-release now ships the [basalt-tools] repository; the installer no
  longer writes basalt-tools.repo and turns the repository off with a dnf
  override only when the plan says repos.tools: false.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: plan engine, preview, rollback, install log, TUI, GUI API
