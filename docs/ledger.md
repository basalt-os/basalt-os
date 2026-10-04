# The audit ledger (basalt-ledger)

`basalt-ledger` keeps the security-relevant events of a Basalt OS system
in one append-only, hash-chained trail, and tells you in plain English
what happened: what an AI agent did today, what it was refused, who
became administrator, when the system was rolled back. It is installed and
enabled by default on Basalt OS servers.

```
basalt-ledger                               your records, newest last
basalt-ledger summary --since today         what happened, grouped by agent session
basalt-ledger --agent claude --severity warning
basalt-ledger --project ~/src/app --since 2h
basalt-ledger --all                         everything (administrator authentication)
basalt-ledger verify                        check the chain across all files
basalt-ledger export --since today -o incident.json
basalt-ledger verify-export incident.json --key /var/log/basalt-ledger/keys/export.pub
basalt ledger summary --since today         the same through the basalt command
```

## What it records

| Producer | Events | How it arrives |
|---|---|---|
| basalt-agent | `session.start`, `session.end`, `egress.allow`, `egress.deny`, `grant.request`, `grant.apply`, `relabel`, `install`, `egress.change` | the launcher sends each record of its session log (schema v1) over the socket |
| basalt-resolver | `egress.session.*`, `dns.allow`, `dns.deny`, `dns.rebinding`, `dns.direct`, `egress.drop`, `egress.grant` | socket, from root in `basalt_resolver_t` only |
| basalt-assistant | `assistant.<type>` (decision, proposal, confirm, apply, refuse, seal and the rest) | journal: the assistant writes every audit record there with its chain fields |
| selinux | `selinux.avc`, `selinux.error` | journal (audit transport); a denial at the SELinux level of a running agent session is attributed to that session |
| polkit, pkexec, sudo | `polkit.auth`, `escalation.pkexec`, `escalation.sudo` (allowed and refused) | journal |
| login | `login.session`, `auth.failure` | journal (systemd-logind, audit) |
| basalt-agent-grant | `agent.grant.helper` | journal (the root helper behind `basalt-agent grant`) |
| snapper | `snapshot.create`, `snapshot.rollback` | snapshot descriptions in `/.snapshots` |
| basalt-ledger | `ledger.start` (with the export key in use), `ledger.seal`, `ledger.continue`, `ledger.retention`, `ledger.refused`, `ledger.chain_error` | itself |

## Records

The shared schema version 1 (the envelope basalt-agent writes), plus what
the ledger adds when it accepts a record:

```jsonc
{
  "v": 1,
  "seq": 1342,                       // ledger sequence (one chain for the system)
  "time": "2026-10-04T14:12:16Z",    // the producer's time
  "received": "2026-10-04T14:12:16.31Z",
  "producer": "basalt-resolver",
  "uid": 1000,                       // whose event (users read their own)
  "session": "s-f3ed68987605",
  "event": "dns.rebinding",
  "outcome": "denied",               // ok, allowed, denied, error
  "severity": "warning",             // computed by the ledger: info, notice, warning, critical
  "subject": { "profile": "labnet", "mode": "native", "level": "s0:c109,c974",
               "project": "/home/dev/src/app", "app": "" },
  "data": { "name": "rebind.example.test", "addresses": ["10.0.0.5"], "reason": "an allowed name pointed at a local or private address" },
  "peer": { "uid": 0, "pid": 812, "context": "system_u:system_r:basalt_resolver_t:s0" },
  "src": { "seq": 41, "hash": "9c1d07aa", "prev": "51b2e0c4" },   // the producer's own chain position, if any (hashes shortened)
  "prev": "4f53e2ee",
  "hash": "e334e9be"                 // SHA-256 of the record with hash empty (64 hex digits)
}
```

