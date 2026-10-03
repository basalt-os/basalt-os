# The Basalt OS system assistant

Status: pre-alpha, milestone 2b. The assistant diagnoses the system and
proposes fixes without any language model. An optional local model
(package `basalt-llm`, [local-model.md](local-model.md)) can translate
requests in natural language into the commands below (`basalt ask`) and
can answer the decision layer's questions; both are off by default and
nothing below depends on them.

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

## Commands

Read-only, no confirmation:

| Command | What it reports |
|---|---|
| `basalt status` | SELinux mode, failed units, denials in 24 h, root file system usage, snapshots, unfinished package transactions, pending rollback, daemon state, pending proposals, audit chain |
| `basalt why UNIT` | unit state and result; relevant journal lines; SELinux denials for the unit's domain (from its executable's label) and, when a service reports "Permission denied" without any logged denial, a label check of every component of the path (dontaudit rules hide many denials); failed dependencies; ports named in the failure and who holds them; full file systems; the service's own config checker (`nginx -t`, `httpd -t`, `sshd -t`, `named-checkconf`, `haproxy -c`, `postfix check`, `testparm`, and others) with the file and line of the error; a snapshot that holds a different copy of the broken file |
| `basalt fix selinux [--since 1h]` | recent denials grouped by domain, target, class, permission and object, each mapped to a known fix or to review |
| `basalt snapshots` | the root snapshots; `[proposal]` marks the ones `basalt apply` took |
| `basalt snapshots diff A [B]` | packages added, removed and changed (rpm databases of the two snapshots) and changed files by directory (`snapper status`) |
| `basalt disk` | btrfs usage, the space each snapshot holds alone (`btrfs filesystem du`), journal and package cache size, a fullness forecast from stored samples |
| `basalt pending [--all]`, `basalt show ID` | proposals |
| `basalt audit [N]`, `basalt audit verify` | the audit log and its hash chain, checked across rotated files |
| `basalt ask "REQUEST"` | (optional, needs the local model) the request translated into one of the commands above, which then runs; a change (apply, rollback) is only printed, see [local-model.md](local-model.md) |

Changes (root):

| Command | Change |
|---|---|
| `basalt apply ID` | runs a proposal after confirmation |
| `basalt ignore ID [--reason TEXT]` | closes a proposal without running it (recorded) |
| `basalt snapshots rollback N` or `--before ID` | proposes and runs `basalt-rollback N`; `--before` uses the snapshot `basalt apply` took before proposal ID |
| `basalt why UNIT --apply`, `basalt fix selinux --apply`, `basalt disk --apply` | store the proposal and go straight to the confirmation |

`--json` gives machine-readable output for every read command.

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

Each action also carries its verification: the label is the policy
default (`matchpathcon -V`), the port or boolean has the new value, the
unit is active and no new denial appeared since the change, the restored
file matches the snapshot, a rollback is pending, the snapshot is gone.

There is no free-form command action and no `audit2allow`: a denial
without a known fix is reported for review, never turned into policy.

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
`decline`, `refuse`, `apply`, `ignore`, `suppress`, `start`, `stop`, `ask`), actor
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
removed from the middle or swapped breaks it. Removing the oldest files on
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
| `unit.cause` | choice | config_error, selinux_denial, port_conflict, dependency_failed, disk_full, missing_file, crashed, unknown |
| `avc.class` | choice | mislabeled, missing_fcontext, port, boolean, unknown, suspicious |
| `event.severity` | score | 1 to 5 |
| `event.notify` | boolean | true, false |
| `dnf.next` | choice | rollback, investigate |
| `disk.cause` | choice | snapshots, journal, package_cache, other_data |
| `route.bigger_model` | boolean | true, false (always false until a model backend exists; logged so the routing seam is visible) |

The first backend, `rules/v1`, is deterministic: each question has a prior
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
whenever the model fails. The rules stay the default: on the evaluation
suite they are more accurate than the small models
([milestone-2b-report.md](milestone-2b-report.md)).

## MCP tools

`basalt-mcp` speaks MCP (protocol 2025-06-18) over stdio.

| Tool | Kind |
|---|---|
| `basalt_status`, `basalt_why_unit`, `basalt_selinux_denials`, `basalt_disk`, `basalt_snapshots`, `basalt_snapshot_diff`, `basalt_pending`, `basalt_proposal`, `basalt_audit_tail` | read (annotated read-only) |
| `basalt_propose_unit_fix`, `basalt_propose_selinux_fix`, `basalt_propose_rollback`, `basalt_propose_disk_cleanup`, `basalt_propose_action` | store a proposal and return its id and commands |

No tool executes a change. The confirmation code is not returned to the
client: the person reads it from `basalt show ID` or `basalt apply ID`.
`basalt_propose_action` lets a client propose any action of the closed set;
it is always marked for review. Example client configuration:

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
- Denials of the assistant's own domain are reported as a bug, never with a
  fix that would widen its access.

The systemd unit adds `ProtectSystem=strict`, `ProtectHome=read-only`,
`CapabilityBoundingSet=CAP_DAC_READ_SEARCH` and the other hardening options
as a second, independent fence.

## Configuration

`/etc/basalt/assistant.conf` (INI): decision backend, thresholds and
calibration, the translator (`[translator]`: enabled, endpoint, prompt), disk
thresholds (warn 85 %, critical 95 %, what counts as large), event timings
(dedup window, hourly limit, disk interval, settle times), the audit log
rotation size (`[audit]`) and notifications (`[notify]`). Restart the
daemon (and `basalt-notify`) after a change.

## Lab

`make lab-assistant-test` (`scripts/lab/assistant-test.sh`) runs the
scenarios on a lab VM: broken nginx config, a log directory moved from
`/root` (hidden denial), nginx on an unlabeled port, a package whose `%post`
fails, a disk filled by a file only a snapshot holds, the confinement probe
and the MCP server. `make assistant-test` runs the unit tests (fixtures of
real journal, AVC, sesearch, seinfo, btrfs and snapper output) in a Fedora
container. Results: [milestone-2a-report.md](milestone-2a-report.md).
