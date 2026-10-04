# Per-session egress: default deny with DNS-aware allowlists (basalt-resolver)

Every confined session on Basalt OS (an AI agent session started by
`basalt-agent`, later a confined app) gets its own network policy: nothing
leaves the session unless it goes to an address that one of the session's
allowed names resolved to, on a port that name allows. The policy is
enforced in the kernel with nftables, matched on the session's cgroup, and
filled from DNS answers by `basalt-resolver`, a small resolver owned by
Basalt. Everything else is dropped and recorded in the audit ledger
(`docs/ledger.md`).

```
basalt-resolver sessions          registered sessions (yours; all as root)
basalt-agent egress PROFILE       a profile's effective allowlist
basalt-agent egress propose PROFILE add|remove ENTRY [--system]
basalt-agent grant SESSION host NAME[:PORTS]
setsebool -P basalt_agent_direct_egress on    let native agents connect without the proxy
```

## How it works

```
 session slice (cgroup)                         kernel, table inet basalt_egress
 +-----------------------------+                +---------------------------------------+
 | agent (basalt_agent_t)      |--- DNS :53 --->| nat: redirect to 127.0.0.1:<port>     |
 | session proxy               |                | filter: session chain                 |
 +-----------------------------+                |   established       accept            |
              |                                 |   own resolver port accept            |
              | connect                         |   other DNS (53/853) log, drop        |
              v                                 |   proxy ports (lo)  accept            |
     allowed address:port? ---------------------|   addr . port in session set  accept |
                                                |   anything else     log, drop         |
                                                +---------------------------------------+
 basalt-resolver (basalt_resolver_t)
   one DNS port per session, answers only allowlisted names,
   adds each answer's addresses to the session set, records refusals
```

1. The launcher (`basalt-agent run`) starts the session proxy and, in
   native mode, the agent in a systemd user slice of their own,
   `basaltagent.slice/basaltagent-<id>.slice`, through `systemd-run --user
   --scope`.
2. It registers the slice with `basalt-resolver` over
   `/run/basalt-resolver/control.sock`: session id, the slice's cgroup path,
   the profile's allowlist and who the session is (profile, mode, SELinux
   level, project). The resolver checks with `SO_PEERCRED` that the cgroup
   lies inside the caller's own user manager and is owned by them, and
   that it is not one of the manager's shared slices.
3. The resolver opens a DNS port for the session (UDP and TCP on
   127.0.0.1, 47200 to 47263, SELinux type `basalt_resolver_port_t`) and
   installs, in one nftables transaction, two sets and two chains for the
   session, plus a jump from the table's `output` and `output_nat` chains
   matched with `socket cgroupv2 level N "<slice>"`.
4. The session's DNS to the stub addresses (127.0.0.53, 127.0.0.54) and to
   127.0.0.1:53 is redirected to its own port. Any other DNS (port 53, 853,
   5353 to anywhere) is dropped and recorded as `dns.direct`.
5. For an allowlisted name the resolver asks the system stub resolver,
   drops addresses that are not public unless the entry is marked
   `private` (DNS rebinding), adds each remaining address to the session
   set for the entry's ports, and only then answers. The set element lives
   for the record's TTL clamped to 1 minute to 1 hour, plus 5 minutes of
   grace; the answer's TTL never exceeds it. Connections already
   established stay up when an element expires.
6. Anything else the session sends is dropped. The first drop per
   destination per minute is logged through nflog and becomes an
   `egress.drop` record; counters keep the totals.
7. When the session ends, the launcher ends its registration and the
   resolver removes the chains, sets and port. A session whose launcher
   died is removed when its cgroup disappears (checked every 15 seconds).

The proxy runs inside the slice too, so its own connections are subject to
the same sets: a bug in the proxy cannot reach an address no allowed name
resolved to. The proxy uses the pure Go resolver (`GODEBUG=netdns=go`),
whose queries go to the stub address and are redirected like the agent's.

## Why a Basalt resolver, not unbound or dnsmasq

The job is per-session policy, not caching or recursion:

- Sessions share 127.0.0.1, so a resolver cannot tell them apart by client
  address; each session needs its own listener, created and removed with
  the session. dnsmasq's `--nftset` and unbound's views are configured per
  domain or per client network, globally, and change only with a reload.
- The set must be filled before the answer leaves, or the first
  connection races the update; with dnsmasq or unbound this needs a hook
  (an unbound Python module, or dnsmasq's asynchronous set updates), and
  still a separate service for sessions, rebinding per entry and records.
- The allowlist format, its matching rules and the "private" exception
  must be the same code as the proxy's, so the two filters can never
  disagree. The resolver carries the same parser (CI checks the copies are
  identical).
- Every refusal must become an audit record attributed to its session.

What remains is small: a forwarder to the system stub resolver (which
keeps doing caching, DNSSEC policy and upstream selection), a DNS message
parser for questions and address records, nftables changes through `nft`,
and nflog reading, in under 3,000 lines of standard-library Go with no third-party code.

## The allowlist

The same format as basalt-agent profiles (`docs/agents.md`): one entry per
line, names only (no IP literals), a wildcard only as the first label,
ports default to 443, `private` allows the name to resolve to loopback,
private or link-local addresses.

```
api.anthropic.com
registry.npmjs.org:443,80
*.githubusercontent.com
models.lab.example:11434 private
```

A profile can also say `loopback = no` in `[egress]`: by default a session
may reach unprivileged loopback ports (a dev server it started itself).
Note that this includes the host's own addresses, which the kernel routes
over loopback; profiles that do not need local services should turn it
off.

## What the resolver answers

| Query | Answer |
|---|---|
| A, AAAA, CNAME of an allowed name | the address records the upstream returned, rebinding-filtered, added to the session set first |
| HTTPS, SVCB of an allowed name | no data (clients fall back to A and AAAA; service bindings carry address hints the sets do not cover) |
| other types of an allowed name (TXT, MX, SRV) | passed through unchanged (no address is added) |
| any name not on the list | REFUSED, recorded as `dns.deny` |
| ANY, non-Internet class | REFUSED |

Because unlisted names are refused before any upstream query, a session
cannot leak data through DNS names it chooses (`secret.attacker.example`).
Wildcard entries still let it choose labels under the allowed suffix.

## Native mode and direct egress

Native-mode agents (`basalt_agent_t`) reach the network only through the
session proxy by default: SELinux lets them connect to the proxy port type
and nothing else. The kernel filter adds default deny for the proxy and
fail-closed behavior around it.

With the SELinux boolean `basalt_agent_direct_egress` on, native agents may
also resolve names and connect directly (tools that ignore proxy settings,
non-HTTP protocols on allowed ports). The kernel filter is then the
boundary for those connections. Two details keep the resolver in the path:

- Agents cannot read `/etc/resolv.conf`. On Fedora it points into
  `/run/systemd`, and being able to search that directory would also let an
  agent reach systemd-resolved's varlink socket (which the base policy
  opens to every domain) and resolve any name outside the session
  resolver. Without `resolv.conf` the C library and Go use 127.0.0.1:53,
  which the session rules redirect. The denial is not audited (it is
  expected on every lookup).
