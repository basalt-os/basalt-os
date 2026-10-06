# The Basalt OS system assistant

Status: pre-alpha, milestone 2c. The assistant diagnoses the system and
proposes fixes without any language model, and explains them in plain,
friendly English (see How it explains). Its decision layer answers with
VSM, a small diagnosis engine with a signed knowledge index, and falls
back to deterministic rules (see Decision layer). An optional local model (package
`basalt-llm`, [local-model.md](local-model.md)) can translate requests in
natural language into the commands below (`basalt ask`), can answer the
decision layer's questions, and can write the explanation of a finding in
its own words (humanize); all three are off by default and nothing below
depends on them. The assistant's text is in English; the feedback flow
(`basalt feedback`) already goes through translation catalogs (see
Translations), and the rest moves there next.

Package: `basalt-assistant` (and `basalt-assistant-selinux`), source in
`packages/basalt-assistant/` (Go, no third-party modules). The installer
puts it on every system and enables `basalt-assistantd`: the daemon only
reads and proposes, and every change still goes through `basalt apply` and
a confirmation. The local model service (`basalt-llm`) stays optional and
is not installed by default.

## What it does

```
journal, audit, unit state,          diagnosers            decision layer        proposal (typed actions)
SELinux policy, snapshots, statfs -> (shared by CLI,   ->  (typed questions,  -> stored in /var/lib/basalt-assistant
                                      daemon, MCP)          probabilities)              |
                                                                                       v
                                       basalt apply ID: exact commands shown, confirmation,
                                       snapshot before and after, run, verify, audit record
```

- `basalt` (command line) reads and explains. A diagnosis that finds a fix
  stores it as a proposal (when run as root) and prints the exact commands.
- `basalt-assistantd` (systemd service) watches for events and stores a
  diagnosis with a proposed fix for each one. It never changes the system.
- `basalt-mcp` (MCP server over stdio) exposes the same diagnosers as tools
  to a future local model or to an external MCP client. Its write tools only
  store proposals.
- `basalt-notify` (systemd service) delivers findings beyond the journal:
  desktop notifications and an optional webhook (see Notifications).
- Only `basalt apply`, run by an administrator, changes anything.

## How it explains

Every finding is written from its structured facts (the cause and the
values the diagnosers found: unit, file and line, snapshot, port, sizes)
by fixed templates, in the same order:

```
[p-1a2b3c] nginx.service stopped: configuration error
Waiting for your decision, found by basalt on 2026-10-04 10:15 UTC

  nginx.service is not running because of a mistake in /etc/nginx/nginx.conf
  (line 47). The configuration check nginx -t says: unknown directive
  "bogus_directive" in /etc/nginx/nginx.conf:47. If you apply it, the
  assistant will put back the copy of /etc/nginx/nginx.conf from snapshot 27
  and then restart nginx.service.

What will run (exactly these commands, as root, in this order)
  $ cp --preserve=mode,ownership,timestamps /.snapshots/27/snapshot/etc/nginx/nginx.conf /etc/nginx/nginx.conf
  $ restorecon -v /etc/nginx/nginx.conf
  $ systemctl restart nginx.service

Risk: medium (replaces the current /etc/nginx/nginx.conf).
Undo: a snapshot is taken before and after, so you can go back to the state
before the change; a rollback takes effect at the next boot.
  $ sudo basalt snapshots rollback --before p-1a2b3c

Next step
  Apply it:   sudo basalt apply p-1a2b3c
              (without a prompt: sudo basalt apply p-1a2b3c --yes --confirm 3c140f0a)
  Ignore it:  sudo basalt ignore p-1a2b3c

Evidence
  - ...
```

- One paragraph says what is wrong, the main evidence, and what applying
  will do. Commands, paths and values are never reworded: the commands
  are rebuilt from the typed actions into their own block.
- Risk is the highest level of the planned actions (low: a relabel to the
  policy default, a restart, a package cache clean; medium: a lasting
  SELinux rule, label or boolean, a restored configuration file, a journal
  vacuum; high: a rollback, deleting a snapshot), with the reason.
- Undo says what the snapshots taken by `basalt apply` give back and what
  they do not: files on the data subvolumes (`/srv`, `/home`, `/var/log`
  and the others), a deleted snapshot, vacuumed journal files.
- Length follows severity: a running unit or a disk below its thresholds
  is two lines; a report without a change has no command block; evidence
  is capped at 8 lines for warnings and 25 for failures (`--verbose`
  shows all of it and the decisions behind the proposal, which are also
  shown when a proposal needs review).
- `basalt status`, `basalt disk`, `basalt snapshots`, `basalt pending` and
  the apply flow use the same plain wording; `--json` is unchanged.

The templates live in `internal/explain` (wording, risk, undo) and
`internal/report` (layout); golden files in
`internal/report/testdata/golden` and `internal/cli/testdata/golden` pin
every kind of finding (`go test ./internal/report -update` rewrites
them after a deliberate change).

### Humanize (optional)

With `[humanize] enabled = yes` a language model writes the explanation
paragraph in its own words, and only that paragraph. The model receives
the facts and the planned actions as JSON (never command lines); the
template keeps the title, the command block, the risk, the undo, the
apply line and the evidence. The model's text is kept only if it passes a
faithfulness check, otherwise the template's paragraph is shown with a
note:

- every number (in digits or words), path, file name, unit name, SELinux
  type or boolean, and proposal id must appear in the facts;
- no command word (`systemctl`, `semanage`, `dnf`, `rm`, `sudo` and the
  others) unless the facts hold it, no shell or markup characters, no
  address or URL, no advice the assistant never gives (disabling SELinux,
  permissive mode, `audit2allow`);
