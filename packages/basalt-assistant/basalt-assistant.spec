# Basalt OS system assistant: diagnosis without a language model, typed
# proposals, confirmed changes with snapshots, a hash-chained audit log, an
# event engine, a decision layer and MCP tools. SELinux module in the
# -selinux subpackage.

%global selinuxtype targeted
%global modulename basalt_assistant
%global goipath github.com/basalt-os/basalt-os/packages/basalt-assistant
# Go binaries carry a Go build id (-B gobuildid); no separate debuginfo.
%global debug_package %{nil}

Name:           basalt-assistant
Version:        0.4.0
Release:        1%{?dist}
Summary:        Basalt OS system assistant: diagnosis, proposals, confirmed changes, audit
# The command runner is adapted from tui-kit (MIT).
License:        Apache-2.0 AND MIT
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.25
BuildRequires:  gcc
BuildRequires:  systemd-rpm-macros
BuildRequires:  selinux-policy-devel
BuildRequires:  make
BuildRequires:  bzip2

Requires:       systemd
Requires:       snapper
Requires:       btrfs-progs
Requires:       basalt-snapshots
# semanage, restorecon, setsebool, matchpathcon, getsebool, sesearch, seinfo
Requires:       policycoreutils
Requires:       policycoreutils-python-utils
Requires:       libselinux-utils
Requires:       setools-console
Requires:       iproute
Requires:       diffutils
Requires:       findutils
Requires:       coreutils
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})
%{?systemd_requires}

%description
The Basalt OS system assistant diagnoses the system without a language
model. `basalt status`, `basalt why UNIT`, `basalt fix selinux`,
`basalt snapshots` and `basalt disk` read the journal, unit state, SELinux
denials and policy, snapshots and disk usage, and explain what is wrong.
A fix is a proposal made of typed actions; `basalt apply` shows the exact
commands, asks for confirmation, takes a snapshot before and after, runs
them, verifies the result and writes a hash-chained audit record.

basalt-assistantd watches the journal, disk usage and package transactions
and stores a diagnosis with a proposed fix for each event; it never changes
the system. basalt-mcp exposes the same diagnosers as MCP tools; its write
tools only store proposals. basalt-notify shows findings as desktop
notifications in local graphical sessions and can post them to a webhook
signed with HMAC-SHA256 (off by default); the journal always has them.
The audit log is sealed and rotated daily once it is large enough, and its
hash chain is verified across the rotated files.

With the optional local model service (basalt-llm), `basalt ask` accepts
requests in English or Portuguese and the decision layer can use the model
as a backend; neither is enabled by default.

%package selinux
Summary:        SELinux policy module for the Basalt OS system assistant
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
%{?selinux_requires}

%description selinux
SELinux policy module basalt_assistant: the domain basalt_assistant_t for
basalt-assistantd and basalt-mcp, which may read what diagnosis needs and
write only its state directory and (append only) its audit log.

%prep
%setup -q -c

%build
export GOFLAGS="-mod=mod -trimpath" GOTOOLCHAIN=local GOPROXY=off
for c in basalt basalt-assistantd basalt-mcp basalt-notify; do
    go build -buildmode=pie -ldflags "-B gobuildid -X main.version=%{version}-%{release}" -o bin/$c ./cmd/$c
done
make -C selinux -f %{_datadir}/selinux/devel/Makefile %{modulename}.pp
bzip2 -9 selinux/%{modulename}.pp

%check
export GOFLAGS="-mod=mod" GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
install -Dpm 0755 bin/basalt %{buildroot}%{_bindir}/basalt
install -Dpm 0755 bin/basalt-mcp %{buildroot}%{_bindir}/basalt-mcp
install -Dpm 0755 bin/basalt-assistantd %{buildroot}%{_libexecdir}/basalt/basalt-assistantd
install -Dpm 0755 bin/basalt-notify %{buildroot}%{_libexecdir}/basalt/basalt-notify
install -Dpm 0644 dist/basalt-assistantd.service %{buildroot}%{_unitdir}/basalt-assistantd.service
install -Dpm 0644 dist/basalt-notify.service %{buildroot}%{_unitdir}/basalt-notify.service
install -Dpm 0644 dist/basalt-audit-rotate.service %{buildroot}%{_unitdir}/basalt-audit-rotate.service
install -Dpm 0644 dist/basalt-audit-rotate.timer %{buildroot}%{_unitdir}/basalt-audit-rotate.timer
install -Dpm 0644 dist/81-basalt-assistant.preset %{buildroot}%{_presetdir}/81-basalt-assistant.preset
install -Dpm 0644 dist/basalt-assistant.tmpfiles %{buildroot}%{_tmpfilesdir}/%{name}.conf
install -Dpm 0644 dist/assistant.conf %{buildroot}%{_sysconfdir}/basalt/assistant.conf
install -dm 0700 %{buildroot}%{_sharedstatedir}/basalt-assistant
install -dm 0700 %{buildroot}%{_sharedstatedir}/basalt-assistant/proposals
install -dm 0700 %{buildroot}%{_localstatedir}/log/basalt-assistant
install -Dpm 0644 selinux/%{modulename}.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2
install -Dpm 0644 selinux/%{modulename}.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/%{modulename}.if
install -d licenses && install -pm 0644 LICENSE third_party/tui-kit.LICENSE licenses/

