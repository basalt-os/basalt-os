# basalt-gate: the approval gate. Every request for a side effect (from
# the person by voice, the assistant, agents, apps, terminal tools and
# scripts) becomes a decision here: a deterministic policy of rules as
# data, hard limits no rule can loosen, a queue a person decides on a
# trusted surface, single-use claims for executors bound to a digest of
# what was approved, and a ledger record naming the rule or person that
# decided. Static Go binaries, the SELinux policy in the -selinux
# subpackage. The protocol and the client library (pkg/gate) are MIT OR
# Apache-2.0.

%global selinuxtype targeted
%global debug_package %{nil}

Name:           basalt-gate
Version:        0.3.0
Release:        1%{?dist}
Summary:        Approval gate: one decision point for every side effect
License:        Apache-2.0 AND (MIT OR Apache-2.0)
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.25
BuildRequires:  systemd-rpm-macros
BuildRequires:  selinux-policy-devel
BuildRequires:  make
BuildRequires:  bzip2

Requires:       polkit
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})
Recommends:     basalt-ledger
%{?systemd_requires}

%description
basalt-gate is the one local service where a request for a side effect
becomes a decision on Basalt OS. Requesters send typed actions from closed
sets (the action registry in /usr/share/basalt/gate/actions.d); the gate
identifies them from the kernel (SO_PEERCRED, the SELinux context, the
agent session), classifies each request by risk (Look, Undoable, System
change, Leaves the computer, Critical, Cannot be undone), checks hard
limits no rule can loosen, and applies the person's rules: the most
restrictive matching rule wins, and no match asks a person. People decide
in one queue (the terminal decider basalt-gate-tty with polkit, later the
shell and the Approvals app); executors claim an approval once, bound to
the digest of exactly what was shown. The emergency stop suspends every
rule at once. Every request, decision, claim and rule change is recorded
in basalt-ledger. The default preset, careful, asks before every change.

%package selinux
Summary:        SELinux policy for basalt-gate
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
Requires(post): policycoreutils
# The agent family attribute (basalt_agent_domain) the neverallow rules name.
Requires:       basalt-agent-selinux
Requires(post): basalt-agent-selinux
%{?selinux_requires}

%description selinux
The SELinux policy for basalt-gate: the service domain basalt_gate_t, its
state, runtime, configuration and data types, the terminal decider domain
basalt_gate_tty_t (entered from user domains only), the interfaces
requesters use to connect, and the rules no agent domain may ever be given
(the rule store, the decider domain).

%prep
%setup -q -c

%build
export GOFLAGS="-mod=mod -trimpath" GOTOOLCHAIN=local GOPROXY=off
for cmd in basalt-gated basalt-gate basalt-gate-tty; do
    go build -buildmode=pie -ldflags "-B gobuildid -X main.version=%{version}-%{release}" -o bin/$cmd ./cmd/$cmd
done
make -C selinux -f %{_datadir}/selinux/devel/Makefile basalt_gate.pp
bzip2 -9 selinux/basalt_gate.pp

%check
export GOFLAGS="-mod=mod" GOTOOLCHAIN=local GOPROXY=off
go test ./...
printf 'registry = actions.d\npresets = presets\nhardlimits = hardlimits.json\n' >check.conf
./bin/basalt-gated -check -config check.conf