- it must name the unit it is about, use Latin script, and stay under
  `max_chars` (600 by default).

On a terminal, accepted sentences are shown as they arrive: each sentence
is checked before it is printed, and the first one that fails stops the
model. `basalt apply` always shows the template, and so do `--json`,
`--plain`, the daemon's journal and the MCP server. Each humanized text is
recorded in the audit log (`humanize`: accepted or not, the problems found,
the time taken). The endpoint may be the local model service, a bigger
local server or, by explicit opt-in, a remote provider; see
[local-model.md](local-model.md).

## Commands

Read-only, no confirmation:

| Command | What it reports |
|---|---|
| `basalt status` | SELinux mode, failed units, denials in 24 h, root file system usage, snapshots, unfinished package transactions, pending rollback, daemon state, pending proposals, audit chain |
| `basalt why UNIT` | unit state and result; relevant journal lines; the unit's SELinux domain (from the loaded policy: `SELinuxContext=` or the transition from `init_t` for its executable's label) and the denials for it; for a "Permission denied", the mode and owner of every component of the path against the unit's `User=` (file permissions, DAC) and, for a confined domain without any logged denial, a label check of every component (dontaudit rules hide many denials); out-of-memory kills (result `oom-kill`, systemd's and the kernel's messages) with the unit's memory limits; crashes (a signal or a core dump) with the unit's recent history: how long the process ran, systemd's automatic restarts, the start limit and how often it ended the same way in the last 24 hours. A unit that crashes again and again or right as it starts gets no restart, only that evidence and how to investigate it (its log, `coredumpctl info`); a restart is proposed only for a first crash after it ran for a while; failed dependencies; ports named in the failure and who holds them; full file systems; the service's own config checker (`nginx -t`, `httpd -t`, `sshd -t`, `named-checkconf`, `haproxy -c`, `postfix check`, `testparm`, and others, run without side effects, see below) with the file and line of the error; the newest snapshot copy of the broken file that passes the checker |
| `basalt fix selinux [--since 1h]` | recent denials grouped by domain, target, class, permission and object, each mapped to a known fix or to review |
| `basalt snapshots` | the root snapshots; `[proposal]` marks the ones `basalt apply` took |
| `basalt snapshots diff A [B]` | packages added, removed and changed (rpm databases of the two snapshots) and changed files by directory (`snapper status`) |
| `basalt disk` | btrfs usage, the space each snapshot holds alone (`btrfs filesystem du`), journal and package cache size, a fullness forecast from stored samples |
| `basalt pending [--all]`, `basalt show ID` | proposals |
| `basalt audit [N]`, `basalt audit verify` | the audit log and its hash chain, checked across rotated files |
| `basalt drivers` | the display controllers by PCI id and their kernel driver, the driver that fits (the NVIDIA driver of basalt-nonfree for Turing and newer GPUs, from NVIDIA's list of supported GPUs), what installing it changes, the Secure Boot state, and the state of an installed NVIDIA driver (waiting for its first start, in use, fell back to nouveau and why, a kernel held back); `basalt drivers license nvidia` prints the NVIDIA Driver License Agreement (docs/nvidia.md) |
| `basalt updates` | what the last check for updates found, grouped (security, Basalt OS components, apps, system) with sizes and advisories, whether a restart is needed, an update being installed, the history and the last update that can be undone ([updates.md](updates.md)) |
| `basalt channels` | Basalt's channels and how each is signed, the sources added through Basalt, other repositories, the catalog of well-known sources |
| `basalt ask "REQUEST"` | (optional, needs the local model) the request translated into one of the commands above, which then runs; a change (apply, rollback) is only printed, see [local-model.md](local-model.md) |

Options: `--json` (machine-readable output), `--verbose` (all evidence and
the decisions), `--plain` (the template text even with humanize on),
`--config FILE`.

Changes (root):

| Command | Change |
|---|---|
| `basalt apply ID` | runs a proposal after confirmation |
| `basalt ignore ID [--reason TEXT]` | closes a proposal without running it (recorded) |
| `basalt confirm ID` | checks, as root, the snapshot a hint of the daemon or the MCP server rests on (see Hints) and stores it as a proposal |
| `basalt snapshots rollback N` or `--before ID` | proposes and runs `basalt-rollback N`; `--before` uses the snapshot `basalt apply` took before proposal ID |
| `basalt why UNIT --apply`, `basalt fix selinux --apply`, `basalt disk --apply` | store the proposal and go straight to the confirmation |
| `basalt drivers install nvidia [display\|compute] [--apply]` | the `driver.install` proposal for the recommended driver (refused without a supported GPU, while the basalt-nonfree repository is not published, when it is installed already, or with Secure Boot on and the Basalt module CA not enrolled) |
| `basalt drivers rollback [--apply]` | the rollback to the snapshot `basalt apply` took before the NVIDIA driver install |
| `basalt updates check` | update.check: refresh the package lists (`dnf makecache --refresh`), without a proposal: nothing is installed; recorded as a `check` audit record |
| `basalt updates install [--security] [--apply]` | the `update.install` proposal for exactly the updates shown (all, or the security ones) |
| `basalt updates rollback [--apply]` | the `update.rollback` proposal: back to the snapshot taken before the last update |
| `basalt channels enable\|disable NAME [--consent preview-builds-1] [--apply]` | `repo.enable` or `repo.disable` of basalt-tools, basalt-testing, basalt-nonfree-testing or a source added through Basalt (testing channels need the consent) |
| `basalt channels add ENTRY\|copr --id OWNER/PROJECT\|custom ... [--apply]` | the `source.add` proposal: the key is downloaded now and shown with its fingerprint and owner |
| `basalt channels remove SOURCE [--apply]` | the `source.remove` proposal |

Other Basalt tools through `basalt`: a first word that is not one of the
commands above runs the program `basalt-<word>` with the rest of the
command line, so `basalt ledger summary --since today` is `basalt-ledger
summary --since today` (docs/ledger.md). Only an executable regular file
named `basalt-<word>` in `/usr/libexec/basalt` or `/usr/bin` (searched in
that order) is run; `PATH` is never consulted, so a program a user or an
agent puts on its own `PATH` cannot be reached this way. The word must be
lower case letters, digits and single hyphens. The commands above always
win, and the assistant's own helpers in `/usr/libexec/basalt`
(`basalt-assistantd`, `basalt-notify`, `basalt-policy-query`) are not
offered. The program replaces the `basalt` process (same user, same
environment, no privilege change); `basalt-ledger` decides what you may
read, exactly as when you call it by its own name.


## Proposals are typed actions

A proposal stores what is wrong, the evidence, the decisions behind it and
a list of actions from a closed set. The commands are rebuilt from the
actions every time they are shown or run, so a proposal file (written by
the daemon, an MCP client or anyone else) can only ever express these
changes, with parameters that pass strict validators:

| Action | Commands |
|---|---|
| `selinux.fcontext` | `semanage fcontext -a -t TYPE 'DIR(/.*)?'`, `restorecon -Rv DIR` |
| `selinux.restorecon` | `restorecon -v PATH` (`-Rv` for a directory) |
| `selinux.port` | `semanage port -a` (or `-m`) `-t TYPE -p PROTO PORT` |
| `selinux.boolean` | `setsebool -P NAME on` |
| `unit.restart` | `systemctl restart UNIT` |
| `file.restore` | `cp --preserve=mode,ownership,timestamps /.snapshots/N/snapshot/PATH PATH`, `restorecon -v PATH` (only files under `/etc`) |
| `snapshot.rollback` | `basalt-rollback --yes N` |
| `snapshot.delete` | `snapper -c root delete N` |
| `journal.vacuum` | `journalctl --vacuum-size=SIZE` |
| `dnf.clean` | `dnf clean packages` |
| `driver.install` | `dnf -y install basalt-nonfree-release`, `dnf config-manager setopt basalt-nonfree.enabled=1`, `dnf -y install --skip-unavailable nvidia-driver` (or `nvidia-driver-compute`) `kmod-nvidia-open-KERNEL`, `basalt-nvidia arm` (docs/nvidia.md) |
| `update.check` | `dnf makecache --refresh` (run by `basalt updates check` without a proposal) |
| `update.install` | `dnf -y upgrade --downloadonly NEVRA...`, `dnf -y upgrade NEVRA...` (exactly the packages shown) |
| `update.rollback` | `basalt-rollback --yes N` (N: the snapshot taken before an applied update.install) |
| `repo.enable`, `repo.disable` | `dnf config-manager setopt REPO.enabled=1` (or `0`); a basalt-nonfree channel not yet defined first gets `dnf -y install` and `dnf -y upgrade basalt-nonfree-release` |
| `source.add` | `basalt __source add ...`: download the key again, refuse another fingerprint, write the key, the repository file (or the Flatpak remote) and the record |
| `source.remove` | `basalt __source remove --id ID` |

Each action also carries its verification: the label is the policy
default (`matchpathcon -V`), the port or boolean has the new value, the
unit is active and no new denial appeared since the change, the restored
file matches the snapshot, a rollback is pending, the snapshot is gone,
the driver's repository is on, its packages are installed, nouveau is off
and the next start is a trial, every update shown is installed, the
channel is on or off, the source is recorded or gone.

The validators of the update and channel actions are in
[updates.md](updates.md): an update names exactly the packages shown; a
channel toggle accepts only Basalt's toggleable channels and sources added
through Basalt, and a testing channel needs the person's consent; a new
source needs https, its key and signature checks on, and a catalog entry
its pinned key.

`driver.install` also carries the SHA-256 of the NVIDIA Driver License
Agreement the person was shown (`license`); any other value is refused.
Its risk is high (the graphics driver changes at the next start); the
first start checks the driver and falls back to nouveau when it fails
(docs/nvidia.md).

There is no free-form command action and no `audit2allow`: a denial
without a known fix is reported for review, never turned into policy.

A proposal is validated when it is stored and again when it is applied:
every action must pass its validator, and a proposal of the confined view
may not carry the two actions below.

### Hints: restores and rollbacks from the confined view

A `file.restore` or a `snapshot.rollback` is only as good as the snapshot
it rests on, and the confined view (the daemon, the MCP server) cannot
check it: it sees snapshot metadata, not the file contents or the package
databases. From there these changes are hints, not actions: the proposal
holds the actions as a hint (a restore together with the restart that
needs it), is marked for review and has nothing to apply. `basalt confirm
ID`, run as root, checks the hint: the snapshot exists, its copy of the
file differs from the current one and passes the service's config checker
in place of it (in the sandbox), or, for a rollback, what it changes in
packages and files. A confirmed hint is stored as a new proposal from the
command line (the hint is closed as resolved) and applied as usual; a
hint whose copy fails the checker is refused. `basalt why UNIT` as root
reaches the same proposal directly.

## Diagnosis without side effects

Config checkers are not read-only: `nginx -t` opens, and so creates, the
log files its configuration names, and `postfix check` creates missing
directories. Run as root during a diagnosis, that would change the fault
being diagnosed. `basalt` therefore runs every checker through a hidden
subcommand (`basalt __sandbox`) in a private mount namespace where every
disk-backed and tmpfs file system is an overlay with a throwaway upper
layer (256 MiB of tmpfs): the checker sees the real files and may create
and write files as usual, and all of it disappears with the namespace. A
file system that cannot be overlaid is bound read-only; `/proc`, `/sys`
and `/dev` are bound as they are. The same tree can hold another copy of
one file, which is how a snapshot copy is tested in place of the current
one. It needs root; without it (or when the namespace cannot be set up)
the checker is not run and the report says so. A check that writes more
than the throwaway layer holds is reported as inconclusive.

The SELinux policy queries (`sesearch`, `seinfo`) read
`/sys/fs/selinux/policy`, which one process at a time may open: when the
daemon and the command line diagnose the same event, one of them gets
EBUSY. Every query is retried with a jittered exponential backoff (10
attempts, about 14 s); one that still fails is an error and the diagnosis
is incomplete (no change is proposed, `basalt why` and `basalt fix
selinux` exit with an error), never a "no rule allows it".

Loading the policy is quick; what costs is walking its rules (some 330 000
on Fedora 44, about 1.5 s per `sesearch` call on a small VM). The
assistant therefore:

- asks the queries an analysis needs together:
  `/usr/libexec/basalt/basalt-policy-query` (python3-setools) answers a
  batch in one walk, with the same output `sesearch` and `seinfo` print
  (checked against them on a lab VM). A label check of a path asks for
  every component at once; `basalt fix selinux` and `basalt why` prefetch
  what all their denial groups need. Without the helper each query runs
  `sesearch` or `seinfo` on its own, as before;
- reuses every answer for as long as the same policy is loaded: the key
  is the kernel's policy load counter (`policyload` in
  `/sys/fs/selinux/status`), which changes on every policy load and
  boolean commit (`semanage`, `setsebool -P`, a module install). Where it
  cannot be read, answers live for a minute;
