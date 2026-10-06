# The approval gate (basalt-gate)

On Basalt OS, everything that changes something on behalf of someone who
is not touching the controls is a request, and a person agrees before it
runs. Until now each feature asked in its own way: the system assistant's
proposals and their confirmation code, the desktop shell's confirmation
sheet, agent grants with polkit, model downloads, terminal tools with
their own confirm dialog. basalt-gate is the one place where all of these
become a decision, so they share one queue, one history, one way to say
"you may do this again without asking", and one stop button.

```
basalt-gate status                      automation state, rules, waiting requests
basalt-gate request --action tool.exec --arg tool=backup --arg operation=prune \
    --arg 'argv:=["borg","prune","--keep-daily=7","/srv/backup"]' --arg class_hint=C2 --wait
basalt-gate queue                       what waits for you
basalt-gate show g-7f3a9c21d04b         who asks, what changes, the code
basalt-gate approve g-7f3a9c21d04b      polkit asks for your password
basalt-gate decline g-7f3a9c21d04b
basalt-gate stop                        stop all automation now, no password
basalt-gate resume                      polkit
basalt-gate rules list                  your rules, as sentences
basalt-gate rules add tidy.toml         a rule (approved like any request)
basalt-gate rules draft tidy.toml       its sentence and what it would have done last week
basalt-gate simulate --rules policy.toml --since 7d
basalt gate status                      the same through the basalt command
```

## Status

The gate runs on its own, and the existing approval paths are moving to
it one at a time (see "Migration" below). Each keeps today's behavior
when the gate is not installed or not running. The package is built and
published with the others and is not part of the default install.

## How a request is decided

```
 requester (a tool, an agent, the assistant, an app)
     |  propose: typed actions from the registry
     v
 basalt-gated (basalt_gate_t)
   1 who asks: uid, SELinux context, agent session, from the kernel
   2 validate the arguments against the action registry
   3 class from the registry (never from the requester)
   4 hard limits (locked: refused; always ask: never automatic)
   5 rules: the most restrictive matching rule wins; no match asks
   6 the emergency stop: every allow becomes ask
     |                                   |
     | allowed / refused                 | asked: the queue
     v                                   v
 executor claims once (digest)       a person decides on a trusted surface
     |
     v
 basalt-ledger: request, decision (which rule or person), claim, result
```

### Actions and classes

