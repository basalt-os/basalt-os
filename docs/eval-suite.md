# The shared evaluation suite

Status: milestone 2b. Files: `eval/`.

The same labeled cases measure every way the assistant can answer: the
rules of the decision layer, a local model behind it, a model a person
plugs in later, and a future specialist model trained for Basalt OS. A
case records what the diagnosers saw, what the right answer is and where
the case comes from. Nothing in a case is secret or specific to one
machine beyond lab names (`basalt-lab-*` units, lab paths).

```
eval/
  translator.jsonl        natural language requests and the intent they mean (pt-BR, en, mixed)
  translator-holdout.jsonl  a second set, written apart from the training generator (see below)
  cases/lab.jsonl         decision cases recorded in the lab
  cases/generated.jsonl   decision cases generated from templates
  tools/capture-to-cases.py   lab captures -> cases
  tools/generate-cases.py     templates -> cases (through `basalt-eval derive`)
  tools/generate-translator-train.py   templates -> translator training pairs (fine-tuning)
  tools/train-translator-lora.py       LoRA fine-tuning of the translator on those pairs
  tools/overlap.py            similarity between training pairs and a test set
  results/2026-10-03/         summaries of the milestone 2b runs (per model; training settings)
```

`packages/basalt-assistant/tools/basalt-eval` runs them:

```sh
cd packages/basalt-assistant
go run ./tools/basalt-eval check -cases ../../eval/cases                  # validate every case
go run ./tools/basalt-eval decide -cases ../../eval/cases                 # rules only
go run ./tools/basalt-eval decide -cases ../../eval/cases -endpoint unix:/run/basalt-llm/llm.sock
go run ./tools/basalt-eval translate -set ../../eval/translator.jsonl -endpoint unix:/run/basalt-llm/llm.sock
```

`decide` prints accuracy, expected calibration error (ECE, 10 bins on the
top probability), Brier score and negative log-likelihood, overall, per
question and per source, and for a model also its latency and a
per-question temperature fitted with 5-fold cross-validation (the numbers
after the fit are measured on held-out folds; the temperatures fitted on
all cases are what goes into `[calibration]` of `assistant.conf`).
`translate` prints intent and argument accuracy (strict, and lenient where
a case lists other acceptable answers), changes the person did not ask
for, latency and tokens per second, for the model's own answer and after
the deterministic checks the assistant applies.

## Decision case format: `basalt-case/v1.1`

One JSON object per line. v1.1 adds `evidence.snapshots` to v1 and is
backward compatible: a v1 reader ignores the new evidence key, and
`basalt-eval` reads both versions (v1 cases need no snapshot evidence).

| Field | Meaning |
|---|---|
| `schema` | `basalt-case/v1.1` (`basalt-case/v1` still accepted) |
| `id` | unique, stable; prefixes `lab-`, `lab-daemon-`, `gen-` |
| `kind` | `unit_failure`, `selinux_denial`, `package_transaction`, `disk_pressure` |
| `subject` | the unit, denial key, transaction or mount it is about |
| `goal` | the structured goal a command or the translator emits for it, e.g. `why(unit=nginx.service)` |
| `evidence` | what was observed: `journal` lines, unit `state`, `config_check`, `avcs` (raw records), `ports`, `deps`, `dnf_log`, `usage`, `snapshots` (v1.1, below) |
| `questions` | the decision-layer questions exactly as the diagnosers ask them: `id`, `subject`, `features` (true ones), `facts` (what a model sees besides the features), `want` (the expected answer) |
| `expected` | `diagnosis` (one sentence), `cause`, `actions` (typed actions of the closed set in [assistant.md](assistant.md), possibly empty), `proposal` (`propose` or `review`) |
| `provenance` | `source` (`lab`, `lab-daemon`, `generated`), `ref` (scenario or generator version), `recorded`, `method`, `labels`, `notes` |
| `license` | Apache-2.0 for every case here |

### Snapshot evidence (v1.1)

`evidence.snapshots` lists the snapshots the diagnosers saw, one object
each:

| Field | Meaning |
|---|---|
| `number` | snapper snapshot number (required, positive) |
| `role` | `transaction_pre` (taken just before a dnf transaction or an apply), `transaction_post` (just after it), `restore_source` (holds another copy of a file the unit needs), `space_holder` (holds space exclusively) |
| `type` | snapper type: `pre`, `post`, `single` (optional) |
| `pre_number` | for a `post` snapshot: its `pre` snapshot |
| `date`, `description` | as snapper lists them (optional) |
| `path`, `how` | `restore_source`: the file it holds (required) and how its copy differs from the current one |
| `exclusive_bytes` | `space_holder`: space only it holds (optional) |

Snapshot numbers are bindings, not labels: a scenario labels "restore
this file from a snapshot" or "roll back to the pre snapshot", and the
number comes from the snapshot the diagnosers found. Every expected
action that names a snapshot must find it here with the role it needs:
`file.restore` a `restore_source` holding the same path,
`snapshot.rollback` a `transaction_pre`, `snapshot.delete` a
`space_holder`. File diffs are never recorded (a restore candidate can be
`/etc/shadow`).

### Validation

`basalt-eval check` (and the Go test `TestAllCasesValidate`, which
`make assistant-test` runs with `eval/cases` mounted in the container)
checks every case file: a known schema, unique ids, `proposal` of
`propose` or `review`, every expected action passes the assistant's own
validator (`internal/action`, the same code that refuses a proposal),
well formed snapshot evidence and, for v1.1, the snapshot bindings above.