The ledger owns `seq`, `received`, `severity`, `peer`, `prev` and `hash`. A
producer that sends `seq`, `prev` or `hash` (basalt-agent sends its session
log's) gets them stored as `src`: they never touch the ledger's chain, and
the two copies can be compared.

## Storage

- `/var/log/basalt-ledger/ledger.jsonl`, JSON lines, each record chained
  to the previous by SHA-256. `/var/log` is its own subvolume, outside the
  root snapshots, so a rollback never rewinds the trail.
- Sealed rotation, the same design as the assistant's audit log: at 8 MiB
  or after 24 hours (or `basalt-ledger rotate` as root) the ledger appends
  a seal record holding the SHA-256 of the file so far, keeps the file as
  `ledger-<UTC time>.jsonl`, and starts the new file with a continue record
  chained to the seal. One chain runs across all files.
- The current file is append-only (`chattr +a`), sealed files immutable
  (`+i`). Records are written with `fsync`.
- `basalt-ledger verify` checks sequence, links and hashes in every file,
  each seal against its file's content, and each continue record against
  the previous seal. Oldest files that are missing are an error unless a
  retention record in the chain accounts for them (below). `verify --path
  FILE` checks a copy offline, without the service.

This is tamper evidence, not tamper proofing: root can clear the file
attributes and rewrite a file, and the chain then shows it. Anchoring the
chain head off the machine (an external immutable vault) is future work.

## Retention

Sealed files are kept for `retention` (default `365d`, one year from the
time a file was sealed); `retention = forever` keeps everything. Once an
hour (and at start) the service removes the sealed files older than that,
oldest first. Before it removes a file it appends a `ledger.retention`
record to the chain:

```jsonc
{ "event": "ledger.retention", "producer": "basalt-ledger", "severity": "notice",
  "data": { "file": "ledger-20251004T101500Z.jsonl", "first_seq": 1, "last_seq": 1342,
            "seal_hash": "<the seal record's hash>", "records": 1342,
            "sha256": "<SHA-256 of the whole file>", "sealed": "2025-10-04T10:15:00Z",
            "retention": "365d" } }
```

The first remaining file starts with a continue record that names the
removed file and its seal's sequence and hash. `verify` accepts that start
only when a retention record in the chain names the same file, sequence
and hash, and then reports "records up to N were removed by the retention
policy". A file removed any other way (by hand, by an attacker) has no
such record and is still reported as a broken chain. The retention record
itself is chained like any other, so it cannot be added later without
breaking the chain after it. If the removal is interrupted after the
record, the next pass removes the file without a second record.

Copy sealed files off the machine before they expire if they must be kept
longer; `verify --path` checks the copies.

## Who may write

Who may connect to the socket at all is SELinux's decision: user domains
and the resolver may, confined agent domains (`basalt_agent_t`,
`container_t`) may not. What a connected peer may write is decided from
the kernel's view of it (`SO_PEERCRED`, `SO_PEERSEC`):

- an unprivileged user may write as `basalt-agent` or `basalt-shell` only
  (`user_producers`), and only records of their own uid;
- `basalt-resolver` records are accepted only from root in
  `basalt_resolver_t`;
- the producer names of the built-in collectors (`selinux`, `polkit`,
  `sudo`, `pkexec`, `login`, `snapper`, `basalt-assistant`,
  `basalt-agent-grant`, `basalt-ledger`) are never accepted from the socket;
- users are rate limited (200 records per second, bursts of 2000).

There is no operation that changes or removes a record. Any other request
(`delete`, `update`, `truncate`) is refused, and the refusal is itself
recorded as `ledger.refused`, visible to the user who tried.

## Who may read

Users read the records of their own uid, including the SELinux denials,
network decisions and escalations attributed to them. Root reads
everything; `basalt-ledger --all` runs the query through pkexec (polkit
action `org.basalt-os.ledger.read-all`, administrator authentication).
The files themselves are root only.

## Plain English

Every record has a sentence made from a template (no model involved):

```
2026-10-04 14:11:55  warning  #126  Agent labnet (s-8f307eb1b510) asked for example.com, which is not on its list; the lookup was refused
2026-10-04 14:12:16  warning  #137  rebind.example.test pointed at a local address (10.0.0.5); refused, to protect local services
2026-10-04 14:32:20  notice   #313  The system was rolled back: snapshot 26 becomes the root at the next boot
```

`basalt-ledger summary` groups agent sessions:

```
Agent claude worked on ~/src/app (native mode, session s-9fd67672c230), exit code 0; reached api.anthropic.com, registry.npmjs.org; 2 attempts refused (example.com, 1.1.1.1).
3 administrator authentications.
Records: 41 info, 3 notice, 2 warning.
```

The local model can later rephrase these; the templates stay the source.

## Signed exports

`basalt-ledger export [FILTERS] -o FILE` writes a JSON document with the
selected records (your own, or everything as root), where they sit in the
chain (first and last sequence, the chain head, whether the chain verified
when exported) and a signature over the document. `verify-export` checks
the signature, every record's hash and the links between consecutive
records; with `--key` it also checks the signer.

### The host key

On a machine with a TPM 2.0 the export key is held by the TPM: an ECDSA
P-256 key (`key_kind` `tpm`, `key_alg` `ecdsa-p256-sha256` in the export).
It is a primary key under the TPM's owner hierarchy, derived from the
TPM's owner seed and a fixed template with a Basalt label, so the same TPM
always gives the same key and nothing is stored on disk. The private half
never leaves the TPM (fixed to the TPM, generated inside it), so an export
signed with it was signed on that machine, by its ledger service or by
someone with root on it at the time. Clearing the TPM gives a new key; the
service then refuses to sign until it is restarted, and uses the new key.
It is not bound to PCRs: firmware and kernel updates do not change it.

Without a usable TPM the service falls back to a software key: an
Ed25519 key generated on first start and kept with the ledger
(`/var/log/basalt-ledger/keys/export-ed25519.key`, root only), labeled
`software` in exports and in `basalt-ledger status`. Anyone who is root on
the machine, or who gets a copy of the disk, can sign with it. Exports
made by basalt-ledger 0.1.0 say `development`; they are software-key
exports and still verify.

`/etc/basalt-ledger/ledger.conf` chooses: `export_key = auto` (default:
the TPM key when the TPM works, else the software key), `tpm` (the TPM key
only; without it exports are refused) or `software`. `basalt-ledger
status` shows the key id, its kind and, for the software key, why it is
used; the `ledger.start` record carries the same.

### Checking an export

The service writes the current public key, in the standard PEM form, to
`/var/log/basalt-ledger/keys/export.pub` (readable by everyone), and every
key it has used to `keys/export-<key id>.pub`, so an export made before a
key change can still be checked.

```
basalt-ledger verify-export incident.json --key export.pub
```

checks the signature against the given key (without `--key` it only
checks that the export is intact under the key it carries, which proves
nothing about where it came from), every record's hash and the chain
links, and says which kind of key signed it. The command is a static
binary and works on any Linux machine, with a copy of `export.pub` taken
from the host when it was known to be good (at install time, or recorded
in an inventory). The signature is ECDSA (ASN.1 DER) or Ed25519 over the
SHA-256 of the format name, a newline and the export document without its
`signature` field, as `encoding/json` writes it.

Proving to a third party that the key is TPM-resident (a TPM2_Certify
of the key by the TPM's attestation key, chained to the manufacturer's
endorsement certificate) and anchoring the chain head off the machine are
future work.

## JSON API

The socket `/run/basalt-ledger/ledger.sock` speaks one JSON request per
line and answers one JSON reply per line; a connection can carry many
requests. The desktop shell's timeline uses it directly (or `basalt-ledger
api`, which passes request lines from stdin). Every reply has `ok` and, on
failure, `error`.

| Request | Reply |
|---|---|
| `{"op":"query","filter":F}` | `records`: matching records, oldest first, each with `text` (the sentence) |
| `{"op":"summary","filter":F}` | `summary`: `lines`, `counts` by severity, `sessions` (per agent session: agent, project, mode, start, end, exit code, reached, refused, SELinux denials, grants) |
| `{"op":"verify"}` | `verify`: files, first and last sequence, head hash, records, seals |
| `{"op":"export","filter":F}` | `export`: the signed document |
| `{"op":"status"}` | `status`: chain head, file, size, key id and kind, whether you see everything |
| `{"op":"append","record":R}` | `seq`, `hash` of the stored record |
| `{"op":"rotate"}` | `rotate` (root only) |

The filter `F` (all fields optional): `since`, `until` (RFC 3339, applied
to the time the ledger received the record, so producers cannot backdate
into a window), `producer`, `event` (exact, or a prefix ending in `.`),
`session`, `agent` (subject profile), `project` (a directory: records for
it and below it), `app` (subject app or producer), `severity` (minimum),
`outcome`, `uid` (root only; users always get their own), `after_seq`
(for polling: only records after this sequence), `limit` (newest N,
default 1000).

A timeline polls with `after_seq` set to the last sequence it showed:

```
{"op":"query","filter":{"after_seq":1342,"limit":200}}
{"ok":true,"records":[{"v":1,"seq":1343,"event":"egress.allow","text":"Agent claude reached api.anthropic.com:443"}]}
```

## SELinux

`basalt_ledger_t` (root service, `NoNewPrivileges`, only
`CAP_LINUX_IMMUTABLE`, no network) may: manage its files in
`/var/log/basalt-ledger` (`basalt_ledger_log_t`) and its socket
(`basalt_ledger_var_run_t`), run `journalctl` (as `journalctl_t`), read
snapper's snapshot descriptions, use the TPM resource manager
(`/dev/tpmrm0`) for its export key, and look up user names. Producers use the
interface `basalt_ledger_stream_connect`; nothing else writes its files.

## Configuration

`/etc/basalt-ledger/ledger.conf`: `dir`, `socket`, `rotate_size` (MiB),
`rotate_age`, `journal` (yes or no), `snapshots` (the snapshot directory,
empty to disable), `user_producers`, `export_key` (auto, tpm, software),
`tpm_device` (default `/dev/tpmrm0`), `retention` (days such as `365d`, a
duration, or `forever`; default `365d`).

## Testing

`make ledger-test` runs the unit tests in a Fedora container: the chain
across rotations, edits, removals, reordering and rewritten sealed files
detected, an interrupted rotation finished on restart, retention (expiry
recorded, verify passing, a removal without a record or with a forged one
detected, an interrupted removal finished), producer rules, refused
operations recorded, read isolation, filters, session attribution of
SELinux denials, signed exports with both key kinds and 0.1.0 exports, the
TPM commands against a strict simulator, the key choice, the journal and
snapshot mappings.
`scripts/lab/ledger-test.sh` runs the lab matrix on a VM (see
`docs/network.md`), including rewrite and delete attempts by users,
producers and root, reads across users, collectors (sudo, pkexec, failed
authentication, logins, snapshots, a rollback), sealed rotation, tamper
evidence on a copy, signed exports with the TPM key (a VM with an
emulated TPM), the software fallback and a required TPM that is missing,
and retention with a one-minute policy.

## Limits (today)

- Tamper evidence on the machine only; anchoring the chain head off the
  machine is future work.
- The TPM key proves "signed on this machine" only to someone who already
  trusts its public key; TPM attestation of the key is future work.
- A rollback is recognized from snapper's snapshot descriptions
  ("writable copy of #N", or a rollback description on a snapshot without
  a cleanup algorithm, as basalt-rollback writes it).
- Journal collectors start at the end of the journal on first start and
  then follow a saved cursor; events while the service is stopped are read
  on the next start, as long as the journal still holds them.
- `basalt ledger ...` works through the `basalt` command's fallback to
  `basalt-<name>` (docs/assistant.md); `basalt-ledger` is the same program.
