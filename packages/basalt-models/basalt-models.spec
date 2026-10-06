# Basalt OS model downloads from the desktop: the privileged entry point
# the desktop shell runs through polkit after the person's consent, and the
# confined system service that downloads and verifies the models with
# basalt-voice-fetch and basalt-llm-fetch. Scripts and configuration only.

Name:           basalt-models
Version:        0.1.0
Release:        1%{?dist}
Summary:        One-click, consented downloads of the desktop's speech and assistant models
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch
Source0:        basalt-models-request
Source1:        basalt-models-request-admin
Source2:        basalt-models-run
Source3:        basalt-models-fetch@.service
Source4:        org.basalt-os.models.policy
Source5:        models.conf
Source6:        basalt-models.tmpfiles
Source7:        LICENSE

BuildRequires:  systemd-rpm-macros
Requires:       polkit
Requires:       systemd
Requires:       coreutils
Requires:       gawk
Requires:       sed
# The downloads themselves (either may be missing: the desktop offers
# only what is installed).
Recommends:     basalt-voice
Recommends:     basalt-llm

%description
When push to talk has no speech model yet, or a skill needs the
assistant's local model, the Basalt desktop asks the person and, if they
choose Download, starts a download here: polkit lets a person in an
active local session do it without a password (the administrator can
limit it to administrators or turn it off in /etc/basalt/models.conf),
the request is checked against the model manifests and recorded in the
audit ledger with who consented, and a confined system service fetches
the files from their pinned URLs and checks their SHA-256. Progress is
shown on the desktop; nothing runs without the person's consent.

%prep
cp -p %{SOURCE7} LICENSE

%build

%install
install -Dpm 0755 %{SOURCE0} %{buildroot}%{_libexecdir}/basalt-models/request
install -Dpm 0755 %{SOURCE1} %{buildroot}%{_libexecdir}/basalt-models/request-admin
install -Dpm 0755 %{SOURCE2} %{buildroot}%{_libexecdir}/basalt-models/run
install -Dpm 0644 %{SOURCE3} %{buildroot}%{_unitdir}/basalt-models-fetch@.service
install -Dpm 0644 %{SOURCE4} %{buildroot}%{_datadir}/polkit-1/actions/org.basalt-os.models.policy
install -Dpm 0644 %{SOURCE5} %{buildroot}%{_sysconfdir}/basalt/models.conf
install -Dpm 0644 %{SOURCE6} %{buildroot}%{_tmpfilesdir}/basalt-models.conf
install -dm 0755 %{buildroot}/run/basalt-models

%post
%tmpfiles_create %{_tmpfilesdir}/basalt-models.conf

%files
%license LICENSE
%dir %{_libexecdir}/basalt-models
%{_libexecdir}/basalt-models/request
%{_libexecdir}/basalt-models/request-admin
%{_libexecdir}/basalt-models/run
%{_unitdir}/basalt-models-fetch@.service
%{_datadir}/polkit-1/actions/org.basalt-os.models.policy
%dir %{_sysconfdir}/basalt
%config(noreplace) %{_sysconfdir}/basalt/models.conf
%{_tmpfilesdir}/basalt-models.conf
%ghost %dir /run/basalt-models

%changelog
* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: the desktop's consented model downloads (polkit action
  for active local sessions, administrator policy in models.conf, ledger
  records of who consented), downloaded by a confined service with
  basalt-voice-fetch and basalt-llm-fetch.