%install
install -Dpm 0755 bin/basalt-gated %{buildroot}%{_libexecdir}/basalt-gate/basalt-gated
install -Dpm 0755 bin/basalt-gate-tty %{buildroot}%{_libexecdir}/basalt-gate/basalt-gate-tty
install -Dpm 0755 bin/basalt-gate %{buildroot}%{_bindir}/basalt-gate
install -d -m 0755 %{buildroot}%{_datadir}/basalt/gate/actions.d %{buildroot}%{_datadir}/basalt/gate/presets
install -pm 0644 actions.d/*.json %{buildroot}%{_datadir}/basalt/gate/actions.d/
install -pm 0644 presets/*.toml %{buildroot}%{_datadir}/basalt/gate/presets/
install -pm 0644 hardlimits.json %{buildroot}%{_datadir}/basalt/gate/hardlimits.json
install -Dpm 0644 dist/basalt-gated.service %{buildroot}%{_unitdir}/basalt-gated.service
install -Dpm 0644 dist/80-basalt-gate.preset %{buildroot}%{_presetdir}/80-basalt-gate.preset
install -Dpm 0644 dist/gate.conf %{buildroot}%{_sysconfdir}/basalt-gate/gate.conf
install -Dpm 0644 dist/org.basalt-os.gate.policy %{buildroot}%{_datadir}/polkit-1/actions/org.basalt-os.gate.policy
install -Dpm 0644 dist/49-basalt-gate-exec.rules %{buildroot}%{_datadir}/polkit-1/rules.d/49-basalt-gate-exec.rules
install -d -m 0700 %{buildroot}%{_sharedstatedir}/basalt-gate
install -Dpm 0644 selinux/basalt_gate.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/basalt_gate.pp.bz2
install -Dpm 0644 selinux/basalt_gate.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/basalt_gate.if
install -d licenses && install -pm 0644 LICENSE LICENSE-MIT licenses/

%post
%systemd_post basalt-gated.service

%preun
%systemd_preun basalt-gated.service

%postun
%systemd_postun_with_restart basalt-gated.service

%pre selinux
%selinux_relabel_pre -s %{selinuxtype}

%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/basalt_gate.pp.bz2

%postun selinux
if [ $1 -eq 0 ]; then
    %selinux_modules_uninstall -s %{selinuxtype} basalt_gate
fi

%posttrans selinux
%selinux_relabel_post -s %{selinuxtype}

%files
%license licenses/LICENSE licenses/LICENSE-MIT
%doc PROTOCOL.md
%{_bindir}/basalt-gate
%dir %{_libexecdir}/basalt-gate
%{_libexecdir}/basalt-gate/basalt-gated
%{_libexecdir}/basalt-gate/basalt-gate-tty
%dir %{_datadir}/basalt
%dir %{_datadir}/basalt/gate
%{_datadir}/basalt/gate/actions.d
%{_datadir}/basalt/gate/presets
%{_datadir}/basalt/gate/hardlimits.json
%{_unitdir}/basalt-gated.service
%{_presetdir}/80-basalt-gate.preset
%dir %{_sysconfdir}/basalt-gate
%config(noreplace) %{_sysconfdir}/basalt-gate/gate.conf
%{_datadir}/polkit-1/actions/org.basalt-os.gate.policy
%{_datadir}/polkit-1/rules.d/49-basalt-gate-exec.rules
# The rule set and the queue stay when the package is removed.
%dir %attr(0700,root,root) %{_sharedstatedir}/basalt-gate

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/basalt_gate.pp.bz2
%{_datadir}/selinux/devel/include/distributed/basalt_gate.if

%changelog
* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.3.0-1
- Registry: the system assistant's update, channel and software source
  actions (source.add is Critical: a new trust root).
- basalt_gate_exec_t starts dnf in rpm_t, so package scriptlets of an
  approved update or driver install run in rpm_script_t.

* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- ADR 0020 phase 2, first wave: hello tells components which approval
  paths the gate decides (enforce in gate.conf: apply, shell, skills,
  models, consent, agent; default skills, models, consent); the others
  stay in shadow mode and report what they decided with observe, recorded
  with what the gate would have decided. confirm: root's typed yes or
  --yes --confirm CODE for a system assistant proposal, recorded as
  person:tty-root (allow_code_confirm). Root may ask as the system
  assistant (root_relays). Executors read the requests addressed to them
  and may claim with their calls instead of a digest; the gate starts an
  executor's unit (basalt-gate-exec@ID.service) once a request for it is
  approved (exec_units). SELinux: the executor domain basalt_gate_exec_t
  (entered only by systemd from basalt-gate-exec@.service), the daemon's
  program type is now basalt_gated_exec_t, the gate may start that unit.
- The gate starts the executor unit with systemctl --no-ask-password and
  ships a polkit rule that lets root start exactly basalt-gate-exec@REQUEST
  units: without it, when systemctl goes over D-Bus, systemd asked polkit
  and the person saw a password prompt for a request they had already
  approved.
- The person's own request of a person-only action (power from the
  command bar) is decided by the same user without an administrator, as
  in the shell before; agents still cannot ask for it at all.
- Registry: skill grants are grant.folder, grant.mailbox and grant.site
  (Undoable, Leaves the computer for sites); a person's approval of one
  is kept as a rule ending with the grant (grant_arg); model downloads
  are the person's to approve in the session (decide_auth = session; the
  executor keeps models.conf and polkit) and the shell claims them;
  knowledge.fetch and remote.consent (consent.json); mailbox resources.
  A user's own programs (not agents) may tighten that user's rules.
- basalt-agent's actions: the tool runs what it asked for (executor
  requester); agent.egress.change is the person's own override,
  agent.egress.system every user's; grants and system allowlists are the
  person's to approve (decide_auth = session; the root helper keeps
  polkit).

* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version (ADR 0020 phase 1): the gate alone, no existing approval
  path uses it yet. Proposal format version 1 with a canonical digest and
  the 8-hex short code (the system assistant's fingerprint for its
  proposals); action registry with today's actions; six risk classes;
  hard limits (locked, always ask) as code plus data; rules as data with
  scopes, conditions, limits and a circuit breaker, taint, expiry, the
  most restrictive match wins; presets careful (default), balanced,
  hands-off (a template); queue with groups, expiry and deferrable
  requests; single-use claims bound to the digest; emergency stop;
  dry runs of rules against the ledger's records; gate.* ledger records;
  the NDJSON socket protocol (PROTOCOL.md) with its third-party subset,
  fixtures and a fake server; the Go client pkg/gate; the basalt-gate
  command and the terminal decider basalt-gate-tty with polkit.
