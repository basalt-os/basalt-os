# Milestone 2a: the system assistant without a language model

Status: done in the lab, 2026-10-03. Design and usage:
[assistant.md](assistant.md).

## Goal

The foundation of the Basalt OS system assistant, with no language model:
diagnosers, a command line, an event engine, typed proposals with
confirmed, verified and audited changes, a decision layer with
probabilities, MCP tools, and SELinux confinement. A model can later
answer the decision layer's questions or translate requests into the same
typed actions; the safety properties do not depend on it.

## What was built

| Part | Result |
|---|---|
| Package | `basalt-assistant` (Go, no third-party modules) and `basalt-assistant-selinux`, built in a Fedora 44 container by `packages/basalt-assistant/build.sh`, also from `scripts/build-rpms.sh`, so CI builds and lints it; rpmlint: 0 errors |
| CLI | `basalt status`, `why`, `fix selinux`, `snapshots` (list, diff, rollback), `disk`, `pending`, `show`, `apply`, `ignore`, `audit` |
| Proposals | typed actions from a closed set of ten; commands rebuilt from the actions at preview and at apply; strict parameter validators; no free-form command, no `audit2allow` |
| Apply | full preview; interactive `yes` or `--yes --confirm CODE` (code bound to the exact commands); snapper pre/post snapshots; stop at the first failure; verification checks per action; audit record |
| Audit | hash-chained JSON lines, root-only, append-only (`chattr +a` via tmpfiles.d), mirrored to the journal with each record's hash; in `/var/log`, so it survives rollbacks |
| Event engine | `basalt-assistantd`: journal follower (unit failures, AVCs from the audit transport, rpm `SOFTWARE_UPDATE` failures, ENOSPC), dnf5 log watcher (scriptlet failures), disk polling with a forecast, unfinished transactions; settle batching, dedup into open proposals, hourly rate limit, stale proposals closed when the problem goes away |
| Decision layer | typed questions (choice, score, boolean) with a probability per option; `rules/v1` backend; thresholds per question; every decision in the audit log; a model backend seam (OpenAI-compatible, not implemented, falls back to rules and says so) |
| MCP | `basalt-mcp`, protocol 2025-06-18 over stdio: 9 read tools, 5 propose tools; nothing executes |
| SELinux | module `basalt_assistant`: `basalt_assistant_t` for the daemon and the MCP server; reads what diagnosis needs, writes only its state directory and appends to its audit log; no transitions out |
| Tests | 10 Go test packages, fixtures recorded in the lab (journal JSON of real nginx failures, AVC records, `sesearch`, `seinfo`, `btrfs`, `snapper`, `ss` output); a lab script for every scenario |

## Lab results

One VM (Fedora 44 base, Basalt OS installer, unencrypted install, 2
vCPU, 4 GiB, 16 GiB disk), `basalt-assistant` installed from the signed
lab repository, the daemon enabled, SELinux enforcing. `make
lab-assistant-test` ran every scenario in one pass: PASS.

