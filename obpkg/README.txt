OpenBasalt packages (obpkg.org)

Release key: https://obpkg.org/keys/openbasalt-release-key.asc
Fingerprint: 3601 7348 42BD 4E48 2D19  DE4A E4EE D5EC A395 B302

Packages subkey: 3024 61D2 6520 E077 D07F  FCA9 AA27 C62C 36CC FC4B

Repositories:
  /basalt/          Basalt OS packages (44: x86_64, source). Available.
  /basalt-tools/    Official OpenBasalt tools, today Samba Conductor
                    (44: x86_64). Available.
  /basalt-testing/  Packages on their way to /basalt/. Coming soon.
  /apt/             Samba Conductor packages for Debian and Ubuntu. Coming soon.

Basalt OS installs come with /basalt/ preconfigured (basalt-release).

basalt-tools: Samba Conductor packages (conductor, conductor-idp,
conductor-sync, conductor-backup, conductor-files and their -selinux
packages), signed by the packages subkey above.
https://github.com/openbasalt/samba-conductor
An upcoming basalt-release will enable it by default on Basalt OS. Until
then, save this as /etc/yum.repos.d/basalt-tools.repo:

[basalt-tools]
name=Basalt OS tools $releasever - $basearch
baseurl=https://obpkg.org/basalt-tools/$releasever/$basearch/
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://obpkg.org/keys/openbasalt-release-key.asc
metadata_expire=6h

Then: sudo dnf install conductor

Setup instructions: https://obpkg.org/
See https://openbasalt.org and https://basalt-os.org
