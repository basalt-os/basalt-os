# Milestone 2c: friendly English and the optional humanize layer

Status: done in the lab, 2026-10-04. The templates are the default and
need no model; humanize is optional and off by default. Design and
configuration: [assistant.md](assistant.md#how-it-explains) and
[local-model.md](local-model.md#humanize-the-model-writes-the-explanation).

## Goal

Make every answer of the assistant plain, friendly English (English only
for now; other languages later through translation of the templates) and
keep the safety properties: the model never decides or acts, commands are
never reworded. Then measure whether a small local model, used only to
write the explanation in its own words, is worth turning on.

## What was built

| Part | Result |
|---|---|
| Templates (default) | every finding is written from structured facts: what is wrong, why (the main evidence), what applying will do, the risk with its reason, how to undo it, the next step; commands in their own block, rebuilt from the typed actions; length follows severity (a running unit is two lines, reports have no command block, evidence capped by severity, `--verbose` for all); `status`, `disk`, `snapshots`, `pending`, the apply flow and `ask` in the same plain wording |
| Facts | proposals carry the facts their text is written from (`facts` in the proposal JSON); older proposals fall back to their stored report |
| Golden tests | 33 rendered findings (every unit cause, every SELinux class, every disk cause, package transactions, rollbacks, hints, low confidence, an incomplete diagnosis, an applied proposal, a legacy proposal) and the status, disk and snapshot listings; a test that every template paragraph passes the faithfulness check |
| Humanize | `[humanize] enabled = yes`: a model writes only the explanation paragraph from the facts as JSON; the template keeps title, commands, risk, undo, apply line, evidence; faithfulness check, fallback to the template, sentence-by-sentence streaming on a terminal, audit record per text; any OpenAI-compatible endpoint (local service, bigger local server, remote provider by opt-in with redaction and the request shown) |
| Faithfulness check | rejects any number (digits or words), path, file name, unit, SELinux type or boolean, proposal id or command word not in the facts; markup and shell characters outside quoted fact values; addresses; advice the assistant never gives; a change claimed when none is planned, a fault claimed for a healthy finding, health claimed for a failed one; a text that does not name its unit; non-Latin script; more than 600 characters |
| Training data | `basalt-eval render-data`: facts sampled from fixed pools, prose from the templates with random equivalent phrasings; training and held-out splits use disjoint unit names, files, paths and users |
| Models | LoRA (rank 16, all projections) of Qwen 3 0.6B (fp32, 2 epochs) and 1.7B (4-bit NF4 with fp32 compute, 1 epoch) on 6,000 pairs, GTX 1070; merged, saved in fp16, GGUF Q8_0 and Q4_K_M |
| Fixes found | MCP clients could read the confirmation code through `basalt_proposal` (now never in MCP text); the model client retries once when a kept-alive connection was closed by the server |

## Measurements

Held-out set: 400 findings generated from the held-out pools (no unit
name, configuration file or data path of the training set). Lab host:
Xeon E5-2690 v4, llama.cpp 0.5.0 CPU build, 4 threads pinned to 4 cores,
context 2048, temperature 0. Latency is the whole paragraph; first
sentence is when the first accepted sentence is shown (streaming).
Faithful: the model's text passed the check and was shown; otherwise the
template was shown (fallback).

| System | Prompt | Faithful (model text shown) | Fallback | p50 / p95 ms | First sentence p50 / p95 ms |
|---|---|---|---|---|---|
| Templates | none | 1.000 (by construction; tested on every golden finding and 6,000 generated ones) | 0 | < 1 / < 1 | |
| 0.6B + LoRA, Q8_0 | compact | 0.998 (399 of 400) | 0.003 | 1706 / 4227 | 840 / 1777 |
| 0.6B + LoRA, Q4_K_M | compact | 0.998 (399 of 400) | 0.003 | 1295 / 3286 | 647 / 1336 |
| 1.7B + QLoRA, Q8_0 | compact | 0.995 (398 of 400) | 0.005 | 4287 / 9953 | 2161 / 4719 |
| Qwen3 0.6B Q8_0 | full instructions | 0.390 | 0.610 | 2021 / 3409 | 1429 / 2071 |
| Qwen3 1.7B Q8_0 | full instructions | 0.420 | 0.580 | 4720 / 8473 | 3484 / 5656 |
| Qwen3 4B Q4_K_M (100 findings) | full instructions | 0.580 | 0.420 | 9190 / 13466 | 4827 / 8808 |

The fine-tuned models' misses: three model calls that failed on the
transport (a kept-alive connection closed by the server; the client now
retries once) and one text from the 0.6B Q4_K_M that said a running unit
had "no known security issue" (rejected: a fault claimed for a healthy
unit). The publisher models were rejected mostly for claiming a change
when none was planned (184, 95 and 26 texts), command words (8, 76, 3),
not naming the unit (17, 29, 7) and claiming a fault for a healthy finding
(30, 28, 6). A rejected text stops at its first bad sentence, so its
latency is shorter than a full answer. The lab host was otherwise busy
(load around 9 of 28 threads, the GPU training on the side); on the
2-vCPU lab VM the fine-tuned 0.6B Q8_0 took 6.3 to 9.1 s per finding.

Training: 0.6B in 67 minutes (fp32), 1.7B in 147 minutes (one epoch,
4-bit NF4 weights, fp32 compute), validation loss 0.021 and 0.023 on
held-back generated pairs. The models learn the generator: 36 % (0.6B)
and 50 % (1.7B) of their held-out texts are the template's default
wording word for word, and most of the rest are its other phrasings.

Quality: a blind rating of 40 held-out findings (texts shuffled per
finding, systems hidden), 1 to 5 on accuracy (says what the facts say and
nothing else), clarity (a person who is not an expert knows what is wrong
and what applying does), tone (friendly and calm, no jargon it does not
need) and concision. The rater was the agent that built the templates,
not a panel of users: the numbers rank the systems, they do not measure
users.

| System | Accuracy | Clarity | Tone | Concision | Overall |
|---|---|---|---|---|---|
| Templates | 5.00 | 4.03 | 3.80 | 4.40 | 4.31 |
| 0.6B + LoRA, Q8_0 | 5.00 | 4.03 | 3.80 | 4.40 | 4.31 |
| 1.7B + QLoRA, Q8_0 (rated after the others, not blind) | 4.97 | 4.08 | 3.80 | 4.47 | 4.33 |
| Qwen3 1.7B Q8_0 | 2.23 | 2.83 | 3.60 | 3.42 | 3.02 |
| Qwen3 4B Q4_K_M | 2.42 | 3.45 | 3.70 | 3.20 | 3.19 |

Counting only the texts the check let through, the publisher models
score 3.84 (1.7B, 16 of 40) and 3.60 (4B, 17 of 40): the check removes
their worst texts, but what passes is still below the templates. Their
best texts were warmer than the templates ("If you stop vaultwarden,
coturn.service can use port 8080"), and their typical errors were
confident and wrong: a package cache "taking up 99 % of the space", a
snapshot "taking up 86 % of the disk", labels swapped, a change promised
for a report.

## Lab VM

basalt-assistant 0.5.0 on a Basalt OS lab VM (2 vCPU, 4 GiB, SELinux
enforcing), faults caused on purpose: a broken nginx configuration, nginx
on a port SELinux does not allow, the disk filled by a 10 GiB file only a
snapshot holds, a package whose `%post` fails, the snapshot list. Before
(0.3.0) and after (0.5.0, templates) for the nginx case:

```
# basalt why nginx
[p-65e2c9] nginx.service failed: config error
  status: pending, from cli, 2026-10-04 05:38:18 UTC

What is wrong
  nginx.service does not start because of an error in /etc/nginx/nginx.conf line 36. Snapshot 82 (2026-10-03 23:48:00) has a different copy of /etc/nginx/nginx.conf (content differs, in place at the last successful start); restoring it and restarting is proposed.

Evidence
  (18 lines: state, journal, nginx -t output, the diff against snapshot 82)

Decision
  - unit.cause: config_error (p=0.99, threshold 0.75, rules/v1); config_error 0.99, crashed 0.00, dependency_failed 0.00, disk_full 0.00, missing_file 0.00, port_conflict 0.00, selinux_denial 0.00, unknown 0.00

Proposed change
  $ cp --preserve=mode,ownership,timestamps /.snapshots/82/snapshot/etc/nginx/nginx.conf /etc/nginx/nginx.conf    # restore /etc/nginx/nginx.conf from snapshot 82
  $ restorecon -v /etc/nginx/nginx.conf    # label the restored file
  $ systemctl restart nginx.service    # restart nginx.service

  Apply:  sudo basalt apply p-65e2c9      (non-interactive: sudo basalt apply p-65e2c9 --yes --confirm 0f2a0109)
  Ignore: sudo basalt ignore p-65e2c9
```

```
# basalt why nginx
[p-fc1b5a] nginx.service stopped: configuration error
Waiting for your decision, found by basalt on 2026-10-04 05:40 UTC

  nginx.service is not running because of a mistake in /etc/nginx/nginx.conf
  (line 36). The configuration check nginx -t says: unknown directive
  "bogus_directive" in /etc/nginx/nginx.conf:36. If you apply it, the
  assistant will put back the copy of /etc/nginx/nginx.conf from snapshot 88
  and then restart nginx.service.

What will run (exactly these commands, as root, in this order)
  $ cp --preserve=mode,ownership,timestamps /.snapshots/88/snapshot/etc/nginx/nginx.conf /etc/nginx/nginx.conf
  $ restorecon -v /etc/nginx/nginx.conf
  $ systemctl restart nginx.service

Risk: medium (replaces the current /etc/nginx/nginx.conf).
Undo: a snapshot is taken before and after, so you can go back to the state
before the change; a rollback takes effect at the next boot.
  $ sudo basalt snapshots rollback --before p-fc1b5a

Next step
  Apply it:   sudo basalt apply p-fc1b5a
              (without a prompt: sudo basalt apply p-fc1b5a --yes --confirm 39bb8261)
  Ignore it:  sudo basalt ignore p-fc1b5a

Evidence
  (19 lines: state, SELinux domain, journal, nginx -t output, the diff against snapshot 88)
```

With humanize on (the fine-tuned 0.6B Q8_0 served by `basalt-llm`,
confined, no network) the four findings were written by the model and all
four passed the check; each took 6.3 to 9.1 s on the 2 vCPUs (the
template is instant):

```
[p-31db7a] root file system 94 % full: snapshots hold the space
Waiting for your decision, found by basalt on 2026-10-04 06:42 UTC, seen 2 times

  The root file system is getting full: 94 % used. Snapshot 95 (2026-10-04
  06:42:01, "demo-holds-10G") alone holds 10.0 GiB. The total size of all
  snapshots is 10.1 GiB. Applying it will delete snapshot 95, which frees the
  space only it holds.

What will run (exactly these commands, as root, in this order)
  $ snapper -c root delete 95

Risk: high (snapshot 95 is gone for good and can no longer be rolled back to).
Undo: deleting snapshot 95 cannot be undone.

Next step
  Apply it:   sudo basalt apply p-31db7a
              (without a prompt: sudo basalt apply p-31db7a --yes --confirm c2cf1f22)
  Ignore it:  sudo basalt ignore p-31db7a
```

## Recommendation

- Keep the templates as the default and humanize off by default. The
  rewritten templates carry the friendliness this milestone asked for,
  cost nothing, and are exact by construction.
- The fine-tuned humanize models are safe (99.5 to 99.8 % of their texts
  pass the check, the rest fall back) but add nothing a person would
  notice: they reproduce the templates' phrasings, at 1.3 to 4.3 s per
  finding on 4 cores and about 1 GiB of memory. Not worth shipping as
  they are.
- Untuned small models are not usable for this: 39 to 58 % of their texts
  pass, and the ones that pass rate below the templates.
- Keep the humanize layer as an opt-in for people who plug a bigger or
  remote model: the fixed slots, the check and the fallback make that
  safe, and the redaction and the printed request make it transparent.
- To make the model worth it, train on explanations written by people
  for each kind of finding (recorded provenance), then measure again with
  the same check and a rating by people, not by the author.

## Gaps

- The fine-tuned models learn the templates' own phrasings: they are as
  faithful as the templates, not friendlier. Friendlier prose needs
  targets written for it (by people, with recorded provenance), then the
  same check.
- The check is lexical: it cannot tell a wrong sentence that uses only
  values from the facts (the shape checks catch the common ones: a change
  claimed for a report, a fault for a healthy unit).
- The local model service serves one model: translator and humanize
  cannot share it yet (llama-server's per-request LoRA adapters on one
  base model would allow it).
- The quality rating was done by the agent, not by people.
- The humanize weights are not published (as for the translators, they
  wait for the signed release).
