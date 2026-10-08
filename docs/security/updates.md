# How updates are verified

Settings, Updates and channels links here. This page says, in plain
words, how Basalt OS knows that an update really comes from where it
says, and how you can check it yourself.

## Every package and every package list is signed

Basalt OS gets its software from repositories: Fedora's, Basalt's own
channels and the other sources you add. dnf checks two signatures before
it installs anything:

- the package list of a repository (its metadata, `repomd.xml`) is signed,
  so nobody between you and the repository can add, remove or swap
  packages in the list (`repo_gpgcheck=1`);
- each package is signed, so a package that was changed after it was
  built does not install (`gpgcheck=1`).

Basalt's channels (basalt, basalt-tools, basalt-testing, basalt-nonfree,
basalt-nonfree-testing) are all signed with the OpenBasalt release key:

```
OpenBasalt release key <openbasalt@openbasalt.org>
3601 7348 42BD 4E48 2D19  DE4A E4EE D5EC A395 B302
```

Settings shows its short form, the last 16 digits: E4EE D5EC A395 B302.
The same key is published at https://obpkg.org/keys/openbasalt-release-key.asc
and installed by basalt-release as `/etc/pki/rpm-gpg/RPM-GPG-KEY-basalt`.
How the key is kept and used: [key-ceremony.md](../key-ceremony.md).

These signature checks do not depend on the repository files in
`/etc/yum.repos.d`, which an upgrade never replaces once they were edited:
basalt-release (and basalt-nonfree-release) ship them as dnf vendor
overrides in `/usr/share/dnf5/repos.override.d`, applied after those files
and replaced by every upgrade. Only an administrator's own file under
`/etc/dnf/repos.override.d` can change them.

Fedora's repositories are signed with the Fedora key of each release,
which Fedora publishes at https://fedoraproject.org/security/.

## A testing channel is still signed

basalt-testing and basalt-nonfree-testing hold preview builds: software on
its way to the stable channels. They are off by default. Turning one on
asks for your consent ("Preview builds can break things. A snapshot is
taken before each update so you can go back.") and goes through the same
confirmation as every other change. Their packages are signed with the
same OpenBasalt release key: preview does not mean unsigned.

## Other sources

Settings, Updates and channels can add other software sources: a short
catalog of well-known ones (Flathub, RPM Fusion, Google Chrome, Visual
Studio Code, Docker CE), a Fedora COPR project by name, or a custom source
by its address. Each source has its own signing key, and adding the source
means trusting that key for your whole system. So:

- only https addresses are accepted, for the source and for its key;
- a source must have a signing key, and package signature checks stay on
  (`gpgcheck=1`) for every source; a custom source must also sign its
  package lists (`repo_gpgcheck=1`). RPM Fusion and COPR do not sign their
  package lists; their packages are signed and RPM Fusion's lists come
  from a metalink over https, which carries their checksums;
- for a catalog source, Basalt OS ships the address and the key's
  fingerprint as its publisher documents them, and refuses a key with any
  other fingerprint;
- the key's fingerprint and owner are shown in plain words before you
  confirm, and the key is downloaded and checked again when the source is
  added;
- adding a source asks for an administrator's password, and it is
  recorded: who decided, when, which key (`basalt channels` and the
  activity log show it).

## A snapshot before every update

Installing updates from Settings takes a snapshot of the system first
(snapper, on btrfs). "Undo the last update" returns the system to that
snapshot at the next start; your files in /home and the other data
volumes are not touched.

## Check it yourself

```sh
basalt channels                 # every channel and source, and the key that signs it
gpg --show-keys /etc/pki/rpm-gpg/RPM-GPG-KEY-basalt
dnf repo info basalt            # gpgcheck and repo_gpgcheck of a repository
rpm -K /var/cache/libdnf5/basalt-*/packages/*.rpm   # package signatures in the cache
```

Compare the fingerprint with the one above and with the one published
with the key. For a source you added, compare it with the one its
publisher shows.