Example (shortened):

```json
{"schema":"basalt-case/v1.1","id":"lab-nginx-port-8085","kind":"unit_failure","subject":"nginx.service",
 "goal":"why(unit=nginx.service)",
 "evidence":{"journal":["19:00:11 nginx: [emerg] bind() to 0.0.0.0:8085 failed (13: Permission denied)", "..."],
             "avcs":[{"raw":"AVC avc:  denied  { name_bind } ... tcontext=system_u:object_r:unreserved_port_t:s0 tclass=tcp_socket","count":1}]},
 "questions":[{"id":"unit.cause","subject":"nginx.service",
               "features":{"avc_for_domain":true,"config_check_passed":true,"journal_permission_denied":true,"unit_failed":true},
               "facts":{"journal":["..."]},"want":"selinux_denial"},
              {"id":"avc.class","subject":"httpd_t|unreserved_port_t|tcp_socket|name_bind|8085",
               "features":{"port_case":true,"port_type_found":true},"want":"port"}],
 "expected":{"diagnosis":"nginx may not bind tcp 8085 (unreserved_port_t)","cause":"selinux_denial",
             "actions":[{"kind":"selinux.port","params":{"type":"http_port_t","proto":"tcp","port":"8085","mode":"add"}},
                        {"kind":"unit.restart","params":{"unit":"nginx.service"}}],"proposal":"propose"},
 "provenance":{"source":"lab","ref":"scripts/lab/eval-capture.sh nginx_port_8085","recorded":"2026-10-03",
               "method":"failure caused on purpose on a lab VM ...","labels":"by construction"},
 "license":"Apache-2.0"}
```

### Where the labels come from

- `lab`: `scripts/lab/eval-capture.sh` breaks something on purpose on a
  lab VM (a bad directive, a port held by another process, a directory
  moved from `/root`, a program that segfaults, a full file system), so
  the cause is known by construction; `basalt why --json` run as root
  records what the diagnosers saw. `tools/capture-to-cases.py` keeps the
  features, facts, evidence and the expected answer of the scenario, never
  the rules' output. It binds the snapshot number of an expected
  `file.restore` from the restore candidate the capture found.
- `lab-daemon`: the proposals the confined daemon stored for the same
  events. It runs fewer probes (no config checkers, no port owners), so
  these cases test the same failures with less evidence.
- `generated`: `tools/generate-cases.py` writes cases from templates of
  the same events (journal messages in the shape the programs print
  them, the structural findings the diagnosers set), with noise on
  purpose: probes a confined daemon cannot run, failures whose real cause
  has no option of its own (an OOM kill, a file-mode permission error), two
  large space holders. Journal features and facts are filled in by
  `basalt-eval derive`, the same code `basalt why` uses. Deterministic for
  a seed. `generate-cases/2` fixed an operator precedence error of `/1`
  that made the second large holder of a disk case about 1 PiB (cases
  `gen-disk-017-journal` and `gen-disk-018-package_cache`, whose facts
  contradicted their labels).

The generated cases reflect how the authors understand these failures;
the lab cases are the ground truth. Results are reported per source.

### Use for a specialist model

Each case is also a training or test example for a model that maps
evidence to a diagnosis and an action (a future "Basalt OS" domain of a
specialist model): the input is `goal` plus `evidence` (or the
`questions` with their features and facts), the target is `expected`
(`cause`, `diagnosis`, `actions`), and `provenance` keeps every example
traceable to a lab run or a generator version. Lab and generated cases
must stay in separate splits.

## Translator test set: `eval/translator.jsonl`

| Field | Meaning |
|---|---|
| `id` | `st-` status, `wh-` why, `se-` fix selinux, `sn-` snapshots, `dk-` disk, `pe-` pending, `sh-` show, `ap-` apply, `cl-` clarify, `no-` none |
| `text` | the request, as a person would type it |
| `lang` | `en`, `pt`, `mix` |
| `tags` | `direct`, `paraphrase`, `typo`, `colloquial`, `terse`, `args`, `ambiguous`, `out-of-scope`, `unsafe`, `injection`, `change`, `no-id`, `multi-request` and others |
| `want` | the intent: `{"intent":"why","unit":"nginx.service"}` |
| `accept` | other answers that are also acceptable (lenient score) |

204 requests. Units are compared with `.service` added when no suffix is
given; `3d` equals `72h`. The examples in the translator's prompt are
different from every request here.

### Held-out set: `eval/translator-holdout.jsonl`

The training generator (`tools/generate-translator-train.py`) was written
after `translator.jsonl`, and its templates follow the same phrasing
families: after masking ids and numbers, 13 of the 204 requests equal a
training request and 68 have a training request with similarity of 0.8 or
more (`tools/overlap.py`). A fine-tuned model scored on that set is
partly scored on its own training distribution.

`translator-holdout.jsonl` (162 requests, same format) was written
separately to measure generalization: chat style, Brazilian Portuguese
slang and abbreviations, typos, longer sentences with context, and 25 of
its 35 unit names never appear in the training data. Against the same
training pairs: 1 exact match after masking (`restaura`), 9 requests with
similarity of 0.8 or more, mean best similarity 0.52 (0.70 for the first
set). Report both sets for any fine-tuned model.