| Scenario | Event seen by the daemon | Proposal | After the confirmed apply |
|---|---|---|---|
| nginx logs to a directory moved from `/root` (keeps `admin_home_t`); no AVC is logged, a dontaudit rule hides it | unit failed, after 11 s | `unit.cause` selinux_denial 0.94, `avc.class` mislabeled 0.90: `restorecon -Rv /srv/nginx-logs`, `systemctl restart nginx.service` | label verified, nginx active, 0 new denials |
| nginx on tcp 8085 (`unreserved_port_t`), AVC logged | unit failed, after 11 s | selinux_denial 0.96, port 0.91: `semanage port -a -t http_port_t -p tcp 8085`, restart | port rule verified, nginx active and answering on 8085, 0 new denials |
| unknown directive in `nginx.conf` | unit failed, after 6 s | config_error 0.86: restore `nginx.conf` from the newest snapshot whose copy was in place at the last successful start, `restorecon`, restart | file matches the snapshot, nginx active |
| undo of that apply | | `basalt snapshots rollback --before ID`: `basalt-rollback --yes 59` (the apply's pre snapshot) | rollback pending verified; after the reboot the broken file is back, exactly the pre-apply state |
| a package whose `%post` fails (installed, not set up; rpm reports success, dnf5 logs a warning) | dnf5 log watcher, proposal after 53 s | `dnf.next` rollback 0.80: `basalt-rollback --yes 63` (the transaction's pre snapshot) | after the reboot the package is gone |
| an 11.3 GiB file kept only by a snapshot, disk at 89 % | disk poll, after 27 s | daemon (confined, cannot measure snapshot space): `disk.cause` other_data, report for review; `basalt disk` as root: snapshots 0.83, `snapper -c root delete 67` | subvolume cleanup verified, disk back to 24 % |
| confinement | | | daemon in `system_u:system_r:basalt_assistant_t:s0`; 0 denials of the domain during all scenarios, on every boot; the probe (the daemon binary run in its domain without the unit's hardening) could create files only in `/var/lib/basalt-assistant`: `/etc`, `/root`, `/var/tmp`, `/srv`, `/var/log` and a new file next to the audit log were refused |
| MCP | | `basalt_why_unit` answered; `basalt_propose_action` stored a boolean proposal | the boolean stayed off; the proposal waited for a person |

Also seen in earlier runs: a failing `%pre` (package not installed) gives
`dnf.next` investigate 0.80 and no change; a wrong confirmation code is
refused and recorded; an apply whose verification fails is marked failed
with its outputs and the undo command; port 8181 (`intermapper_port_t`)
gives a port relabel below the threshold, so only a report.

Daemon footprint on the VM: about 14 MiB resident, 0.6 s CPU over the
scenarios.

### Transcript excerpts

The daemon's proposal for the moved log directory, as printed to its
journal and by `basalt show`:

```
[p-97dfa5] nginx.service failed: selinux denial
  status: pending, from daemon, 2026-10-03 15:42:35 UTC

What is wrong
  nginx.service was stopped by SELinux denials for its domain httpd_t. /srv/nginx-logs is labeled
  admin_home_t but the policy says var_t, which httpd_t may use; it was probably moved here (mv keeps
  labels) or created with the wrong label.

Evidence
  - state: failed/failed, result exit-code, main process 0 status 0, restarts 0
  - journal: 15:42:24 nginx: [emerg] open() "/srv/nginx-logs/access.log" failed (13: Permission denied)
  - httpd_t may not add_name the dir /srv/nginx-logs (labeled admin_home_t); no AVC was logged, so a
    dontaudit rule hides the denial
  - policy default label for /srv/nginx-logs: var_t
  - not checked here: config syntax check `nginx -t` (run `basalt why nginx` as root)

Decision
  - unit.cause: selinux_denial (p=0.94, threshold 0.75, rules/v1); selinux_denial 0.94, unknown 0.01
  - avc.class: mislabeled (p=0.90, threshold 0.75, rules/v1); mislabeled 0.90, unknown 0.04
  - event.severity: 4 (p=0.60, threshold 0.75, rules/v1)
  - event.notify: true (p=0.90, threshold 0.75, rules/v1)

Proposed change
  $ restorecon -Rv /srv/nginx-logs    # reset the label of /srv/nginx-logs to the policy default
  $ systemctl restart nginx.service    # restart nginx.service

  Apply:  sudo basalt apply p-97dfa5      (non-interactive: sudo basalt apply p-97dfa5 --yes --confirm 19a12e65)
  Ignore: sudo basalt ignore p-97dfa5
```

The apply:

```
These 2 command(s) will run as root, in this order, with a snapshot before and after:
  $ restorecon -Rv /srv/nginx-logs
  $ systemctl restart nginx.service

pre snapshot 55
$ restorecon -Rv /srv/nginx-logs
  Relabeled /srv/nginx-logs from unconfined_u:object_r:admin_home_t:s0 to unconfined_u:object_r:var_t:s0
$ systemctl restart nginx.service
post snapshot 56

Verification
  [ok] /srv/nginx-logs carries its default label: /srv/nginx-logs verified.
  [ok] nginx.service is active: active
  [ok] no new SELinux denials since the change: 0 denials

p-97dfa5: applied (audit record #144)
Undo with: sudo basalt snapshots rollback --before p-97dfa5   (snapshot 55)
```

The failed package transaction:

```
[p-9fa660] package transaction failed: dnf -y install /root/basalt-lab-postfail-1-1.noarch.rpm
What is wrong
  The transaction "dnf -y install /root/basalt-lab-postfail-1-1.noarch.rpm" failed after changing
  packages; rolling the root back to snapshot 63 (taken just before it) is proposed.
Evidence
  - snapshot 63 (pre, 2026-10-03 15:43:51) was taken just before the transaction
  - dnf5.log: 2026-10-03T15:43:51+0000 [1703] WARNING [rpm] %post(basalt-lab-postfail-1-1.noarch)
    scriptlet failed, exit status 1
Decision
  - dnf.next: rollback (p=0.80, threshold 0.75, rules/v1); rollback 0.80, investigate 0.20
Proposed change
  $ basalt-rollback --yes 63    # make snapshot 63 the root at the next boot (data subvolumes untouched)
```

The audit log, after the run:

```
audit chain verifies: 185 records, /var/log/basalt-assistant/audit.jsonl
#166 basalt-assistantd decision  dnf.next [dnf -y install /root/basalt-lab-postfail-1-1.noarch.rpm] -> rollback (p=0.80, threshold 0.75): propose p-9fa660
#175 basalt-assistantd decision  disk.cause [/] -> other_data (p=0.80, threshold 0.75): review p-2f5fe7
#181 basalt            decision  disk.cause [/] -> snapshots (p=0.83, threshold 0.75): propose p-2f5fe7 (cli)
#183 basalt            confirm   confirmed p-2f5fe7 (5d3e1fc0)
#184 basalt            apply     p-2f5fe7 applied: root file system 89 % full: snapshots
#185 basalt-mcp        proposal  p-174919 from mcp: set the boolean httpd_can_network_connect on (persistent)
-----a---------------m /var/log/basalt-assistant/audit.jsonl
```

## Findings

1. Many SELinux denials are never logged: Fedora's policy has dontaudit
   rules for common mistakes (a confined service searching or writing an
   administrator's home directory). A diagnosis that waits for an AVC
   misses the most common case, a directory moved from `/root` (`mv` keeps
   the label). The label check of each path component named in a
   "Permission denied" message finds it.
2. A failing `%post` scriptlet is not a failed transaction for rpm: the
   audit record says `res=success` and dnf5 logs a WARNING. Only the dnf5
   log shows it; the daemon reads that log incrementally.
3. The libdnf5 actions hook takes the post snapshot even when the
   transaction fails, so "pre without post" only catches interrupted
   transactions (power loss, a killed dnf).
4. `NoNewPrivileges=yes` blocks the domain transition from systemd unless
   the policy allows `nnp_transition` (`init_nnp_daemon_domain`); without it
   the daemon silently runs as `init_t`.
5. Booleans on attributes are a trap: `sesearch` offers `nis_enabled` (every
   nsswitch domain may bind every unreserved port) for nginx on a free port,
   and `httpd_run_preupgrade` for port 8099. Only booleans written for the
   domain and the exact target type are proposed; otherwise the port is
   labeled.
6. A diagnosis is a snapshot of a moment: proposals for a unit that runs
   again, or a disk that is no longer full, are closed by the daemon;
   evidence is limited to the last start attempt, so an earlier, fixed
   failure of the same unit does not count.
7. The state directory is part of the root and rolls back with it; the
   audit log does not. The journal position is kept per boot so the daemon
   does not report already handled events after a rollback.
8. Restoring a config file from "the newest different snapshot copy" can
   pick another broken copy (seen once in the lab: the apply failed its
   checks and was marked failed). The candidate must have been in place at
   the unit's last successful start.
9. `btrfs subvolume delete` frees space asynchronously; the verification of
   a snapshot deletion waits with `btrfs subvolume sync`.
10. `ausearch` without `--input-logs` reads standard input when it is not a
    terminal and hangs in scripts.

## Gaps

- The confined daemon cannot measure the space each snapshot holds (btrfs
  ioctls, reading every file), compare package databases (rpm opens them
  read-write) or run config checkers (they open log files); it says so in
  the report and `basalt` as root completes the diagnosis. Quotas
  (snapper's used-space column) or a small privileged helper are options.
- `basalt-mcp` stores proposals only when run as root (the state directory
  is root-only); a socket to the daemon would let an unprivileged client
  propose.
- Notifications go to the journal only (priority warning for important
  ones); no mail, desktop or console broadcast yet.
- The rules' probabilities are hand-set; calibration needs the shared
  evaluation suite.
- The model backend is a seam only.
- The assistant is not in the default package set of the installer yet;
  CI installs it from the test repository in the boot test.

## Decisions for the owner

1. Install `basalt-assistant` by default (kickstart) and enable the daemon,
   or keep it opt-in for now.
2. Allow the daemon more read access (snapshot space via qgroups or a
   helper, config checkers in their own domains) or keep the split where
   the root CLI completes those diagnoses.
3. Retention of the audit log: today it grows forever (append-only, no
   rotation); a sealed rotation (chain carried into the next file) is the
   natural next step.
4. Who may use the MCP server: root only (today), or a group through the
   daemon.
5. Notification channel for high-severity findings.
