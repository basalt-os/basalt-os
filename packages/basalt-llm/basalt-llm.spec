# Basalt OS local language model service: llama.cpp's llama-server built for
# the CPU only, a systemd unit without network access, an SELinux domain
# and a download tool for checksum-pinned models. No model is packaged.
#
# Optional and not part of the default package set. Not built by CI (it
# compiles llama.cpp, several minutes): packages/basalt-llm/build.sh.

%global selinuxtype targeted
%global modulename basalt_llm
%global llama_version 0.5.0
# Several CPU variants are built and the best one is picked at run time.
%global debug_package %{nil}
# The libraries are private to the server (RUNPATH $ORIGIN): no
# automatic Provides for them, no Requires on them.
%global __provides_exclude_from ^%{_libdir}/basalt-llm/.*$
%global __requires_exclude ^lib(llama|ggml|mtmd).*$

Name:           basalt-llm
Version:        0.2.0
Release:        1%{?dist}
Summary:        Local language model service for the Basalt OS assistant (no network)
# basalt-llm files: Apache-2.0. llama.cpp and ggml: MIT; bundled
# cpp-httplib, nlohmann/json: MIT.
License:        Apache-2.0 AND MIT
URL:            https://github.com/basalt-os/basalt-os
Source0:        https://github.com/ggml-org/llama.cpp/archive/refs/tags/v%{llama_version}.tar.gz#/llama.cpp-%{llama_version}.tar.gz
Source1:        basalt-llm.service
Source2:        basalt-llm-start
Source3:        basalt-llm-fetch
Source4:        models.manifest
Source5:        llm.conf
Source6:        basalt_llm.te
Source7:        basalt_llm.if
Source8:        basalt_llm.fc
Source9:        LICENSE
Source10:       basalt-llm-select

ExclusiveArch:  x86_64 aarch64
BuildRequires:  cmake
BuildRequires:  gcc-c++
BuildRequires:  systemd-rpm-macros
BuildRequires:  selinux-policy-devel
BuildRequires:  make
BuildRequires:  bzip2
Requires:       systemd
Requires:       curl
Requires:       coreutils
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})
Provides:       bundled(llama-cpp) = %{llama_version}
Provides:       bundled(cpp-httplib)
Provides:       bundled(json)
%{?systemd_requires}

%description
An optional local language model service for the Basalt OS system
assistant: llama.cpp's server, built for the CPU only, started by
basalt-llm.service on a Unix socket, in an empty network namespace and
its own SELinux domain without network access. The assistant uses it, if
enabled, to translate requests in natural language into its commands
(basalt ask) and as an opt-in backend of its decision layer.

No model is included. basalt-llm-fetch downloads a model listed in the
manifest and verifies its SHA-256. By default (MODEL=auto) the service
runs the fine-tuned translator that fits the machine, chosen at each
start: the 1.7B with 4 or more CPU cores and enough free memory, else
the 0.6B.

%package selinux
Summary:        SELinux policy module for the Basalt OS local language model service
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires(post): selinux-policy-%{selinuxtype}
%{?selinux_requires}

%description selinux
SELinux policy module basalt_llm: the domain basalt_llm_t for the model
service, which may read its models and answer on its Unix socket, with no
network access and no transitions out of its domain.

%prep
%setup -q -n llama.cpp-%{llama_version}
mkdir -p selinux
cp -p %{SOURCE6} %{SOURCE7} %{SOURCE8} selinux/

%build
%cmake -DGGML_NATIVE=OFF -DGGML_BACKEND_DL=ON -DGGML_CPU_ALL_VARIANTS=ON \
       -DLLAMA_OPENSSL=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
       -DLLAMA_BUILD_SERVER=ON -DBUILD_SHARED_LIBS=ON \
       -DCMAKE_SKIP_INSTALL_RPATH=OFF -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON -DCMAKE_INSTALL_RPATH='$ORIGIN'
