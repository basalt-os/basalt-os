# Knowledge index of the Basalt OS assistant's VSM decision backend for one
# Fedora release: the cases VSM looks up by evidence pattern (diagnosis,
# typed action templates, the checks that confirm a fix, version range,
# provenance). New knowledge is a data update shipped with normal system
# updates, not a new model.
#
# The files are not kept in the source tree: scripts/data-package.sh
# fetches them by path from the artifacts location and checks the SHA-256
# and size in sources.manifest; the prep section checks the SHA-256
# again. manifest.json addresses cases.jsonl and index.bin by SHA-256 and
# is signed (manifest.json.sig, a detached OpenPGP signature by the
# OpenBasalt knowledge subkey 85D6 1430 B704 3868 0F6E B955 E79E 4020 605A
# 659A of the release key 3601 7348 42BD 4E48 2D19 DE4A E4EE D5EC A395
# B302, made by scripts/sign-knowledge.sh). The package cannot be built
# without that signature. The assistant checks the signature, the
# fingerprints and every SHA-256 when it opens the index, and its rules
# answer if anything does not match.

%global fedora_release 44
%global knowledge_date 20261004
# Placeholder until the artifacts location is published; the build script
# takes BASALT_ARTIFACTS_URL or a local mirror (BASALT_ARTIFACTS_DIR).
%{!?basalt_artifacts_url:%global basalt_artifacts_url https://obpkg.org/artifacts}

Name:           basalt-knowledge
Version:        %{fedora_release}.%{knowledge_date}
Release:        1%{?dist}
Summary:        Knowledge index of the Basalt OS assistant (VSM backend, Fedora %{fedora_release})
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
Source0:        %{basalt_artifacts_url}/knowledge/%{fedora_release}/%{knowledge_date}/cases.jsonl
Source1:        %{basalt_artifacts_url}/knowledge/%{fedora_release}/%{knowledge_date}/index.bin
Source2:        %{basalt_artifacts_url}/knowledge/%{fedora_release}/%{knowledge_date}/manifest.json
Source3:        sources.manifest
Source4:        LICENSE
Source5:        %{basalt_artifacts_url}/knowledge/%{fedora_release}/%{knowledge_date}/manifest.json.sig
# The OpenBasalt release key (public), the same file as basalt-release's
# RPM-GPG-KEY-basalt: the certificate the signature is checked against.
Source6:        openbasalt-release-key.asc
BuildArch:      noarch
BuildRequires:  coreutils
# The DSL the cases are written in (feature codes, goals); the planner
# weights must speak the same one.
Provides:       basalt-knowledge(dsl) = 3

%description
The knowledge index of the Basalt OS system assistant's VSM decision
backend for Fedora %{fedora_release}: known problems as cases, each with the
evidence pattern it requires (feature codes the assistant's diagnosers
find), the cause it implies, a diagnosis, the typed actions that fix it
from the assistant's closed action set, the checks that confirm the fix,
the versions where it applies, its provenance and license.

Installed under /usr/share/basalt/knowledge/%{fedora_release}. The
assistant only reads it (SELinux type basalt_knowledge_t); the VSM backend
is used only when /etc/basalt/assistant.conf selects it.

%prep
# The build script checked these already; check again in the build root.
for f in %{SOURCE0} %{SOURCE1} %{SOURCE2} %{SOURCE5}; do
  want=$(awk -v n="${f##*/}" '!/^#/ && $1 == n {print $2}' %{SOURCE3})
  [ -n "$want" ] || { echo "no checksum for $f"; exit 1; }
  echo "$want  $f" | sha256sum -c --quiet -
done

%build

%install
install -d %{buildroot}%{_datadir}/basalt/knowledge/%{fedora_release}
install -pm 0644 %{SOURCE0} %{SOURCE1} %{SOURCE2} %{SOURCE5} %{buildroot}%{_datadir}/basalt/knowledge/%{fedora_release}/
install -pm 0644 %{SOURCE6} %{buildroot}%{_datadir}/basalt/knowledge/openbasalt-release-key.asc
install -Dpm 0644 %{SOURCE4} licenses/LICENSE

%files
%license licenses/LICENSE
%dir %{_datadir}/basalt
%dir %{_datadir}/basalt/knowledge
%{_datadir}/basalt/knowledge/%{fedora_release}/
%{_datadir}/basalt/knowledge/openbasalt-release-key.asc

%changelog
* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 44.20261004-1
- DSL version 3 (code dm: a file-permission error found by the path
  check). 39 cases: kb-unit-dac (mode and owner deny a path: unknown, not
  SELinux) and kb-unit-selinux-avc-quiet (a denial stopped the unit, no
  permission error logged); kb-unit-selinux-label excludes dm.
- Signed: manifest.json.sig (OpenBasalt knowledge subkey), and the
  manifest addresses index.bin too; the release key ships next to the
  indexes.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 44.20261003-1
- First knowledge index: 37 cases for unit failures, SELinux denials, disk
  pressure and failed package transactions (lab fault injection, the
  assistant's own logic, a live run on a lab machine), DSL version 2.