A request names actions of closed sets, registered in
`/usr/share/basalt/gate/actions.d/*.json`. The registry defines each
action: the schema of its arguments, its class, the resources it touches
(paths, units, packages, hosts, recipients), whether only the person or
only an agent may ask for it, and which program runs it. The first
registry files describe the actions that exist today without changing
them: the system assistant's 11 actions, the shell's 16 desktop actions
(plus agent control and screenshots, which only agents ask for, and the 5
that only the person's own words ask for), basalt-agent grants and
allowlist changes, model downloads, skill grants, rule changes, and
`tool.exec` for terminal tools.

| Class | Name | Examples |
|---|---|---|
| C0 | Look | reading a folder the person granted |
| C1 | Undoable | theme, windows, moving files inside a granted folder |
| C2 | System change | restarting a unit, an SELinux label, a driver (snapshot first) |
| C3 | Leaves the computer | sending mail, a new host for an agent, a screenshot for an agent |
| C4 | Critical | power, agent control of the desktop, rule changes |
| C5 | Cannot be undone | deleting a snapshot, emptying the trash |

A request's class is the highest of its calls. A requester may raise the
class with a hint, never lower it.

### Rules

Rules are data, decided without any model. A model may draft one; a
person confirms it, after seeing it as a sentence and as a dry run.

```toml
[[rule]]
id = "r-tidy"
effect = "allow-tell"                      # ask, ask-remember, allow-tell, allow-quiet, refuse
actions = ["group:files.tidy"]             # an action, group:NAME, class:Cn, or *
requesters = ["assistant"]                 # person, assistant, system-assistant, agent, agent:NAME, app:NAME, tool:NAME, any
resources = { path_beneath = ["~/Downloads"], exclude = ["~/Downloads/keep"] }
when = { days = "mon-fri", hours = "09:00-18:00", power = "ac" }
limits = { per_run_items = 500, per_day = 1, requester_per_hour = 30 }
taint_max = "system"
expires = "90d"
```

- The most restrictive matching rule wins: refuse, then ask, then
  ask-remember, then allow-tell, then allow-quiet. An allow rule can never
  override an "always ask" or "never allow" rule, whoever wrote them. A
  request no rule covers asks a person.
- Every party of the request must match `requesters`: a rule for the
  assistant does not cover a request an agent made through the
  assistant.
- An allow rule covers a call only when every resource of the call is in
  its scope; an ask or refuse rule applies as soon as one is. A
  destination (a host, a recipient) is covered only when the rule lists
  it: a new destination always asks.
- `taint_max` (default `system`): a request that follows web pages, mail
  or files does not match allow rules beyond it. Requests from agents
  carry at least `web`, because agents read untrusted content by nature.
- Conditions are in local time. Power comes from the power supply; a
  condition the gate cannot read yet (presence, network) never lets an
  allow rule match and always keeps an ask or refuse rule in force.
- Limits turn an allow into ask and pause the rule until a person looks
  (`basalt-gate rules unpause`), with a `gate.limit` record.
- Rules above Undoable that allow must expire: 90 days by default, a year
  at most.
- A rule with a `trigger` matches only the requests the gate itself
  starts for it, never a request that content caused.
- Scopes: user rules (each user's own, C0 to C3, never a system action)
  and system rules (administrator authentication).
- `ask-remember`: the person approves with `--remember` and a narrow
  temporary rule (the same actions, requester and resources) allows it for
  the rule's `remember` time.

Presets are rule sets the person picks: careful (the default: asks
before every change; it only lets the person's own reads run), balanced
(the person's and the assistant's undoable requests run and tell; the
system assistant's fixes ask once and are remembered for 8 hours),
hands-off (a template for chosen agents working inside a chosen folder,
for a few days). Presets never hold "ask" rules, so the person's own
allow rules always can apply.

Rule changes are requests themselves (`gate.rule.change`, Critical,
always asks), carried out by the gate when a person approves.
Tightening takes effect at once: adding "always ask" or "never allow", or
removing an allow rule, through `basalt-gate rules add` or `remove` with a
password at the terminal.

### Hard limits

Checked before any rule (`/usr/share/basalt/gate/hardlimits.json` plus
built-in ones that always hold).

Locked, refused whatever a rule says: SELinux to permissive, removing
Basalt policy modules, the agent confinement booleans, Secure Boot keys
and enrolled MOKs, turning off or rewriting the ledger, turning off the
gate or loosening its limits, removing disk encryption or the last
recovery key, wiping disks, reading saved secrets in bulk, adding an
administrator. For terminal tools the command line itself is checked:
`setenforce 0`, `mkfs`, `wipefs`, `dd of=/dev/sda`, `cryptsetup
luksKillSlot`, `mokutil --delete`, `usermod -aG wheel`, stopping the
ledger or the gate, also behind sudo, pkexec or env. Unlocking one
occurrence needs a second factor at the computer itself, which this
version does not have yet: locked actions stay locked.

Always ask, never automatic: deleting a snapshot, emptying the trash,
anything that cannot be undone, power or logout asked by anyone but the
person, agent control of the desktop and screenshots for agents, rule
changes, payments.

Actions only the person's own words may ask for (typing dictated text,
sending mail, moving files, spoken answers, power from the command bar)
are refused to agents before any rule, including through an
intermediary.

## Who decides

| Surface | SELinux type | Authentication |
|---|---|---|
| `basalt-gate approve` (the terminal decider `basalt-gate-tty`) | `basalt_gate_tty_t`, entered from user domains only | polkit on every approval: your password (kept 5 minutes) for your own undoable and outgoing requests, an administrator for system changes, Critical and Cannot be undone |
| the shell's confirmation sheet, the Approvals app (when they move to the gate) | `basalt_shell_ui_t`, `basalt_app_approvals_t` | the session; polkit (administrator) for system changes, Critical and Cannot be undone |

Agents ask and never decide: the gate refuses a decision from any other
peer and records the attempt, and SELinux keeps agent domains out of the
decider domain. Declining never needs a password. A decider sees its own
user's requests and the system's; another user's requests need an
administrator.

`basalt-gate approve` runs `pkttyagent` for the terminal decider's
process, so polkit asks for the password in your terminal. Approving
several requests at once never includes Critical or Cannot be undone
requests, nor requests of different requesters when one of them follows
untrusted content.

Requests wait 5 minutes, or 24 hours when the requester marks them
deferrable (an unattended agent can leave a request for the morning).
Identical pending requests from the same requester are one item.

## Executors and claims

The gate decides; it does not run anything (rule changes are the
exception). The program that runs an action claims the decision once,
with the digest of what it is about to run: the SHA-256 of the canonical
JSON of the calls, the preview and the resources (PROTOCOL.md). A claim
from anyone but the registered executor, a second claim, a claim older
than 10 minutes, or a different digest is refused and recorded; a
different digest voids the decision. Terminal tools run their own
`tool.exec` requests after `allowed`, and report the result.

The short code people read is the first 8 hex digits of the digest; for
the system assistant's proposals it is the code `basalt show` already
prints.

## Emergency stop

`basalt-gate stop` (or the `stop` operation from any program, an agent
included, without authentication) suspends every allow rule at once:
every request asks a person until someone resumes. The queue is kept and
the stop survives a restart. Resuming is a loosening: only a decider,
with polkit at the terminal.

## Dry runs

Every request is recorded with its metadata (actions, requester chain,
class, resources, taint, conditions, outcome), never its content.
`basalt-gate rules draft FILE` shows a rule's sentence and replays the
last week against your rules plus the draft; `basalt-gate simulate
--rules FILE` replays against a whole rule file; `--records FILE` replays
a ledger export instead of the ledger. Users replay their own records;
root replays all.

## Migration

Every approval path that existed before the gate moves to it in steps.
`enforce` in `/etc/basalt-gate/gate.conf` lists the paths where Basalt's
own components let the gate decide; on the others they keep their own
confirmation and tell the gate what they decided (shadow mode: the
`observe` operation, recorded as a request with `observed: true` and what
the gate would have decided, so `basalt-gate simulate` has real data
before the switch). Without the gate nothing changes at all. A component
asks which paths are enforced in `hello`.

| Path | `enforce` name | Default | With the gate deciding |
|---|---|---|---|
| The system assistant's proposals (`basalt apply`) | `apply` | shadow | see below |

The system assistant's proposals (`apply`). `basalt apply ID` queues the
proposal in the gate (calls: its typed actions; reference: its id;
preview: its title, actions and exact command lines), so the short code
in the queue is the fingerprint `basalt show` prints. Root's typed yes
at the terminal, or `--yes --confirm CODE`, is recorded as the person's
decision (`person:tty-root`; `allow_code_confirm = no` turns the code
off); a decision in the queue (`basalt-gate approve`, the desktop shell)
or a system rule counts the same. Once approved, the gate starts
`basalt-gate-exec@REQUEST.service`, which runs `basalt apply REQUEST
--gate` in its own SELinux domain (`basalt_gate_exec_t`, the only type
the gate lets claim these requests): it reads the request, rebuilds the
calls and the preview from the stored proposal, claims the decision with
them (a different proposal is a digest mismatch and voids the decision),
applies it with its snapshots, checks and audit record, and reports the
result and the snapshots. `basalt apply` follows the unit and prints
what it did. `basalt submit ID` queues a proposal without applying it
(the desktop shell uses it before the person decides on its sheet). In
shadow mode `basalt apply` asks at the terminal exactly as before and
records the outcome with `observe`.

## Ledger records

Producer `basalt-gate`, accepted by basalt-ledger only from root in
`basalt_gate_t`:

| Event | What |
|---|---|
| `gate.request` | id, actions, requester and chain, taint, origin, class, resources (clipped), digest, conditions |
| `gate.decision` | outcome (allowed, approved, asked, refused, declined, expired, cancelled) and by whom: `rule:<id>@<hash>`, `person:<surface>`, `hard-limit:<tier>:<name>`, `stop`, `limit:<rule>`, `default` |
| `gate.claim`, `gate.result` | executor, digest match, outcome |
| `gate.rule.add`, `gate.rule.change`, `gate.rule.remove` | the rule, its hash and sentence, who confirmed it |
| `gate.stop`, `gate.resume`, `gate.unlock`, `gate.limit` | who, from where, what tripped |
| `gate.start`, `gate.seal_error` | rule count, preset; a rule file that failed its seal (critical) |

`basalt-ledger --producer basalt-gate` shows them in plain English.

## Storage

`/var/lib/basalt-gate` (root only, `basalt_gate_var_lib_t`, written by
`basalt_gate_t` only): `rules.json` (sealed with an HMAC key the gate
keeps there; a mismatch loads only the careful preset, moves the file
aside and records a critical event), `state.json` (the stop, paused rules,
counts for limits), `queue.json` (requests, so a restart keeps what
waits).

## SELinux

`basalt_gate_t` (root, no capabilities, `NoNewPrivileges`, no network)
may: manage its state and socket, read its registry, presets, hard limits
and configuration, read the `/proc` entries of connecting processes, read
the power supply in sysfs, run `pkcheck` and talk to polkit over the
system bus, connect to basalt-ledger, and start the executor unit
`basalt-gate-exec@.service` with systemctl (that unit only).
`basalt_gate_exec_t` is entered only by systemd from that unit's program;
it keeps the root powers `basalt apply` has, and no agent domain may
enter or trace it. Requesters connect through the
interface `basalt_gate_stream_connect`; user domains and agent domains
may connect. `basalt_gate_tty_t` is entered only from user domains
(`basalt_gate_run_tty`). The module forbids every agent domain
(`neverallow`) to read or write the rule store, write the configuration,
enter or trace the gate's domains (the executor's included), or execute
the terminal decider.

## Configuration

`/etc/basalt-gate/gate.conf`: `enforce` (the migration paths the gate
decides, default `skills models consent`), `exec_units` (yes: start an
executor's unit once approved), `systemctl`, `root_relays`
(system-assistant: what root may say it asks as), `socket`, `state`, `registry`, `presets`,
`hardlimits`, `ledger`, `default_preset` (careful), `allow_code_confirm`
(yes: root's `basalt apply ID --yes --confirm CODE` stays a person's
decision once the system assistant uses the gate), `interactive_expiry`
(5m), `deferrable_expiry` (24h), `claim_window` (10m),
`agent_taint_floor` (web), `requests_per_minute` (30), `deciders`,
`tty_deciders`, `relays` (SELinux types that may say on whose behalf
they ask: the shell daemon for the person, the assistant and agents; the
system assistant for itself and agents), `agent_types`.

## For tool authors

`packages/basalt-gate/PROTOCOL.md` is the protocol, MIT OR Apache-2.0,
with a subset of five messages for tools outside Basalt and a fake gate
(`basalt-gate fake-server SOCKET`) to test a client against. The Go
client is `pkg/gate` (standard library only, same licenses). Without the
gate a tool behaves as on any other system.

## Testing

`make gate-test` runs the unit tests in a Fedora container: the policy
matrix, careful wins, hard limits (also behind sudo and env), requests
after untrusted content, new destinations, the stop, limits and the
circuit breaker, conditions, expiry, schedules, user scope, presets,
remember, deciders and polkit, claims (replay, wrong executor, digest
mismatch, the claim window), rule changes, seal tampering, dry runs, rate
limits, the protocol fixtures against the fake and the real gate, and
digest vectors computed independently in Python.
`scripts/lab/gate-test.sh` runs the lab matrix on a VM with SELinux
enforcing (see the script).

## Limits (today)

- The approval paths move to the gate one by one (Migration); until a
  path is enforced, its own confirmation decides.
- The executor of the system assistant's proposals keeps the powers
  `basalt apply` has as root (its SELinux type is its identity for the
  gate, not a confinement of the actions yet).
- The seal key is a software key; a TPM-bound key comes later.
- Presence and network conditions are not read yet (treated as unknown,
  which never allows).
- Unlocking a locked action needs a second factor that does not exist
  yet, so locked stays locked.
- Scheduled triggers are parsed and matched, but the gate does not start
  jobs yet.
- The emergency stop does not freeze agent sessions yet.
- No phone approvals, no D-Bus interface, no Approvals app, no
  `basalt approve` terminal interface beyond the command line.
