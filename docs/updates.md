# Updates and channels

Settings, Updates and channels (pt-BR "Atualizações e canais") in the
desktop shell, and `basalt updates` and `basalt channels` on the command
line, keep Basalt OS up to date and say where its software comes from.
Nobody needs a terminal for it. Every change is one of the system
assistant's proposals from its closed set of actions: the person sees it
in plain words, with the exact commands under Details, and confirms it
with an administrator's password (or the approval gate decides it, where
it does). The desktop never runs dnf and never writes a repository file.

```
basalt updates                     what the last check found, grouped; restart; history
sudo basalt updates check          refresh the package lists (update.check)
sudo basalt updates install [--security] [--apply]
sudo basalt updates rollback [--apply]
basalt channels                    channels, added sources, other repositories, the catalog
sudo basalt channels enable basalt-testing --consent preview-builds-1 [--apply]
sudo basalt channels disable basalt-testing [--apply]
sudo basalt channels add vscode | copr --id OWNER/PROJECT | custom --repo-url URL [--apply]
sudo basalt channels remove vscode [--apply]
```

## Actions

| Action | Commands | Class at the gate | Snapshot |
|---|---|---|---|
| `update.check` | `dnf makecache --refresh` | Look (C0) | no: nothing is installed |
| `update.install` | live: `dnf -y upgrade --downloadonly NEVRA...`, `dnf -y upgrade NEVRA...`; offline: `dnf -y upgrade --offline NEVRA...`, then `dnf -y offline reboot` | System change (C2) | before and after (offline: the after one at the next start) |
| `update.rollback` | `basalt-rollback --yes N` | System change (C2) | works on snapshots |
| `repo.enable`, `repo.disable` | `dnf config-manager setopt REPO.enabled=1` (or `=0`); for a basalt-nonfree channel not yet defined, first `dnf -y install` and `upgrade basalt-nonfree-release` | System change (C2) | no: one setting |
| `source.add` | `basalt __source add ...` | Critical (C4): a new trust root | before and after |
| `source.remove` | `basalt __source remove --id ID` | System change (C2) | before and after |

Validators (`internal/action`, `internal/sources`):

- `update.install` names exactly the packages the person saw, as
  `name-[epoch:]version-release.arch`, sorted, with their count and a
  digest of the list; options, globs and duplicates are refused, and at
  most 1500 packages go in one proposal. The check after the install
  confirms that each of them is installed.
- `update.rollback` is the snapshot `basalt apply` took before an applied
  `update.install`; `basalt updates rollback` refuses anything else. Like
  every rollback it needs the root view: from the confined daemon or the
  MCP server it is only a hint.
- `repo.enable` and `repo.disable` accept basalt-tools, basalt-testing and
  basalt-nonfree-testing, and sources added through Basalt; basalt (always
  on), basalt-nonfree (Settings, Additional drivers turns it on with the
  driver), Fedora's repositories and anything else are refused. Turning on
  a testing channel needs `consent=preview-builds-1`: the person was shown
  "Preview builds can break things. A snapshot is taken before each update
  so you can go back."
- `source.add` refuses plain http (for the source and its key), a missing
  key, signature checks off (`gpgcheck` is always 1; a custom source also
  needs `repo_gpgcheck=1`), an id that would shadow a Fedora or Basalt
  repository, characters that could change how a repository file or a
  command line reads, and, for a catalog entry, any address, key address,
  fingerprint or metadata setting other than the pinned one.
- `source.remove` acts only on a source Basalt added (its record in
  `/etc/basalt/sources.d`).

`basalt __source add` runs as a confirmed proposal's command, as root:
it downloads the key again (https only, also after redirects, 1 MiB at
most), refuses a file with more than one key or a fingerprint other than
the confirmed one, writes the key to
`/etc/pki/rpm-gpg/basalt-source-GROUP.asc` and imports it into rpm, writes
`/etc/yum.repos.d/basalt-source-ID.repo` (`gpgcheck=1`, the confirmed
`repo_gpgcheck`, `gpgkey=file://...`, `skip_if_unavailable=True`) or adds
the Flatpak remote system wide from its `.flatpakrepo`, and records the
source in `/etc/basalt/sources.d/ID.json`. Once the apply verified it,
`basalt apply` adds which proposal added it and who decided. Removing a
source removes the repository file (or the remote; flatpak refuses while
apps from it are installed), the record, and, with the group's last
repository, the key file and the key in rpm's database.

## How the desktop applies a proposal