- looks up a unit's domain only when denials or a "Permission denied"
  need it, so a crash or a missing file costs no policy walk at all;
- for the root command line only, keeps the answers across runs in
  `/var/cache/basalt-assistant/policy/` (root, mode 0700), one file per
  boot and policy load. It reads such a file only when the directories and
  the file belong to root, nobody else may write them, the file is a plain
  file with one link (opened without following links) and its SELinux type
  is not one of the daemon's own. The daemon and the MCP server keep
  answers in memory only. Nothing is shared between them and the command
  line on purpose: the root command line never trusts answers the less
  privileged daemon wrote.

Every path a diagnosis uses comes from the audit record, a journal message
or an inode search (the document roots in the nginx and httpd
configurations, the usual web roots, then the usual service directories),
and must be a valid path with the denial's name and inode before it is
used; a tool's error message is never taken for one.

## Apply: preview, confirmation, snapshot, verification, audit

`basalt apply ID` prints the full report and the exact command lines, then:

- interactively, asks the person to type `yes`;
- non-interactively, needs `--yes --confirm CODE`, where CODE is the first
  8 hex digits of the SHA-256 of the proposal id and its command lines,
  printed with the proposal. A changed proposal has a different code, so an
  old confirmation cannot run new commands. A wrong code is refused and
  recorded.

