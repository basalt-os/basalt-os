# Basalt OS desktop voice: whisper.cpp (speech to text with Silero voice
# activity detection) built for the CPU only, and a download tool for
# checksum-pinned speech models. The voice service itself (basalt-voiced)
# and its SELinux domain come with basalt-shell; this package provides
# the speech-to-text program and the models it uses. No model is packaged.
#
# Fedora's whisper-cpp package was not used: it ships the library only (no
# whisper-cli) and requires the ROCm runtime (gigabytes), against a light
# desktop. Built for basalt-testing by scripts/release/build-testing.sh
# (packages/basalt-voice/build.sh); not by CI, it compiles whisper.cpp.

%global whisper_version 1.9.4
%global debug_package %{nil}
%global __provides_exclude_from ^%{_libdir}/basalt-voice/.*$
%global __requires_exclude ^lib(whisper|ggml|parakeet).*$

Name:           basalt-voice
Version:        0.3.0
Release:        1%{?dist}
Summary:        Local speech to text for the Basalt OS desktop (no network)
# basalt-voice files: Apache-2.0. whisper.cpp and ggml: MIT; bundled
# dr_libs/miniaudio: MIT-0 or public domain.
License:        Apache-2.0 AND MIT
URL:            https://github.com/basalt-os/basalt-os
Source0:        https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v%{whisper_version}.tar.gz#/whisper.cpp-%{whisper_version}.tar.gz
Source1:        basalt-voice-fetch
Source2:        models.manifest
Source3:        LICENSE

ExclusiveArch:  x86_64 aarch64
BuildRequires:  cmake
BuildRequires:  gcc-c++
BuildRequires:  make
Requires:       curl
Requires:       coreutils
Requires:       pipewire-utils
# One-click, consented downloads from the desktop (polkit, the confined
# download service).
Recommends:     basalt-models
Provides:       bundled(whisper-cpp) = %{whisper_version}

%description
The speech-to-text program of the Basalt OS desktop's push to talk:
whisper.cpp's whisper-cli built for the CPU only (every x86-64 variant,
the best one picked at run time), with Silero voice activity detection.
The desktop's voice service (basalt-voiced, in basalt-shell) runs it on
each utterance, without network access.

No model is included. basalt-voice-fetch downloads the speech models
listed in the manifest (Whisper, Silero VAD and the Piper voices trained
on public-domain data only), each pinned to a publisher revision and
verified against its SHA-256 before use.

%prep
%setup -q -n whisper.cpp-%{whisper_version}

%build
%cmake -DGGML_NATIVE=OFF -DGGML_BACKEND_DL=ON -DGGML_CPU_ALL_VARIANTS=ON \
       -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DWHISPER_SDL2=OFF \
       -DBUILD_SHARED_LIBS=ON -DCMAKE_SKIP_INSTALL_RPATH=OFF -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON \
       -DCMAKE_INSTALL_RPATH='$ORIGIN'
%cmake_build --target whisper-cli

%install
install -d %{buildroot}%{_libdir}/basalt-voice
install -pm 0755 %{__cmake_builddir}/bin/whisper-cli %{buildroot}%{_libdir}/basalt-voice/
for f in %{__cmake_builddir}/bin/lib*.so* %{__cmake_builddir}/src/lib*.so* %{__cmake_builddir}/ggml/src/lib*.so*; do
    [ -e "$f" ] && cp -P "$f" %{buildroot}%{_libdir}/basalt-voice/
done
install -Dpm 0755 %{SOURCE1} %{buildroot}%{_bindir}/basalt-voice-fetch
install -Dpm 0644 %{SOURCE2} %{buildroot}%{_datadir}/basalt-voice/models.manifest
install -dm 0755 %{buildroot}%{_sharedstatedir}/basalt-voice/models
install -d basalt-licenses
install -pm 0644 %{SOURCE3} basalt-licenses/LICENSE
install -pm 0644 LICENSE basalt-licenses/whisper.cpp.LICENSE

%check
# The program starts and finds a CPU backend.
LD_LIBRARY_PATH=%{buildroot}%{_libdir}/basalt-voice %{buildroot}%{_libdir}/basalt-voice/whisper-cli --help >/dev/null

%posttrans
# whisper-cli runs in the voice service's SELinux domain only with the type
# basalt-shell-selinux gives it (basalt_voice_tool_exec_t, through the
# /usr/lib64 = /usr/lib equivalence). Installed before or with that module,
# the file may keep the label of the policy loaded when it was written.
if [ -x %{_sbindir}/selinuxenabled ] && %{_sbindir}/selinuxenabled; then
    %{_sbindir}/restorecon -R %{_libdir}/basalt-voice %{_sharedstatedir}/basalt-voice >/dev/null 2>&1 || :
fi

%files
%license basalt-licenses/*
%{_libdir}/basalt-voice/
%{_bindir}/basalt-voice-fetch
%{_datadir}/basalt-voice/
%dir %{_sharedstatedir}/basalt-voice
%dir %{_sharedstatedir}/basalt-voice/models

%changelog
* Tue Oct 06 2026 Basalt OS project <noreply@basalt-os.org> - 0.3.0-1
- %posttrans restores the SELinux labels of the speech-to-text program
  (its domain only runs it with basalt-shell-selinux's type).
- basalt-voice-fetch for the desktop's consented downloads (basalt-models):
  the english set (ggml-base.en and Silero VAD), --plan (what a download
  would fetch, sizes and host, without root), --list --porcelain,
  --status FILE (progress while downloading), --remove, and exit codes
  for an unreachable server (3, the partial file is kept and resumed),
  a checksum mismatch (4) and an unwritable model directory (5).

* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.1-1
- basalt-voice-fetch multilingual fetches ggml-base-q5_1 (with Silero
  VAD) instead of ggml-small-q5_1: about three times faster, and it
  heard short spoken commands best in the lab. ggml-small-q5_1 stays in
  the manifest, fetched by name, for long dictation.

* Mon Oct 05 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- Multilingual Whisper models in the manifest (ggml-base, ggml-small and
  their q5_1 quantizations, from the same pinned whisper.cpp revision),
  for a speech language other than English; basalt-voice-fetch
  multilingual fetches ggml-small-q5_1 and Silero VAD. No Portuguese
  Piper voice qualifies (each is fine-tuned from lessac or ryan): answers
  in Portuguese are shown, not spoken.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: whisper.cpp 1.9.4 whisper-cli for the CPU, model
  manifest (Whisper, Silero VAD, public-domain Piper voices) and
  basalt-voice-fetch.