The desktop's own domains never run dnf, rpm or basalt apply. Where the
approval gate decides the assistant's proposals, the shell queues the
proposal there and the gate's executor (basalt-gate-exec@REQUEST.service,
basalt_gate_exec_t) applies it. Otherwise the shell starts the
assistant's unit `basalt-apply@ID_CODE.service`; polkit asks for an
administrator's password (rule 50-basalt-assistant.rules) and
basalt-apply-exec runs `basalt apply ID --yes --confirm CODE` in
basalt_apply_t. Both executors start dnf in rpm_t, so package scriptlets
run in rpm_script_t. Checking for updates starts
`basalt-updates-check.service` (no password for an administrator at the
computer); it refreshes the package lists and the report the assistant
keeps in `/var/lib/basalt-assistant/updates.json`, which is all the desktop
reads. The Additional drivers page applies its proposals the same way, and
its report is written by `basalt-drivers-refresh.service` (`basalt drivers
refresh`, root) to `/var/lib/basalt-assistant/drivers.json`; the desktop's
read helper only asks `basalt drivers --json --cached` (and `drivers
install` or `rollback` with `--cached`), so no rpm or dnf runs in the
desktop's domain. A test of the assistant runs every request the read
helper allows against a runner that refuses rpm and dnf.

## Offline updates

A set that replaces core packages (the kernel, systemd, dbus, glibc, the
package manager, the SELinux policy, basalt-shell, basalt-greeter,
basalt-gate, Mesa, the compositor and the session's shell) installs
offline: update.install carries `mode=offline` (the validator refuses a
live install of such a set), the packages are downloaded and checked now,
the snapshot before is taken, and the update stays staged. The restart
into it never happens without warning: Settings opens the power menu's 60
second countdown ("Restarting to install updates", focused on Cancel);
when it ends, or with Restart now, the desktop starts
basalt-offline-reboot.service, which runs `basalt updates restart` (`dnf
offline reboot`, refused unless an update is staged). Cancel keeps it
staged and the page offers "Restart and update" again; at a terminal,
`sudo basalt updates restart`. dnf installs it before the session starts
(system-update.target). A start that did not go through it leaves it
staged. At the next start
basalt-offline-finish.service takes the snapshot after, checks that every
package is installed and records the result; the page offers undo as for
a live update. Smaller sets without core packages stay live. The page
says it in one sentence and its button is "Restart and update".

## Channels

| Channel | What | Default | Who changes it |
|---|---|---|---|
| basalt | the packages that make Basalt OS | on | nobody: always on |
| basalt-tools | optional OpenBasalt tools | on | Settings, a toggle |
| basalt-testing | preview builds of Basalt OS components | off | Settings, a toggle with consent |
| basalt-nonfree | non-free drivers (docs/nvidia.md) | off | Settings, Additional drivers, with the driver |
| basalt-nonfree-testing | preview builds of the non-free drivers | off | Settings, a toggle with consent; shown on hardware the driver supports, or after "Show all channels" |

basalt-release defines the first three, basalt-nonfree-release the other
two. Each card says what the channel is, who it is for and the risk, and
shows how it is signed (the OpenBasalt release key, short form E4EE D5EC
A395 B302, read from the key file the repository names). A repository that
is neither Fedora's nor Basalt's and was not added through Basalt is
listed, not changed.

## Sources

The catalog (`internal/sources/catalog.go`) pins each entry's address and
key fingerprint as the publisher documents them: Flathub (Flatpak), RPM
Fusion free and nonfree (two repositories each), Google Chrome, Visual
Studio Code and Docker CE. A COPR project is added by name: its address
and key address are the project's own on download.copr.fedorainfracloud.org,
and its key, not pinned, is shown for the person to compare. A custom
source is a `.repo` address (its first repository) or an address with a
key address and a name. In every case the key is downloaded when the
proposal is made, so the confirmation shows its fingerprint and owner.

The network: dnf runs as the system, not inside an agent session, so the
per-session egress allowlists of basalt-resolver (docs/network.md) do not
apply to it and adding a source changes none of them. An agent session
still cannot reach a source's host unless its own allowlist says so.

## Updates

`basalt updates` reads dnf's cached metadata (refreshed by update.check),
`dnf advisory list --updates --json` and the rpm database, and groups the
updates: security (an advisory of type security, with its severity),
Basalt OS components (from a basalt channel), apps (packages that own an
entry of the app launcher) and system (everything else), with sizes. A
restart is needed when one of the packages dnf's needs-restarting watches
(the kernel, glibc, systemd, dbus, openssl and the others) was installed
after the last boot, or when a rollback waits for the next boot. History
lists the update proposals applied, failed or undone, and offers to undo
the last update that still has its snapshot.

While a proposal is applied, `basalt apply` records its step (the
snapshot, each command, the checks) in the proposal, so the page shows
"Step 2 of 4: download 378 updates".

Automatic security updates are off by default. They become a rule of the
approval gate once it starts scheduled jobs (docs/gate.md, Limits); until
then the page shows the option without turning it on.

## Lab

`lab/updates/` in the basalt-shell repository: a VM on a qcow2 overlay of
an installed desktop disk, no TPM device, and the development install of
the shell and the assistant's command line.