Then it takes a snapper `pre` snapshot (userdata `basalt=apply`,
`proposal=ID`), runs the commands in order (stopping at the first failure;
no timeout, a cancelled command gets SIGTERM before SIGKILL; no child gets a
terminal), takes the `post` snapshot, runs the checks and writes an `apply`
audit record with every output. `basalt snapshots rollback --before ID`
undoes it. Data subvolumes (`/srv`, `/home`, `/var/log`, databases) are not
part of a rollback: for example a relabel of files under `/srv` stays,
while the file context rule (in the root) is rolled back.

The command runner follows the tui-tools pattern (preview the argv, run
that same argv, no shell) and is adapted from tui-kit's runner (MIT).

## Audit log

`/var/log/basalt-assistant/audit.jsonl`, one JSON record per line:
sequence number, time, type (`decision`, `finding`, `proposal`, `confirm`,
`decline`, `refuse`, `apply`, `ignore`, `suppress`, `start`, `stop`, `ask`,
`humanize`), actor
(program, uid, login uid, sudo user), text, data, the previous record's
hash and its own SHA-256. Editing, removing or reordering a record breaks
the chain (`basalt audit verify`). The file is root-owned, mode 0600, and
append-only (`chattr +a`, set by tmpfiles.d): rewriting it needs
`CAP_LINUX_IMMUTABLE` first. Every record is also sent to the journal
(`SYSLOG_IDENTIFIER=basalt-assistant`, fields `BASALT_AUDIT_SEQ`,
`BASALT_AUDIT_HASH`, `BASALT_AUDIT_PREV`, `BASALT_AUDIT_DATA`). The log
lives in `/var/log`, a separate subvolume, so it survives rollbacks.

### Sealed rotation

The chain must survive rotation, so the log is never cut and restarted.
`basalt audit rotate` (root; `basalt-audit-rotate.timer` runs it daily and
it acts once the file reaches `[audit] rotate_size`, 32 MiB by default;
`--force` rotates now), under the same lock every writer takes:

1. appends a `seal` record whose data holds the SHA-256 of every byte of the
   file before it, the first sequence number and the record count, and the
   name the file will be kept under;
