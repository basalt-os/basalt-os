# Planner weights of the Basalt OS assistant's VSM decision backend: a
# small gated linear recurrence (about 65,000 parameters, 256 KiB) that
# reads the goal, the evidence codes and the ranked knowledge candidates,
# and accepts a candidate, picks another or abstains. It changes only when
# its DSL changes or the evaluation shows a drop; new problems arrive as
# knowledge (basalt-knowledge), without retraining.
#
# The files are not kept in the source tree: scripts/data-package.sh
# fetches them by path from the artifacts location and checks the SHA-256
# and size in sources.manifest; the prep section checks the SHA-256 again.

%global dsl 2
%global planner_build 20261003
# Placeholder until the artifacts location is published; the build script
# takes BASALT_ARTIFACTS_URL or a local mirror (BASALT_ARTIFACTS_DIR).
%{!?basalt_artifacts_url:%global basalt_artifacts_url https://obpkg.org/artifacts}

Name:           basalt-vsm-planner
Version:        %{dsl}.%{planner_build}
Release:        1%{?dist}
Summary:        Planner weights of the Basalt OS assistant's VSM decision backend
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{basalt_artifacts_url}/vsm-planner/dsl%{dsl}/%{planner_build}/weights.bin
Source1:        %{basalt_artifacts_url}/vsm-planner/dsl%{dsl}/%{planner_build}/config.txt
Source2:        sources.manifest
Source3:        LICENSE
BuildArch:      noarch
BuildRequires:  coreutils
Provides:       basalt-vsm-planner(dsl) = %{dsl}
# Weights without knowledge in the same DSL decide nothing.
Requires:       basalt-knowledge(dsl) = %{dsl}

%description
The planner weights of the Basalt OS system assistant's VSM decision
backend: a small recurrent model (one embedding, two gates, two heads,
dimension 128) that judges the knowledge index's candidates for the
evidence the assistant's diagnosers found. A deterministic guard outside
the model refuses any pick the evidence does not fully support.

Installed under /usr/share/basalt/vsm-planner. The assistant only reads it
(SELinux type basalt_knowledge_t); the VSM backend is used only when
/etc/basalt/assistant.conf selects it.

%prep
# The build script checked these already; check again in the build root.
for f in %{SOURCE0} %{SOURCE1}; do
  want=$(awk -v n="${f##*/}" '!/^#/ && $1 == n {print $2}' %{SOURCE2})
  [ -n "$want" ] || { echo "no checksum for $f"; exit 1; }
  echo "$want  $f" | sha256sum -c --quiet -
done

%build

%install
install -d %{buildroot}%{_datadir}/basalt/vsm-planner
install -pm 0644 %{SOURCE0} %{SOURCE1} %{buildroot}%{_datadir}/basalt/vsm-planner/
install -Dpm 0644 %{SOURCE3} licenses/LICENSE

%files
%license licenses/LICENSE
%dir %{_datadir}/basalt
%{_datadir}/basalt/vsm-planner/

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 2.20261003-1
- First planner for DSL version 2 (candidates shown in rank order).