%post
%tmpfiles_create %{name}.conf
%systemd_post basalt-assistantd.service basalt-notify.service basalt-audit-rotate.timer

%preun
%systemd_preun basalt-assistantd.service basalt-notify.service basalt-audit-rotate.service basalt-audit-rotate.timer

%postun
%systemd_postun_with_restart basalt-assistantd.service basalt-notify.service
%systemd_postun basalt-audit-rotate.service basalt-audit-rotate.timer

%pre selinux
%selinux_relabel_pre -s %{selinuxtype}

%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2

%postun selinux
if [ $1 -eq 0 ]; then
    %selinux_modules_uninstall -s %{selinuxtype} %{modulename}
fi

%posttrans selinux
%selinux_relabel_post -s %{selinuxtype}

%files
%license licenses/LICENSE licenses/tui-kit.LICENSE
%{_bindir}/basalt
%{_bindir}/basalt-mcp
%dir %{_libexecdir}/basalt
%{_libexecdir}/basalt/basalt-assistantd
%{_libexecdir}/basalt/basalt-notify
%{_unitdir}/basalt-assistantd.service
%{_unitdir}/basalt-notify.service
%{_unitdir}/basalt-audit-rotate.service
%{_unitdir}/basalt-audit-rotate.timer
%{_presetdir}/81-basalt-assistant.preset
%{_tmpfilesdir}/%{name}.conf
%dir %{_sysconfdir}/basalt
%config(noreplace) %{_sysconfdir}/basalt/assistant.conf
%dir %attr(0700,root,root) %{_sharedstatedir}/basalt-assistant
%dir %attr(0700,root,root) %{_sharedstatedir}/basalt-assistant/proposals
%dir %attr(0700,root,root) %{_localstatedir}/log/basalt-assistant

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2
%{_datadir}/selinux/devel/include/distributed/%{modulename}.if
%ghost %verify(not md5 size mode mtime) %{_sharedstatedir}/selinux/%{selinuxtype}/active/modules/200/%{modulename}

%changelog
* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.4.0-1
- Fixes from a live run on a lab VM. SELinux policy queries retry with a
  jittered backoff when another process holds /sys/fs/selinux/policy
  (EBUSY) and a query that still fails is an error and an incomplete
  diagnosis, never "no rule"; answers are cached briefly per process.
- Config checkers (nginx -t and the others) run in a private mount
  namespace where every file system is a throwaway overlay (basalt
  __sandbox): a diagnosis no longer creates the log files a configuration
  names. A restore candidate is the newest snapshot copy that passes the
  checker in place of the current file; copies that fail are listed.
- The AVC path search covers the document roots in the nginx and httpd
  configurations and the usual web roots; every answer must be a valid
  path with the AVC's name and inode (find's error text is never a path).
- The service domain comes from the policy (the transition from init_t,
  or SELinuxContext=): a unit that runs a shell is unconfined_service_t,
  not shell_t. "Permission denied" is checked against file mode and owner
  for the unit's User= first (DAC), so file permissions are not blamed on
  SELinux.
- Out-of-memory kills (result oom-kill, systemd and kernel messages) are
  recognized: cause crashed, the memory limits as evidence, no restart.
- From the confined daemon and the MCP server a file restore or a rollback
  is a hint, not an action; basalt confirm ID checks the snapshot as root
  and stores it as a proposal. Proposals are validated when stored and
  applied.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.3.0-1
- Installed and enabled by default (the daemon only proposes; applying
  still needs basalt apply and a confirmation).
- Audit log sealed rotation: basalt audit rotate (daily timer, [audit]
  rotate_size) ends the file with a seal record carrying the SHA-256 of
  its content, keeps it immutable and starts the next file with a continue
  record chained to the seal; basalt audit verify checks every file.
- basalt-notify: desktop notifications (freedesktop) for findings the
  decision layer marks, only where a graphical session exists; optional
  HMAC-signed webhook ([notify] in assistant.conf), off by default.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- Optional local language model (package basalt-llm): basalt ask translates
  requests in natural language into basalt commands (schema-constrained,
  validated, grounded in the request; changes are never run from it), and
  an opt-in model backend for the decision layer (option probabilities from
  token log-probabilities, per-question temperature). Rules stay the default.
- Unit-cause questions carry the journal lines, disk-cause questions the
  sizes, for a model backend.
- [translator] prompt = auto (new default): the compact prompt for a
  fine-tuned translator (basalt-translator-*), the prompt with examples
  for any other model, by the name the model service reports.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: basalt CLI (status, why, fix selinux, snapshots, disk,
  pending, apply, ignore, audit), basalt-assistantd event engine, decision
  layer with a rules backend, MCP server, SELinux module.