2. keeps the file as `audit-<UTC time>.jsonl` (a hard link, then
   `chattr +i`: immutable, not only append-only);
3. writes the new `audit.jsonl` with a single `continue` record chained to
   the seal (its `prev` is the seal's hash, its data names the sealed file
   and the seal) and renames it into place atomically, then sets `+a` on
   it. A writer that was waiting for the lock on the old file notices that
   it was replaced and writes to the new one.

`basalt audit verify` walks every file in order: the chain inside each
file, a seal as the last record of every rotated file with a matching
SHA-256 and record count, and a `continue` record linked to that seal at
the start of the next file. A sealed file that is edited, truncated,
removed from the middle or swapped breaks it. Files sealed within the same
second are named `audit-<time>.jsonl`, `audit-<time>-2.jsonl` and so on,
and are read in that order. Removing the oldest files on
purpose (they are immutable: `chattr -i` first) leaves a chain that starts
with a `continue` record; verify reports that the earlier records cannot be
checked. A rotation interrupted after the seal leaves a file ending with
it: writers then refuse to append, so nothing is chained after a seal,
until `basalt audit rotate` (or the next timer run) finishes the job with
the same seal.

## Event engine

`basalt-assistantd` follows the journal (`journalctl -f -o json`) and polls
disk usage and the snapshot list:

| Event | Source | Diagnosis |
|---|---|---|
| a unit failed | systemd's "Failed with result" and "Failed to start" records (message ids `d9b373ed55a64feb8242e02dbe79a49c`, `be02cf6855d2428ba40df7e9d022f03d`) | `basalt why` |
| SELinux denial | AVC records of the journal's audit transport | `basalt fix selinux` (folded into the unit's proposal when the denial explains a unit failure) |
| disk filling | statfs every 5 minutes, warning and critical thresholds, the forecast; ENOSPC messages | `basalt disk` (without per-snapshot space, see confinement) |
| package transaction failed | `SOFTWARE_UPDATE` audit records with `res=failed` (rpm-plugin-audit); scriptlet failures in `/var/log/dnf5.log` (a failing `%post` leaves the package installed and rpm reports success); a dnf `pre` snapshot without its `post` | what changed since the transaction's `pre` snapshot; rollback proposed when packages were installed but not set up |

Rate limiting and dedup: events of the same kind settle for a few seconds
and are processed together (a unit failure first, so a denial that caused it
is part of the unit's proposal, not a second one); a repeat of a problem
with an open proposal only increases its counter; a problem whose proposal
was ignored stays quiet for the dedup window (default 1 h); at most 20 new
proposals per hour (excess is recorded as `suppress`). The journal position
is kept per boot, so after a rollback (the state directory is part of the
root) the daemon does not report old events again.

Each proposal is printed to the service's journal from a text template:
what is wrong, the evidence, the decisions, the proposed change with its
exact commands, and how to apply or ignore it.

## Notifications

- Journal, always: every finding is an audit record in the journal, and
  the daemon prints the proposal; findings the decision layer marks for
  notification (`event.notify`) are printed at warning priority, so
  `journalctl -p warning` shows them.
- Desktop: `basalt-notify` shows those findings as freedesktop
  notifications (`org.freedesktop.Notifications`, through `busctl --user`
  run as each user) in every local graphical session (wayland or x11, not
  remote). With `[notify] desktop = auto` (default) it is on only when the
  default target is `graphical.target`; on a server it has nothing to do
  and exits at once.
- Webhook, optional and off by default: set `webhook_url` (https; plain
  http only to localhost) and put a random key in `webhook_secret_file`
  (default `/etc/basalt/webhook.key`, root-owned, mode 0600):

  ```sh
  install -m 0600 /dev/null /etc/basalt/webhook.key
  head -c 32 /dev/urandom | base64 >/etc/basalt/webhook.key
  systemctl restart basalt-notify
  ```

  Each finding is a JSON POST (`{"event": "finding", "finding": {seq, time,
  host, proposal, title, kind, subject, severity, needs_review, notify},
  "hint": "basalt show ID"}`) with headers `X-Basalt-Event`,
  `X-Basalt-Timestamp` (Unix seconds) and `X-Basalt-Signature:
  sha256=HEX`, the HMAC-SHA256 of `timestamp + "." + body` with the key.
  Receivers should check the signature and reject old timestamps.
  `webhook_events = all` sends every finding, not only the marked ones.
  Failures are retried twice (network errors, 5xx, 429) and logged.
- No e-mail.

`basalt-notify` reads the findings from the journal, matched on the
trusted field `_SYSTEMD_UNIT=basalt-assistantd.service` (a local user
cannot forge it with `logger`), and keeps its journal cursor in
`/var/lib/basalt-notify` (its first start delivers new findings only, not
the backlog). It is a separate unit because the daemon has no
network access and no way into user sessions, and should not get either.
It runs as root without an SELinux domain of its own yet (it drops to each
user to reach that user's session bus), with the systemd hardening options
as its fence.

## Decision layer

Bounded, typed questions with a probability for every option: the
application, not the backend, decides what to do.

| Question | Kind | Options |
|---|---|---|
| `unit.cause` | choice | config_error, selinux_denial, port_conflict, dependency_failed, disk_full, missing_file, crashed (also an out-of-memory kill), unknown (also a file-permission error) |
| `avc.class` | choice | mislabeled, missing_fcontext, port, boolean, unknown, suspicious |
| `event.severity` | score | 1 to 5 |
| `event.notify` | boolean | true, false |
| `dnf.next` | choice | rollback, investigate |
| `disk.cause` | choice | snapshots, journal, package_cache, other_data |
| `route.bigger_model` | boolean | true, false (always false until a model backend exists; logged so the routing seam is visible) |

The default backend is `vsm` (see The VSM backend below); `rules/v1`
answers the questions VSM does not (severity, notification, routing) and
every question whenever VSM cannot.

The rules backend, `rules/v1`, is deterministic: each question has a prior
over its options and rules that multiply options when the diagnosers found
a feature (for example "the config checker failed" multiplies config_error
by 30); the product is normalized. The factors are set by hand from lab
cases, so the probabilities are calibrated only roughly. At or above a
question's threshold (default 0.75, per question in
`/etc/basalt/assistant.conf`) the automatic path is taken (diagnose and
propose; never apply); below it the proposal is marked for review and, for
SELinux, no change is proposed. Every decision is written to the audit log
with its question, features, options, probabilities, threshold, backend,
the rules that fired and the action taken.

A second backend, `openai-compatible`, asks a language model behind an
OpenAI-compatible endpoint (normally the local `basalt-llm` service): the
options are numbered, the output is constrained to one number, and the
probability of each option comes from the token log-probabilities, with
an optional per-question temperature (`[calibration]`). It answers only
the questions the shared evaluation suite measures (`unit.cause`,
`avc.class`, `dnf.next`, `disk.cause`); severity, notification and
routing stay with the rules, and the rules answer, marked `(fallback)`,
whenever the model fails. It is not the default: on the evaluation suite
the rules are more accurate than the small models
([milestone-2b-report.md](milestone-2b-report.md)).

### The VSM backend (default)

The default backend, `vsm`, answers the same four questions with VSM, a
small diagnosis engine that runs in process (Go, no model server):

- a knowledge index (package `basalt-knowledge`, one per Fedora release,
  under `/usr/share/basalt/knowledge/<release>`) holds known problems as
  cases: the evidence pattern each one needs (the diagnosers' findings as
  two-letter codes), the cause, a diagnosis, typed actions from the closed
  set and the checks that confirm the fix, the versions where it applies;
  for the findings of a question it returns the three best ranked cases
  (required codes matched, minus contradictions, then the most specific);
- a planner (package `basalt-vsm-planner`, about 65,000 parameters, 256
  KiB of weights under `/usr/share/basalt/vsm-planner`) reads the goal,
  the codes and the candidates' match statistics, never names, paths or
  ports, and accepts a candidate, picks another or abstains; its pick
  probabilities become the answer's probabilities (an abstention goes to
  the cautious option: unknown, investigate, other data);
