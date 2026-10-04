OpenBasalt packages (obpkg.org)

Release key: https://obpkg.org/keys/openbasalt-release-key.asc
Fingerprint: 3601 7348 42BD 4E48 2D19  DE4A E4EE D5EC A395 B302

Packages subkey: 3024 61D2 6520 E077 D07F  FCA9 AA27 C62C 36CC FC4B

Repositories:
  /basalt/          Basalt OS packages (44: x86_64, source). Available.
  /basalt-tools/    Official OpenBasalt tools (44: x86_64). Available.
  /basalt-testing/  Packages on their way to /basalt/ (44: x86_64).
                    Opt-in, off by default. Available.
  /apt/             OpenBasalt packages for Debian and Ubuntu. Coming soon.

Basalt OS installs come with /basalt/, /basalt-tools/ (on) and
/basalt-testing/ (off) preconfigured by basalt-release.

basalt-tools: official OpenBasalt tools for Basalt OS and other
Fedora-based systems where they apply, signed by the packages subkey
above. More tools will be added over time.
Currently: Samba Conductor (conductor, conductor-idp, conductor-sync,
conductor-backup, conductor-files and their -selinux packages),
https://github.com/openbasalt/samba-conductor

On Basalt OS it is enabled by default since basalt-release 44-7:
  sudo dnf install conductor
Turn it off with: sudo dnf config-manager setopt basalt-tools.enabled=0

On other Fedora-based systems, save this as
/etc/yum.repos.d/basalt-tools.repo:

[basalt-tools]
name=Basalt OS tools $releasever - $basearch
baseurl=https://obpkg.org/basalt-tools/$releasever/$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://obpkg.org/keys/openbasalt-release-key.asc
metadata_expire=6h

Then install a tool, for example: sudo dnf install conductor

basalt-testing: packages on their way to /basalt/, opt-in and off by
default. Currently: the Basalt desktop shell preview (basalt-shell and
basalt-shell-selinux), https://github.com/basalt-os/basalt-shell
On Basalt OS:
  sudo dnf config-manager setopt basalt-testing.enabled=1
  sudo dnf install basalt-shell
Or for a single command: sudo dnf --enablerepo=basalt-testing install basalt-shell
(config-manager comes from dnf5-plugins on Fedora 44.)

Setup instructions: https://obpkg.org/
See https://openbasalt.org and https://basalt-os.org
