# Basalt OS installer: one engine (declarative plan, exact preview, rollback,
# hash-chained install log) with a text frontend (tui-kit), a local JSON API
# and a graphical frontend (Quickshell) in the -gui subpackage.

Name:           basalt-installer
Version:        0.2.3
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
BuildRequires:  gettext

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
for po in po/*.po; do
    lang=$(basename "$po" .po)
    mkdir -p locale/$lang/LC_MESSAGES
    msgfmt --check -o locale/$lang/LC_MESSAGES/%{name}.mo "$po"
done

%check
export GOFLAGS="-mod=vendor" GOTOOLCHAIN=local GOPROXY=off
go vet ./...
go test ./...

%install
install -Dpm 0755 basalt-installer %{buildroot}%{_bindir}/basalt-installer
install -d %{buildroot}%{_datadir}/basalt-installer/gui
install -pm 0644 gui/*.qml %{buildroot}%{_datadir}/basalt-installer/gui/
install -Dpm 0644 examples/server.yaml %{buildroot}%{_datadir}/basalt-installer/examples/server.yaml
for mo in locale/*/LC_MESSAGES/%{name}.mo; do
    install -Dpm 0644 "$mo" "%{buildroot}%{_datadir}/$mo"
done
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
%{_datadir}/locale/*/LC_MESSAGES/%{name}.mo

%files gui
%{_datadir}/basalt-installer/gui/

%changelog
* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.3-1
- The installation's end state changes together with the "done" event: a
  frontend that finishes, retries or reads the status right after it no
  longer sees the installation still running. A run that stops before it
  can report (lock held, log not writable) is marked failed and announced
  instead of leaving the frontends waiting; a retry waits for the previous
  run to release its lock.
- An unattended installation reacts to each state change instead of
  polling once a second.

* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.2-1
- Desktop edition: the graphical boot splash (rhgb quiet
  plymouth.ignore-serial-consoles on the kernel command line, the serial
  console still works), so the disk passphrase is asked on the splash's
  card; servers keep the text boot.
- The splash theme follows the system's language (basalt-pt_BR for
  Portuguese, basalt otherwise).
- The desktop edition installs basalt-greeter-selinux (the login screen's
  SELinux module).

* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.1-1
- The Fedora release of the live system comes from the major number of
  os-release's VERSION_ID (44.0 since basalt-release 44-8), else
  PLATFORM_ID.
- New plan field repos.testing: basalt-testing at install time (from the
  media's basalt/testing tree when there is one) and turned on in the
  installed system; the desktop edition needs it while its shell is
  pre-release.
- A starting plan without target.disk (the live desktop's plan on the
  media) takes the suggested disk.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- Owner decisions of 2026-10-04: the recovery key is never written to a
  disk by default; the person can save a copy to a USB stick (text
  installer ctrl+o, graphical installer button, plan field
  encryption.recovery_key_media) and still acknowledges it.
- Unattended installation from the boot menu only when the boot line
  names the disk to erase (basalt.inst.confirm=DISK with
  basalt.inst.plan=); the engine service runs it and the text installers
  follow it (tui --attach). Without the confirmation the plan prefills the
  wizard, and the welcome screen says which plan was loaded.
- The engine refuses to start outside install_t on the live image (the
  systemd debug shell runs in initrc_t) and says how to start it.
- install --plan ends with a summary: disk, edition, encryption, TPM,
  accounts, network, repositories, where the key went, time, log.
- dnf5-plugins in the package set of both editions (dnf config-manager).
- Text installer: a plan's password hash survives the accounts screen;
  the static network form is filled with the detected interface; the
  failure screen, the accounts form and the encryption, review and status
  texts fit 80x24; no terminal queries on a serial console (no 5 s wait,
  no swallowed keys); n on the welcome screen opens nmtui (network, Wi-Fi).
- Translation catalogs (po/, Brazilian Portuguese) for the new and changed
  texts; a test keeps the template current and the translations complete.
- Storage choices (ADR 0002 addendum): install into the free space of a
  disk that keeps its partitions (dual boot, own or shared EFI system
  partition, every partition change in the preview); encrypt the whole
  system, only a new /home, or nothing; adopt an existing /home partition
  (LUKS or plain, passphrase checked first, never formatted, crypttab with
  a passphrase prompt or the TPM too, user with the same name, uid and gid
  and a directory of its own, only that directory relabeled); zram swap.

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