- a deterministic guard outside the model refuses any pick the evidence
  does not fully support (a required finding missing, or one the case
  excludes), and answers the cautious option when the SELinux policy could
  not be queried.

New problems arrive as knowledge cases (a data update with the system
updates), not as a new model. Every decision it makes goes to the audit
log with the picked case, the candidates, the evidence codes, the view
and the knowledge and planner versions. If the packages are missing,
damaged, unsigned or for another DSL, or a question takes more than 2 s,
the rules answer, marked `(fallback)`, with the reason in the decision
record. Severity, notification and routing stay with the rules.

The confined daemon skips probes the root command line runs (config
checkers, the process owning a port, the inode search for a denial's
path, the space each snapshot holds, the package cache size). Its
questions say so (`view: confined` in the decision record), and when VSM
abstains there, part of the probability goes to the other options
instead of all of it to the cautious one: the answer's confidence then
reflects that the cause may be one the daemon could not see. How much is
fitted per question for each planner and shipped with it
(`calibration.json` in `basalt-vsm-planner`).

#### Signed knowledge

The knowledge index is used only when it is signed. Its `manifest.json`
holds the SHA-256 of the cases and of the index table, and
`manifest.json.sig` is a detached OpenPGP signature of the manifest by
the knowledge signing subkey of the OpenBasalt release key:

- release key (primary): `3601 7348 42BD 4E48 2D19 DE4A E4EE D5EC A395 B302`
- knowledge subkey: `85D6 1430 B704 3868 0F6E B955 E79E 4020 605A 659A`

