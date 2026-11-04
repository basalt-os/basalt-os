# cotty: a terminal shared by a person and an AI coding agent. The agent
# reads the screen and types through MCP; what the person types at a
# password prompt never reaches it. Ships in basalt-testing (opt-in) while
# it is pre-release.
#
# Build: packages/cotty/build.sh (from the commit pinned in source.conf; the
# version is the snapshot computed there). Source1 is the vendored Go module
# tree (go mod vendor at that commit), so the build needs no network.

%{!?cotty_version: %global cotty_version 0.1.0}
%{!?cotty_commit: %global cotty_commit unknown}

# Static Go binaries built with -trimpath; no separate debuginfo.
%global debug_package %{nil}

Name:           cotty
Version:        %{cotty_version}
Release:        1%{?dist}
Summary:        A terminal shared by a person and an AI coding agent
# cotty: Apache-2.0. Vendored Go modules: MIT (charmbracelet, cilium/ebpf,
# creack/pty, go-landlock, and others), BSD-3-Clause (golang.org/x),
# BSD-3-Clause OR GPL-2.0-only (libcap psx). The guard's BPF object:
# Apache-2.0 OR GPL-2.0-only.
License:        Apache-2.0 AND MIT AND BSD-3-Clause AND (BSD-3-Clause OR GPL-2.0-only) AND (Apache-2.0 OR GPL-2.0-only)
URL:            https://github.com/openbasalt/cotty
ExclusiveArch:  x86_64 aarch64

Source0:        cotty-%{version}.tar.gz
Source1:        cotty-%{version}-vendor.tar.gz
Source2:        third-party-licenses.sh

BuildRequires:  golang >= 1.26
BuildRequires:  systemd-rpm-macros

%description
cotty runs one real terminal session that a person and an AI coding agent
use at the same time. The agent reads the screen and types through MCP, so
any agent CLI that speaks MCP works. The person sees everything it types,
can type at any moment and can take the keyboard back. What the person
types at a password prompt never reaches the agent, and values on a
masking list are hidden in everything it reads. cotty agent starts the
agent CLI confined (namespaces, Landlock, seccomp, its own cgroup).

Pre-release software: not reviewed outside the project yet.

%package guard
Summary:        Kernel enforcement of cotty's agent rules (BPF LSM)
Requires:       cotty = %{version}-%{release}
%{?systemd_requires}

%description guard
cotty-guard is an optional root service that loads BPF LSM programs which
enforce cotty's agent rules in the kernel, for every process of an agent,
and protect a cotty session's terminal from every other process. It needs
the bpf LSM active (bpf in /sys/kernel/security/lsm) and kernel BTF.

The service is not enabled at installation: systemctl enable --now
cotty-guard. Removing the package stops it and runs cotty-guard uninstall,
which detaches the programs and removes their maps.

%prep
%setup -q -n cotty-%{version}
%setup -q -T -D -b 1 -n cotty-%{version}

%build
export CGO_ENABLED=0 GOFLAGS=-mod=vendor GOTOOLCHAIN=local GOPROXY=off
for bin in cotty cotty-guard; do
  go build -trimpath -ldflags "-s -w -buildid=" -o "bin/$bin" "./cmd/$bin"
done
bash %{SOURCE2} vendor >THIRD-PARTY-LICENSES

%install
install -Dpm 0755 bin/cotty %{buildroot}%{_bindir}/cotty
install -Dpm 0755 bin/cotty-guard %{buildroot}%{_bindir}/cotty-guard
install -Dpm 0644 dist/systemd/cotty-guard.service %{buildroot}%{_unitdir}/cotty-guard.service

%check
# The unit tests only: no PTY, no shell, no namespaces (the Makefile's
# "test" target). Sessions and confinement are tested in VMs upstream.
export CGO_ENABLED=0 GOFLAGS=-mod=vendor GOTOOLCHAIN=local GOPROXY=off
go test ./internal/... ./cmd/...

%post guard
%systemd_post cotty-guard.service
if [ "$1" -eq 1 ] && ! grep -qw bpf /sys/kernel/security/lsm 2>/dev/null; then
  echo "cotty-guard: the bpf LSM is not active on the running kernel; the guard will not start until it is (see the Distributions section of /usr/share/doc/cotty-guard/guard.md)."
fi

%preun guard
%systemd_preun cotty-guard.service
if [ "$1" -eq 0 ]; then
  # Removal: the service is stopped above; detach the programs and remove
  # the pinned maps, so no rule outlives the package.
  # The unit keeps its runtime directory across a stop
  # (RuntimeDirectoryPreserve); once the programs are detached nothing in
  # it is needed. Kept when uninstall fails: its "enforcing" marker makes
  # cotty fail closed while pinned programs may still enforce.
  if %{_bindir}/cotty-guard uninstall; then
    systemctl clean --what=runtime cotty-guard.service >/dev/null 2>&1 || rm -rf /run/cotty-guard
  else
    echo "cotty-guard: uninstall failed; run cotty-guard uninstall as root before removing cotty" >&2
  fi
fi

%postun guard
%systemd_postun_with_restart cotty-guard.service

%files
%license LICENSE THIRD-PARTY-LICENSES
%doc README.md SECURITY.md CHANGELOG.md docs
%{_bindir}/cotty

%files guard
%license LICENSE
%doc docs/guard.md
%{_bindir}/cotty-guard
%{_unitdir}/cotty-guard.service

# The entry's version comes from packages/cotty/source.conf, so a pin bump
# changes no line here.
%changelog
* Wed Nov 04 2026 Basalt OS project <noreply@basalt-os.org> - %{version}-%{release}
- First package for basalt-testing: cotty (session broker, attach, MCP
  server, cotty agent) and cotty-guard (BPF LSM service, not enabled at
  installation, uninstalled with the package).
