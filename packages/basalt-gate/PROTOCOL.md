# The Basalt approval gate protocol, gate/1

SPDX-License-Identifier: MIT OR Apache-2.0

This document and the reference client (`pkg/gate`) are dual-licensed MIT
OR Apache-2.0, so any program can carry its own client written from this
text, and another system can implement the server. The rest of
basalt-gate is Apache-2.0.

The gate is the one local service where a request for a side effect
becomes a decision. A program that wants something done (a terminal tool,
a script, an agent, the assistant, an app) sends a request; the gate
answers "allowed", "refused", or "asked" (a person decides); the program
that runs the action claims the decision once, bound to a digest of
exactly what was approved.

Most programs need only the third-party subset at the end of this
document: five message shapes and one discovery rule.

## Transport

- A Unix stream socket, `$BASALT_GATE_SOCKET`, default
  `/run/basalt-gate/gate.sock`.
- Newline-delimited JSON: one request object per line, one reply object
  per line, in order, any number per connection. Lines are UTF-8, at most
  256 KiB.
- No authentication handshake: the gate identifies the peer from the
  kernel (SO_PEERCRED: uid and pid; SO_PEERSEC: the SELinux context; the
  process's cgroup for agent sessions). Nothing a client says about itself
  raises what it may do.
- Every reply has `ok`. `"ok": false` comes with `error` (a sentence) and
  means the request could not be handled at all (bad format, not allowed
  for this peer, unknown id). A request the gate handled but refused is
  `"ok": true, "decision": "refused"` with a `reason` to show.

## Feature detection

The gate is present when the socket exists and answers `hello` within
500 ms with a `protocol` of the same major version (`gate/1`). Absent,
unreachable, silent, or another major version: the program behaves
exactly as it does on any other system (its own preview and
confirmation). A program never fails because the gate is missing.

## Who is who

The gate classifies each connection from the kernel's view:

| Peer | Kind | May |
|---|---|---|
| a user's own programs (user domains) | `tool` (named from `hello.client`) | request; run its own `tool.exec` requests |
| agent domains (`basalt_agent*`, `container_*`, `basalt_skill*`) | `agent` (named from its session) | request only |
| the system assistant (`basalt_assistant_t`) | `system-assistant` | request, with its preview |
| desktop apps (`basalt_app_<name>_t`) | `app` | request |
| trusted relays (the shell daemon, the system assistant) | as reported, within the kinds each may report | request on behalf of someone |
| the terminal decider (`basalt_gate_tty_t`) | decider | decide, with polkit on every approval |
| the shell UI, the Approvals app | decider | decide; polkit for system, Critical and Cannot be undone |
| executors (types named in the action registry) | executor | claim and report results |

Requester kinds: `person`, `assistant`, `system-assistant`, `agent`,
`app`, `tool`, `schedule` (set only by the gate itself), `phone`.

## The request

A requester sends calls of registered actions:

```json
{"op": "propose",
 "calls": [{"action": "files.trash", "args": {"paths": ["~/Downloads/setup.iso"]}}],
 "group": "tidy-downloads",
 "taint": "none",
 "deferrable": false,
 "class_hint": "C1",
 "via": [{"kind": "assistant"}]}
```

- `calls`: 1 to 32 calls. An action is defined by the registry
  (`/usr/share/basalt/gate/actions.d/*.json`), never by the client: its
  argument schema, class, resources, who may ask for it, and its
  executor. Unknown actions and arguments are refused.
- `group`: requests of one requester that a person may approve together.
- `taint`: `none`, `system`, `personal` or `web`: the request follows
  untrusted content (a web page, a mail, a file). Requests from agents
  carry at least `web`.
- `deferrable`: may wait up to 24 hours instead of 5 minutes.
- `class_hint`: raises the class of every call (never lowers it).
- `via`: intermediaries; they only narrow which rules match.
- `ref` (system assistant only): its proposal id, for the short code.
- `preview` (trusted relays and the system assistant only): the preview
  its planner built; for everyone else the gate builds the preview from
  the registry, so a requester cannot describe its request in its own
  words.
- `on_behalf` (trusted relays only): `{"kind", "name", "session", "via"}`.

The gate fills in the rest of the proposal: the requester (from the
kernel), the class, the resources, the preview, the expiry, the digest and
the short code.

## Classes

| Class | Name | Meaning |
|---|---|---|
| C0 | Look | reads; nothing changes, nothing leaves |
| C1 | Undoable | the person's own things, can be undone |
| C2 | System change | root-level, with a snapshot and a rollback |
| C3 | Leaves the computer | sends, shares, publishes; cannot be called back |
| C4 | Critical | security, identity, power, the gate itself |
| C5 | Cannot be undone | destroys data with no copy |

A request's class is the highest class of its calls.

## Decisions

`allowed` (a rule, or a person approved), `asked` (waits for a person),
`refused` (a rule, a hard limit, the registry), `declined` (a person said
no), `expired` (nobody decided in time), `cancelled` (the requester
withdrew it). Replies carry `by`: `rule:<id>@<hash>`, `person:<surface>`,
`hard-limit:locked:<name>`, `hard-limit:always-ask:<name>`, `stop`,
`limit:<rule>`, `registry`, `default` (no rule covered it).

## Operations

| op | Who | Request members | Reply members |
|---|---|---|---|
| `hello` | anyone | `role`, `client`, `protocol` | `protocol`, `roles`, `kind`, `enforce` |
| `propose` | requester | see above | `id`, `decision`, `class`, `reason`, `by` |
| `check` | requester | as `propose` | `decision`, `class`, `reason`, `by` (nothing is queued) |
| `wait` | the requester, a decider | `id`, `timeout` (seconds, 1 to 600, default 180) | `id`, `decision`, `class`, `reason`, `by`, `timed_out` |
| `status` | the requester, a decider | `id` (without: the gate's state) | `request` (a view), or `status` |
| `cancel` | the requester | `id` | `id`, `decision` |
| `pending`, `history` | the requester (own), a decider | `limit` | `requests` |
| `subscribe` | the requester (own), a decider | | then one `event` per line |
| `decide` | decider | `id`, `ids` or `group`; `approve`; `remember` | `decided` (id to decision) |
| `claim` | executor | `id`, `digest` (or `calls` and `preview`), `executor` | `request` (with its calls), `digest` |
| `result` | the claimer | `id`, `ok`, `exit`, `detail`, `snapshots` | `id` |
| `stop` | anyone | `reason` | |
| `resume` | decider | | |
| `rules.list` | not agents | | `rules` (with sentences) |
| `rules.draft` | not agents | `rule`, `scope`, `since` | `rules`, `simulation` |
| `rules.simulate` | not agents | `rules` (a rule file's text), `scope`, `since`, `records` | `simulation` |
| `rules.apply` | decider | `rule`: `{op, scope, rule or id}`, tightening only | |
| `rules.unpause` | decider | `id` (the rule key) | |
| `observe` | not agents | as `propose`, plus `outcome` and `decided_by` | `id`, `decision` (what the gate would have decided), `class`, `reason`, `by` |
| `confirm` | root outside agent domains | `id`, `code`, `mode` (`code` or `terminal`) | `id`, `decision`, `by` |
| `unlock` | | | refused in this version |

`enforce` (in the hello reply) lists the migration paths where Basalt's
own components let the gate decide: `apply` (the system assistant's
proposals), `shell` (the desktop shell's proposals), `skills`, `models`,
`consent`, `agent`. On the other paths they keep their own confirmation
and only send `observe` (shadow mode): what their confirmation decided
(`outcome`: approved, declined, expired, refused, failed or cancelled;
`decided_by`: who), which the gate records with what it would have
decided, queueing nothing. Third-party tools ignore it.

`confirm` is the system assistant's compatibility path: root at a
terminal confirms one of its proposals (`basalt apply ID`, a typed yes, or
`--yes --confirm CODE`) with the short code; recorded as
`person:tty-root`. `allow_code_confirm = no` in the gate's configuration
refuses the `code` mode.

A request view (`request`, `requests`) holds: `id`, `group`, `ref`, `decision`,
`class`, `class_name`, `actions`, `who`, `requester`, `via`, `uid`,
`taint`, `origin`, `calls`, `preview`, `resources`, `leaves`,
`reversible`, `created`, `expires`, `deferrable`, `by`, `reason`,
`remember` (offered "approve and remember" duration), `needs_admin`,
`digest`, `code` (deciders only), `executor`, `claimed`, `result`.

Never returned to a requester: the short code, a decision token, a claim.
A requester cannot decide its own request.

Rule changes are requests too: the action `gate.rule.change` with
`{"op": "add", "scope": "user", "rule": RULE}` (scope `user` or `system`,
RULE a rule object as in docs/gate.md) or `{"op": "remove", "scope":
"user", "id": "r-tidy"}`. It is Critical and always asks; the
gate carries it out when a person approves. Tightening (adding an
"always ask" or "never allow" rule, removing an allow rule) takes effect
at once from a decider through `rules.apply`.

## Claims

An executor runs an action only with a claimed decision:

```json
{"op": "claim", "id": "g-7f3a9c21d04b", "digest": "sha256:<64 hex digits>", "executor": "basalt-shell"}
```

- The peer must be the action's executor (its SELinux type and uid, from
  the registry).
- The request must be allowed, unclaimed, and decided within the claim
  window (10 minutes).
- `digest` is what the executor computed from what it is about to run.
  A different digest voids the decision (the request is refused as
  stale, and must be planned and decided again).
- A claim succeeds once.
- Instead of `digest`, an executor may send `calls` (and `preview` when it
  planned the preview): the gate validates them through the registry,
  extracts the resources for the requester and computes the digest
  exactly as for the request, then compares.
- The executor of an action may read the requests addressed to it
  (`status`), so it can find what to run before claiming.
- An executor whose registry entry names a `unit` (a systemd template,
  `basalt-gate-exec@.service`) is started by the gate with the request id
  as the instance once the request is approved; a unit that cannot be
  started is recorded as the request's result.

Actions whose executor is the requester (`tool.exec`) are claimed for the
requester when `propose` or `wait` returns `allowed`.

## Digest

The digest binds a decision to its content:

```
digest = "sha256:" + hex(SHA-256(canonical(normalize(
            {"calls": CALLS, "preview": PREVIEW, "resources": RESOURCES, "ref": REF}))))
```

Normalization: in `preview`, in each preview line and in each resource,
members whose value is null, false, 0, "", [] or {} are removed; `ref` is
removed when empty; nothing is removed inside call arguments, title
arguments or line arguments.

Canonical JSON (the subset of RFC 8785 that the gate uses):

- object members sorted by key; keys are ASCII;
- no whitespace;
- strings in UTF-8; only `"`, `\` and control characters below U+0020
  are escaped (`\b`, `\t`, `\n`, `\f`, `\r`, others as `\u00xx`, lower
  case); everything else, including `<`, `>`, `&`, U+2028 and U+2029, is
  written as is;
- numbers are integers between -(2^53 - 1) and 2^53 - 1, in plain
  decimal; fractions and exponents are refused;
- `true`, `false`, `null`.

Python's `json.dumps(v, sort_keys=True, separators=(",", ":"),
ensure_ascii=False)` produces the same bytes for this subset.
`testdata/protocol/digest.json` holds vectors computed that way.

## Short code

The code people read and type: the first 8 hex digits of the digest. For
a system assistant proposal (with `ref` and `preview.commands`) it is
that proposal's fingerprint instead: the first 8 hex digits of
SHA-256(ref + "\n" + the command lines joined with "\n"), so the code
`basalt show` prints is the code in the queue.

## The third-party subset

A tool outside Basalt needs this and nothing else.

1. Feature detection (above).
2. `{"op": "hello", "role": "requester", "client": "tui-systemd/0.4.0", "protocol": "gate/1"}`
3. `propose` with calls of the generic action `tool.exec`:

   ```json
   {"op": "propose", "calls": [{"action": "tool.exec", "args": {
     "tool": "tui-systemd", "operation": "unit.restart",
     "argv": ["systemctl", "restart", "nginx.service"],
     "description": "Restart nginx", "destructive": false, "class_hint": "C2"}}]}
   ```

   The answer is `allowed` (run it now), `refused` (show `reason`, run
   nothing), or `asked` (with an `id`). A missing `class_hint` counts as
   C2; `destructive: true` makes it C5; the tool's registry file, when
   Basalt installs one, may raise the class, never lower it. Hard limits
   apply to the command line (wrapping it in sudo, pkexec or env does not
   hide it).
4. `{"op": "wait", "id": "g-000000000001", "timeout": 180}` until `allowed`,
   `declined`, `refused` or `expired` (show "Waiting for approval" with a
   cancel key; on timeout `timed_out` is true and the decision is still
   `asked`).
5. `{"op": "result", "id": "g-000000000001", "ok": true, "exit": 0}` after
   running (optional but expected).

That is the whole contract: no authentication, no library, no other
message.

## Fixtures and conformance

`testdata/protocol/` holds the subset's exchange (`hello.json`,
`propose-asked.json`, `propose-allowed.json`, `propose-refused.json`,
`wait-allowed.json`, `result.json`) and the digest vectors. To test a
client, run the fake gate and point the client at it:

```
basalt-gate fake-server /tmp/fake-gate.sock
BASALT_GATE_SOCKET=/tmp/fake-gate.sock your-tool
```

The fake answers deterministically (refused when `destructive` is true or
`class_hint` is C5, allowed for C0 and C1 hints, asked otherwise; `wait`
declines when argv holds `--decline`) and, when stopped, reports every
message that did not follow this document. The gate's own tests check
that the fake answers exactly the fixtures and that the real gate gives
the same decisions, classes and members.

## Versions

`gate/1` is this document. New optional members and new operations may
appear within `gate/1`; clients ignore members they do not know. A change
that breaks a client is `gate/2`, and feature detection treats it as no
gate.