%cmake_build
make -C selinux -f %{_datadir}/selinux/devel/Makefile %{modulename}.pp
bzip2 -9 selinux/%{modulename}.pp

%install
# The server, its libraries and the CPU backends in one private directory
# (RUNPATH $ORIGIN; ggml loads the backends from the program's directory).
install -d %{buildroot}%{_libdir}/basalt-llm
install -pm 0755 %{__cmake_builddir}/bin/llama-server %{buildroot}%{_libdir}/basalt-llm/
for f in %{__cmake_builddir}/bin/lib*.so*; do
    case "$(basename "$f")" in
        libllama-*-impl.so) [ "$(basename "$f")" = libllama-server-impl.so ] || continue ;;
    esac
    cp -P "$f" %{buildroot}%{_libdir}/basalt-llm/
done
install -Dpm 0755 %{SOURCE2} %{buildroot}%{_libexecdir}/basalt-llm/basalt-llm-start
%{_libexecdir}/basalt-llm/basalt-llm-select
install -Dpm 0644 %{SOURCE10} %{buildroot}%{_libexecdir}/basalt-llm/basalt-llm-select
install -Dpm 0755 %{SOURCE3} %{buildroot}%{_bindir}/basalt-llm-fetch
install -Dpm 0644 %{SOURCE4} %{buildroot}%{_datadir}/basalt-llm/models.manifest
install -Dpm 0644 %{SOURCE5} %{buildroot}%{_sysconfdir}/basalt/llm.conf
install -Dpm 0644 %{SOURCE1} %{buildroot}%{_unitdir}/basalt-llm.service
install -dm 0755 %{buildroot}%{_sharedstatedir}/basalt-llm/models
install -Dpm 0644 selinux/%{modulename}.pp.bz2 %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2
install -Dpm 0644 selinux/%{modulename}.if %{buildroot}%{_datadir}/selinux/devel/include/distributed/%{modulename}.if
install -d basalt-licenses
install -pm 0644 %{SOURCE9} basalt-licenses/LICENSE
install -pm 0644 LICENSE basalt-licenses/llama.cpp.LICENSE
install -pm 0644 licenses/LICENSE-jsonhpp basalt-licenses/nlohmann-json.LICENSE
install -pm 0644 vendor/cpp-httplib/LICENSE basalt-licenses/cpp-httplib.LICENSE

%post
%systemd_post basalt-llm.service

%preun
%systemd_preun basalt-llm.service

%postun
%systemd_postun_with_restart basalt-llm.service

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
%license basalt-licenses/*
%{_libdir}/basalt-llm/
%dir %{_libexecdir}/basalt-llm
%{_libexecdir}/basalt-llm/basalt-llm-start
%{_bindir}/basalt-llm-fetch
%{_datadir}/basalt-llm/
%dir %{_sysconfdir}/basalt
%config(noreplace) %{_sysconfdir}/basalt/llm.conf
%{_unitdir}/basalt-llm.service
%dir %{_sharedstatedir}/basalt-llm
%dir %{_sharedstatedir}/basalt-llm/models

%files selinux
%{_datadir}/selinux/packages/%{selinuxtype}/%{modulename}.pp.bz2
%{_datadir}/selinux/devel/include/distributed/%{modulename}.if
%ghost %verify(not md5 size mode mtime) %{_sharedstatedir}/selinux/%{selinuxtype}/active/modules/200/%{modulename}

%changelog
* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- MODEL=auto (new default): the fine-tuned translator that fits the
  machine, chosen and logged at each start: 1.7B Q8_0 with 4 or more CPU
  cores, 3584 MiB available and a service memory limit of 3072 MiB or
  more, else 0.6B Q8_0. MODEL=0.6b or 1.7b selects one explicitly.
- Manifest entries for both fine-tuned translators, marked unpublished
  until a signed release: basalt-llm-fetch refuses them (fail closed).

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: llama.cpp 0.5.0 server (CPU only), systemd unit
  without network, SELinux domain basalt_llm_t, model download tool.