The assistant checks the signature itself with the Go standard library
(no gpg, no helper program): the certificate
(`/usr/share/basalt/knowledge/openbasalt-release-key.asc`, the same file
as basalt-release's `RPM-GPG-KEY-basalt` and as
https://obpkg.org/keys/openbasalt-release-key.asc) must contain both
pinned fingerprints, the subkey must be bound to the primary key by a
valid binding signature that allows signing and carries the subkey's own
back signature, neither may be revoked, the data signature must be a
version 4 RSA signature over SHA-256, SHA-384 or SHA-512 made while the
subkey was valid, and the manifest must match every file. Anything else
and the rules answer.

Anyone can check the same link with standard tools:

```sh
gpg --show-keys --with-subkey-fingerprints /usr/share/basalt/knowledge/openbasalt-release-key.asc
# compare with the fingerprints above and with the key published at https://obpkg.org/keys/
# gpgv needs the key as a binary keyring file (a pipe does not work)
gpg --dearmor </usr/share/basalt/knowledge/openbasalt-release-key.asc >openbasalt-release-key.gpg
gpgv --keyring ./openbasalt-release-key.gpg \
     /usr/share/basalt/knowledge/44/manifest.json.sig /usr/share/basalt/knowledge/44/manifest.json
# must print: Good signature from "OpenBasalt release key <openbasalt@openbasalt.org>"
sha256sum /usr/share/basalt/knowledge/44/cases.jsonl /usr/share/basalt/knowledge/44/index.bin
# the two sums are "sha256" and "index_sha256" in manifest.json
```

From a source checkout, `go run ./tools/basalt-eval knowledge-verify -dir
DIR` (in `packages/basalt-assistant`) runs the assistant's own check on an
index directory.

A lab or a private knowledge build can trust another key in
`/etc/basalt/assistant.conf` (`[vsm] knowledge_key`, `knowledge_signer`);
`scripts/sign-knowledge.sh` signs an index with a subkey kept in a 0600
file and checks the result with gpg and with the assistant's verifier.

#### Results

On the evaluation suite (253 questions of 239 cases, knowledge of 39
cases, planner 3.20261004):

| Backend | Accuracy | ECE | Brier | lab | lab-daemon | generated |
|---|---|---|---|---|---|---|
| rules/v1 | 96.8 % | 0.212 | 0.138 | 32/34 | 29/30 | 184/189 |
| vsm | 98.0 % | 0.020 | 0.042 | 32/34 | 29/30 | 187/189 |

On faults injected on a lab machine (basalt-assistant 0.7.0, the same
fault diagnosed by `basalt` as root and by the confined daemon; 35 faults
from earlier runs and 15 new ones written before this release was
measured; root misses include two package transactions, for which the
command line has no root command):

| Backend | root: answers right | daemon: answers right | ECE root / daemon | unsafe proposals |
|---|---|---|---|---|
| rules/v1, earlier 35 | 33 | 31 | 0.27 / 0.19 | 0 |
| vsm, earlier 35 | 33 | 31 | 0.01 / 0.13 | 0 |
| rules/v1, new 15 | 14 | 12 | 0.23 / 0.17 | 0 |
| vsm, new 15 | 14 | 12 | 0.06 / 0.11 | 0 |

The changes proposed were right exactly where the answers were. VSM
answers as well as the rules in every view and on every set measured,
and its probabilities match how often it is right far better. Its cost:
about 4 ms per question, 0.4 MB of memory for the knowledge and the
planner. On these numbers it became the default backend in
basalt-assistant 0.8.0, with the knowledge signed by the OpenBasalt
knowledge subkey.

Whatever the backend, a denial's fix is proposed only when the decision
is confident in the class the fix was built for (never for a confident
"unknown"), and a case VSM knows without a verified fix (a port another
service's type owns) never takes the automatic path.

`basalt-assistant` recommends `basalt-knowledge` and
`basalt-vsm-planner` (weak dependencies), so a default install has them.
Without them, or for a Fedora release with no index yet, the rules answer
and each decision record says why (`fallback_reason`); the daemon's
start record names the knowledge it loaded (`vsm`) or the reason it could
not (`vsm_error`). To use the rules only:

```ini
# /etc/basalt/assistant.conf
[decision]
backend = rules
```

## MCP tools

`basalt-mcp` speaks MCP (protocol 2025-06-18) over stdio.

| Tool | Kind |
|---|---|
| `basalt_status`, `basalt_why_unit`, `basalt_selinux_denials`, `basalt_disk`, `basalt_snapshots`, `basalt_snapshot_diff`, `basalt_pending`, `basalt_proposal`, `basalt_audit_tail` | read (annotated read-only) |
| `basalt_drivers` | read (annotated read-only): the Additional drivers report |
| `basalt_propose_unit_fix`, `basalt_propose_selinux_fix`, `basalt_propose_rollback`, `basalt_propose_disk_cleanup`, `basalt_propose_driver_install`, `basalt_propose_action` | store a proposal and return its id and commands |

No tool executes a change. The confirmation code is never returned to the
client, by any tool (`basalt_proposal` included): the person reads it from
`basalt show ID` or `basalt apply ID`.
`basalt_propose_action` lets a client propose any action of the closed set;
it is always marked for review. The MCP server runs in the confined
domain: a file restore or a rollback it is asked for is stored as a hint
for `basalt confirm`. Example client configuration:

```json
{ "mcpServers": { "basalt": { "command": "sudo", "args": ["basalt-mcp"] } } }
```

## Confinement

SELinux module `basalt_assistant` (package `basalt-assistant-selinux`):

- `basalt_assistant_t` runs the daemon (from systemd, also with
  `NoNewPrivileges`) and the MCP server (an unconfined administrator's
  process transitions into it when it runs `basalt-mcp`).
- It may read what diagnosis needs: the journal (running `journalctl` in
  its own domain), unit state through systemd, SELinux policy and file
  contexts (`sesearch`, `seinfo`, `matchpathcon`, `getsebool`), snapshot
  metadata under `/.snapshots`, file attributes, statfs, socket lists.
- It may write only `/var/lib/basalt-assistant` (`basalt_assistant_var_lib_t`),
  append to (not write) `/var/log/basalt-assistant` (`basalt_assistant_log_t`),
  and send to journald. It has no transitions out of its domain.
- Probes that need more are skipped in the daemon and the MCP server and
  named in the report: config checkers (they open log files), btrfs ioctls
  (space per snapshot), rpm database comparison, process owners of
  sockets. `basalt` as root runs them.
- What the daemon and the MCP server cannot check they do not propose: a
  file restore or a rollback from them is a hint (see Hints).
- The VSM backend's knowledge and planner weights
  (`/usr/share/basalt/knowledge`, `/usr/share/basalt/vsm-planner`) have
  their own type, `basalt_knowledge_t`, which the domain may only read.
- Denials of the assistant's own domain are reported as a bug, never with a
  fix that would widen its access.

The systemd unit adds `ProtectSystem=strict`, `ProtectHome=read-only`,
`CapabilityBoundingSet=CAP_DAC_READ_SEARCH` and the other hardening options
as a second, independent fence.

## Feedback

`basalt feedback` sends a report to the Basalt OS project. It is opt-in in
every part: nothing leaves the machine until the person has seen the exact
payload and said yes.

```
basalt feedback                                   asks for everything, step by step
basalt feedback "the installer froze" --kind bug  the message and kind on the command line
basalt feedback "dark theme" --kind idea --include os,packages --preview
basalt feedback "dark theme" --kind idea --include os,packages --yes --confirm CODE
```

1. The kind (bug, idea, other), the message and, optionally, an e-mail
   address for a reply.
2. Each optional part is collected, scrubbed and shown, and included only
   if the person answers yes: the OS version and kernel release
   (`/etc/os-release`), the versions of the installed `basalt-*`
   packages, a hardware summary (processor model, cores, memory,
   firmware type, virtual machine or not, TPM present; no serial numbers,
   MAC addresses or disk identifiers) and the assistant's findings of the
   last 7 days (from the system journal, readable by root and by members
   of wheel, adm or systemd-journal; others are told to use sudo).
3. Personal data is scrubbed from the message and every part: this
   machine's host names, the user names and full names of its accounts,
   IPv4 and IPv6 and MAC addresses, the name after `/home/` and `/root/`,
   e-mail addresses in the text, and anything shaped like a password,
   token, key or JWT (the same redaction as for remote models, see
   Humanize). The reply address the person typed is kept, and the report
   says so.
4. The exact JSON payload is shown with the endpoint and the number of
   values that were redacted. `edit` opens it in `$VISUAL` or `$EDITOR`
   (what the person writes there is sent as they leave it); `yes` sends;
   anything else cancels.

Without a terminal it never asks: `--preview` prints the payload and a
confirmation code (the first 8 hex digits of the payload's SHA-256;
`--json` gives both as JSON), and only `--yes --confirm CODE` with that
code sends it, so a confirmation always matches the payload it was given
for. This is the hook for the Basalt shell's future voice action "send
feedback": the shell shows the payload on its confirmation sheet and
sends with the code after the person confirms, with `--source voice`.

The report goes to the project's feedback service (source:
[basalt-os/feedback-worker](https://github.com/basalt-os/feedback-worker)),
which stores it privately with the time it arrived and keeps no IP
address. People can also use the form on
[basalt-os.org](https://basalt-os.org/#feedback) or write to
feedback@basalt-os.org. A report is evidence for the project's lab, not
knowledge by itself: a fix learned from it is reproduced and verified
before it reaches `basalt-knowledge`.

Where it runs, and the network:

- `basalt feedback` sends from the person's own session, as the person,
  over HTTPS. The confined daemon and the MCP server have no network
  access and offer no feedback tool: sending data off the machine always
  starts with a person.
- AI agent sessions (`basalt-agent`) are default deny: the feedback
  service is on no shipped allowlist, so an agent that runs `basalt
  feedback` inside its session cannot reach it, and the denied lookup is
  recorded in the ledger (`dns.deny`). Allowing it is an ordinary
  allowlist change with its preview, confirmation and record
  ([network.md](network.md)), for example `basalt-agent egress propose
  PROFILE add basalt-feedback.openbasalt.workers.dev`; we do not
  recommend it.
- `[feedback] endpoint` in `/etc/basalt/assistant.conf` points it
  elsewhere (https only; plain http to localhost for tests), and an empty
  value turns sending off.

## Translations

User-facing text goes through GNU gettext catalogs, domain
`basalt-assistant` (package `internal/i18n`), in the language of
`LANGUAGE`, `LC_ALL`, `LC_MESSAGES` or `LANG`; English is the reference
text and Brazilian Portuguese the first translation. Sources:
`po/basalt-assistant.pot` (generated: `go test ./internal/i18n -update`)
and `po/<lang>.po`; the package build compiles them with `msgfmt --check`
into `/usr/share/locale/<lang>/LC_MESSAGES/basalt-assistant.mo`. The tests
fail when the template is not current, a translation is missing or fuzzy,
or a translation's placeholders differ from the English text. Whole
sentences only, with placeholders, and plural forms through `i18n.N`.
Machine output (`--json`), logs and audit records stay English.

## Configuration

`/etc/basalt/assistant.conf` (INI): decision backend, thresholds and
calibration, where the VSM backend's data is (`[vsm]`), the translator (`[translator]`: enabled, endpoint, prompt), the
humanize layer (`[humanize]`: enabled, endpoint, model, allow_remote,
api_key_file, prompt, max_chars, timeout, stream), disk
thresholds (warn 85 %, critical 95 %, what counts as large), event timings
(dedup window, hourly limit, disk interval, settle times), the audit log
rotation size (`[audit]`), notifications (`[notify]`) and where `basalt
feedback` sends (`[feedback]`: endpoint, timeout). Restart the
daemon (and `basalt-notify`) after a change.

## Lab

`make lab-assistant-test` (`scripts/lab/assistant-test.sh`) runs the
scenarios on a lab VM: broken nginx config (the daemon's hint, confirmed
as root), a config checker that must leave no file behind, a log
directory moved from `/root` (hidden denial), nginx on an unlabeled port,
a package whose `%post` fails, a disk filled by a file only a snapshot
holds, the confinement probe and the MCP server. `make assistant-test` runs the unit tests (fixtures of
real journal, AVC, sesearch, seinfo, btrfs and snapper output) in a Fedora
container. Results: [milestone-2a-report.md](milestone-2a-report.md).