- Names in `/etc/hosts` never reach a DNS resolver, so they never fill a
  set: connections to them are dropped (closed, not open).

`basalt-agent run` refuses native mode when basalt-resolver is not running.
Container mode works without it (the container has no network at all) and
uses it when present, for the proxy.

## Changing an allowlist

Allowlists only grow through a person:

- `basalt-agent grant SESSION host NAME[:PORTS]`: one running session,
  administrator authentication (polkit), recorded; the root helper widens
  the proxy and the kernel filter.
- `basalt-agent egress propose PROFILE add|remove ENTRY`: a persistent
  change. It prints the proposal, the file it writes and the resulting
  list, asks for confirmation and records the change. User scope writes
  your own override in `~/.config/basalt-agent/profiles`; `--system` writes
  `/etc/basalt-agent/profiles` for every user and needs administrator
  authentication. Agents can do neither (SELinux keeps them out of your
  configuration, `no_new_privs` keeps them away from pkexec).

The resolver accepts allowlist changes only from root; a session's owner
can register and end sessions, nothing more.

## Records

All with the session's uid, session id and subject, so the owner sees them
in `basalt-ledger`:

| Event | When |
|---|---|
| `egress.session.start` | session registered (cgroup, resolver port, allowlist) |
| `egress.session.end` | session ended (counters: answered, refused, dropped) |
| `egress.session.refused` | a registration was refused (whose cgroup, why) |
| `egress.grant` | allowlist widened (or a refused attempt) |
| `dns.allow` | first answer per name and type in a session, with the addresses |
| `dns.deny` | a name not on the list |
| `dns.rebinding` | an allowed name pointed at a local or private address |
| `dns.direct` | DNS to another server |
| `egress.drop` | a connection to an address no allowed name resolved to |

Refusals and drops are throttled to one record per name or destination per
minute per session; the counters in `egress.session.end` hold the totals.

## Failure behavior

- The table and the session chains stay in the kernel when the service
  stops: sessions keep their sets until the elements expire and lose DNS,
  so new connections fail. Nothing opens up.
- On start the resolver reads its saved sessions (`/run/basalt-resolver/
  sessions`), reopens their DNS ports and keeps their rules; sessions whose
  cgroup is gone are removed.
- If the rules cannot be installed, the registration fails and the agent
  session does not start.
- Agents cannot take over a stopped resolver's port: binding it needs
  `name_bind` on `basalt_resolver_port_t`.

## SELinux

`basalt_resolver_t` (root service, `NoNewPrivileges`, only
`CAP_NET_ADMIN`) may: listen on its control socket and its port type, ask
the stub resolver, run `nft` (as `iptables_t`), read nflog over netlink,
look at cgroup directories and connect to the ledger. Users' domains may
connect to the control socket; agent domains may not.

## Configuration

`/etc/basalt-resolver/resolver.conf`: `upstream` (default the stub,
127.0.0.53:53), `ports`, `log_group` (nflog group, 47), `min_ttl`,
`max_ttl`, `grace`, `ledger`.

## Testing

`make resolver-test` runs the unit tests in a Fedora container (DNS
parsing and building, the ruleset, nflog parsing, the server with a fake
firewall and upstream: allowed names fill sets, unlisted names and
rebinding are refused, cgroup ownership checks, root-only grants,
restart). `scripts/lab/ledger-test.sh` runs the lab matrix on a VM: allowed
work with zero SELinux denials, then escape attempts (unlisted names
through the proxy and directly, DNS names carrying data, IP literals,
allowed addresses on other ports, other DNS servers over UDP, TCP and TLS,
another session's resolver, rebinding, leaving the cgroup, starting a new
scope, widening the allowlist, writing the ledger), a container session,
and a resolver restart while a session runs.

## Limits (today)

- The proxy's SELinux domain connects only to HTTP port types, so a
  `private` entry on another port (a model server on 11434) works through
  direct egress, not through the proxy.
- IPv6 DNS to `::1` is dropped, not redirected (the C library tries
  127.0.0.1 first).
- `loopback = yes` (the default) also covers the host's own addresses.
- One nflog group for all sessions; a flood is rate limited (10 records
  per second per session) and counted.
- Per-process accounting inside a session (which program opened what) is
  the next step (eBPF), not part of this service.
