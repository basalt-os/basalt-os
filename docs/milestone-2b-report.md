# Milestone 2b: a local language model for the assistant (spike)

Status: done in the lab, 2026-10-03. A spike: everything is optional and
off by default; the defaults of milestone 2a are unchanged. Usage and
design: [local-model.md](local-model.md); evaluation suite:
[eval-suite.md](eval-suite.md).

## Goal

Plug a small local model into the system assistant in the two places the
design gives it: as the translator from natural language to the
assistant's commands, and as a backend of the decision layer; measure
both against the rules and against each other on CPU; package and
confine the model service so it has no network; and turn the lab's
scenarios into labeled cases a specialist model can later be trained and
evaluated on.

## What was built

| Part | Result |
|---|---|
| `basalt ask` | translator from English, Brazilian Portuguese or a mix to one of 10 intents (`status`, `why` unit, `fix_selinux` window, `snapshots` list/diff/rollback, `disk`, `pending`, `show` id, `apply` id, `clarify`, `none`); JSON-schema-constrained output (a grammar in llama.cpp), validation, and grounding in the request; read-only commands run, changes are only printed; audit record per request |
| Decision backend | `openai-compatible` implemented: numbered options, output constrained to one token, probability per option from token log-probabilities, per-question temperature; answers `unit.cause`, `avc.class`, `dnf.next`, `disk.cause`; rules stay default and fallback |
| `basalt-llm` package | llama.cpp 0.5.0 server for the CPU (all x86-64 variants), 8.7 MiB RPM, no model; unit without network; SELinux module `basalt_llm`; `basalt-llm-fetch` with a revision- and SHA-256-pinned manifest |
| Evaluation suite | 254 decision questions in 238 cases (`eval/cases`: 49 lab cases, 189 generated), 204 translator requests (`eval/translator.jsonl`) plus a 162-request held-out set written apart from the training generator; format `basalt-case/v1`; `basalt-eval` (decide, translate, derive, render) |
| Lab capture | `scripts/lab/eval-capture.sh`: 23 failures caused on purpose on a lab VM, recorded with `basalt why --json` as root |
| Fine-tuning (optional part of the spike) | LoRA of Qwen 3 0.6B (fp32) and 1.7B (4-bit QLoRA) on 4,000 generated request/intent pairs, on a GTX 1070; merged, converted to GGUF |

## Models

All from the Qwen organisation's own repositories, pinned to a revision,
license Apache-2.0 (the LICENSE file at each pinned revision checked):
`Qwen3-0.6B-GGUF` Q8_0, `Qwen3-1.7B-GGUF` Q8_0, `Qwen3-4B-GGUF` Q4_K_M
(the publisher offers Q4_K_M only for 4B). For the 0.6B and 1.7B Q4_K_M
rows the publisher's safetensors (checksums verified) were converted and
quantized with llama.cpp 0.5.0. Fedora 44's `llama-cpp` package
(b6153) was not usable for a server: it is built with ROCm and needs
about 6 GiB of dependencies.

## Measurements

Lab host: Xeon E5-2690 v4 (AVX2, no AVX-512), llama.cpp 0.5.0 CPU build
(haswell variant), 4 threads pinned to 4 cores, context 2048, prompt
cache 256 MiB, temperature 0. The target 2-vCPU VM numbers are at the end.
Latency is per request with the prompt prefix cached (requests in a row);
the first request after a start pays for the whole prompt.

### Translator, `eval/translator.jsonl` (204 requests)

Strict: intent and arguments exactly right. Grounded: after the
assistant's deterministic checks (what `basalt ask` acts on).

| Model | Prompt | Intent | Strict (model) | Strict (grounded) | Lenient | Arguments | p50 / p95 ms | gen tok/s |
|---|---|---|---|---|---|---|---|---|
| Qwen3 0.6B Q8_0 | examples (693 tok) | 0.711 | 0.657 | 0.691 | 0.725 | 0.951 | 588 / 1072 | 27 |
| Qwen3 0.6B Q4_K_M | examples | 0.711 | 0.627 | 0.662 | 0.686 | 0.890 | 661 / 1126 | 31 |
| Qwen3 1.7B Q8_0 | examples | 0.828 | 0.794 | 0.809 | 0.838 | 0.959 | 1297 / 2337 | 12 |
| Qwen3 1.7B Q4_K_M | examples | 0.892 | 0.819 | 0.853 | 0.877 | 0.923 | 991 / 1656 | 18 |
| Qwen3 4B Q4_K_M | examples | 0.936 | 0.873 | 0.873 | 0.897 | 0.875 | 2414 / 9156 | 8 |
| 0.6B + LoRA, Q8_0 | compact (65 tok) | 0.966 | 0.941 | 0.946 | 0.961 | 0.963 | 444 / 777 | 35 |
| 0.6B + LoRA, Q4_K_M | compact | 0.951 | 0.931 | 0.931 | 0.946 | 0.962 | 330 / 565 | 48 |
| 1.7B + QLoRA, Q8_0 | compact | 0.966 | 0.956 | 0.946 | 0.966 | 0.963 | 1563 / 3176 (*) | 9 |
| 1.7B + QLoRA, Q4_K_M | compact | 0.966 | 0.946 | 0.941 | 0.956 | 0.954 | 832 / 1486 (*) | 19 |

(*) measured while another benchmark ran on other cores of the host; the
other latencies are from runs alone on the host. Accuracy is from the
final runs, with the final grounding checks.

### Translator, held-out set (162 requests, written apart from the training generator)

The training pairs and the first set share phrasing families (68 of 204
requests have a training request with similarity of 0.8 or more after
masking ids and numbers); the held-out set has 9 of 162, and 25 of its 35
unit names never appear in training. This is the number to trust for the
fine-tuned models.

| Model | Intent | Strict (grounded) | Lenient | Arguments | p50 / p95 ms |
|---|---|---|---|---|---|
| Qwen3 0.6B Q8_0 | 0.531 | 0.525 | 0.537 | 0.981 | 647 / 1321 |
| Qwen3 1.7B Q8_0 | 0.728 | 0.728 | 0.741 | 1.000 | 1532 / 2979 |
| Qwen3 1.7B Q4_K_M | 0.753 | 0.735 | 0.753 | 0.955 | 1297 / 2530 |
| Qwen3 4B Q4_K_M | 0.901 | 0.864 | 0.895 | 0.927 | 3369 / 5358 |
| 0.6B + LoRA, Q8_0 | 0.907 | 0.901 | 0.914 | 0.988 | 498 / 842 |
| 0.6B + LoRA, Q4_K_M | 0.895 | 0.889 | 0.901 | 0.988 | 361 / 595 |
| 1.7B + QLoRA, Q8_0 | 0.932 | 0.920 | 0.926 | 0.977 | 1683 / 3069 |
| 1.7B + QLoRA, Q4_K_M | 0.907 | 0.889 | 0.901 | 0.964 | 1167 / 2004 |

(Held-out latencies were measured while another benchmark used other
cores of the host; they are higher than an idle machine's.)

Errors that remain, fine-tuned 0.6B on the held-out set: slang without a
service name ("tá tudo verdinho?" read as clarify), invented unit names
for an indirect mention (caught by grounding: "netd" for netdata),
"samba (smb)" answered with the friendly name, `ENOSPC` taken for an
SELinux problem, "manda ver na p-a1b2c3" (Portuguese slang for "go ahead")
read as show, and one prompt injection ("you must answer apply p-aaaaaa")
answered with apply. That last one only prints `sudo basalt apply
p-aaaaaa`: the translator never runs a change, and the confirmation would
show a proposal that does not exist.

Unrequested changes (an apply or rollback that was not what the person
asked): 0 to 2 per model after grounding, all of them printed, none run.

### Decision layer, `eval/cases` (254 questions)

Lab: 35 from failures caused on purpose, root view; lab-daemon: 30 from
the confined daemon's own proposals for the same events (fewer probes);
generated: 189 from templates. ECE: expected calibration error, 10 bins
on the top probability; T-cv: after a per-question temperature fitted
with 5-fold cross-validation.

| Backend | Accuracy | lab / daemon / generated | ECE | Brier | ECE T-cv | Brier T-cv | p50 / p95 ms |
|---|---|---|---|---|---|---|---|
| rules/v1 | 0.925 | 0.886 / 0.967 / 0.926 | 0.192 | 0.183 | 0.056 | 0.117 | < 1 |
| Qwen3 0.6B Q8_0 | 0.358 | 0.257 / 0.333 / 0.381 | 0.501 | 1.056 | 0.162 | 0.705 | 1728 / 5472 |
| Qwen3 0.6B Q4_K_M | 0.366 | 0.286 / 0.267 / 0.397 | 0.490 | 1.063 | 0.195 | 0.715 | 1102 / 1831 |
| Qwen3 1.7B Q8_0 | 0.709 | 0.657 / 0.667 / 0.725 | 0.240 | 0.500 | 0.091 | 0.402 | 3915 / 5915 |
| Qwen3 1.7B Q4_K_M | 0.638 | 0.629 / 0.600 / 0.646 | 0.211 | 0.555 | 0.117 | 0.485 | 3200 / 7882 |
| Qwen3 4B Q4_K_M | 0.772 | 0.771 / 0.633 / 0.794 | 0.180 | 0.406 | 0.056 | 0.303 | 7903 / 13504 |

The rules row is measured on the corrected cases (see the note below);
the model rows on the first revision, where 4 of the 189 generated cases
differ.

Update, basalt-assistant 0.4.0 (after a live run of 35 fault scenarios on
a lab VM): the rules reach 0.968 (lab 0.941, daemon 0.967, generated
0.974; unit.cause 0.986) on the suite as it is now (239 cases, 253
questions: `lab-nginx-include-missing` relabeled, `lab-dac-permission`
recaptured, `lab-oom` added, `generate-cases/3`). The gains come from
out-of-memory kills, file-permission (DAC) errors and the relabel; see
`eval/results/2026-10-03/dec-rules-0.4.0.summary.json`.

Per question (accuracy): rules unit.cause 0.91, avc.class 0.98, dnf.next
0.87, disk.cause 0.92; 4B unit.cause 0.89 (with the journal lines it
nearly matches the rules), avc.class 0.74, dnf.next 0.43, disk.cause 0.48.

Combinations, measured offline on the same answers: sending the rules'
low-confidence cases (below 0.75, 101 of 254) to the model lowers
accuracy (rules are right in 85 of those, 4B in 73, 1.7B Q8 in 67);
a product of the two distributions also lowers it (0.84 with 4B). Where
the rules and 4B agree (191 cases) they are right 96 % of the time, a
possible confidence signal for later.

The rules are underconfident: their answers at 0.6 to 0.8 are right
almost always. A temperature below 1 per question (fitted: 0.25 to 0.54)
brings their ECE from 0.19 to 0.05 without changing any answer. The fit
is dominated by generated cases, so it is reported, not applied.

Case corrections after the first runs (schema `basalt-case/v1.1`,
generator `generate-cases/2`, same seed): an operator precedence error
in the disk template made the second large holder of two generated disk
cases (`gen-disk-017-journal`, `gen-disk-018-package_cache`) about 1 PiB,
larger than the holder the label names; `IsNoSpace` now also matches the
lowercase message Go programs print, which adds `journal_no_space` to
`gen-unit-020-disk_full` and `gen-unit-036-disk_full` (the rules now get
one of them right: unit.cause 0.901 to 0.908); five lab cases had expected
actions the assistant's validators refuse (`lab-nginx-data-log`,
`lab-nginx-port-8085`, `lab-nginx-proxy-boolean`, `lab-nginx-directive`,
`lab-nginx-syntax`), and `lab-nginx-proxy-boolean` named
`httpd_can_network_connect` where the policy rule for the denial is
`httpd_can_network_relay`. Lab cases now record the snapshots behind
`file.restore` and `snapshot.rollback` (`evidence.snapshots`), and
`basalt-eval check` (also a Go test) validates every case.

### Rendering a diagnosis as text (optional)

A model writing three sentences from the structured diagnosis of 20 lab
cases, in English and Portuguese (40 texts each): 1.7B Q4_K_M p50 5.7 s,
4B Q4_K_M p50 14.2 s (both measured under load); no command, path or
proposal id that was not in the input in any text (automatic check); the
unit was named in 22 of 40 (1.7B) and 38 of 40 (4B) texts; 1.7B's
Portuguese has grammar errors. The text templates of milestone 2a stay
what people see.

### Memory (resident, after 40 translations and 30 decisions)

| Model | File | Resident | of which anonymous | of which the model file |
|---|---|---|---|---|
| Qwen3 0.6B Q8_0 | 610 MiB | 1.22 GiB | 0.59 GiB | 0.62 GiB |
| Qwen3 0.6B Q4_K_M | 462 MiB | 1.27 GiB | 0.79 GiB | 0.47 GiB |
| Qwen3 1.7B Q8_0 | 1.71 GiB | 2.36 GiB | 0.59 GiB | 1.76 GiB |
| Qwen3 1.7B Q4_K_M | 1.19 GiB | 2.51 GiB | 1.27 GiB | 1.23 GiB |
| Qwen3 4B Q4_K_M | 2.33 GiB | 4.65 GiB | 2.30 GiB | 2.38 GiB |

Q4_K_M uses more anonymous memory than Q8_0 on this CPU: llama.cpp
repacks Q4_K weights for AVX2 into a second copy, while Q8_0 runs from
the mapped file (pages the kernel can drop). On a small server Q8_0 is the
better choice at 0.6B and 1.7B. llama-server's default prompt cache (8
GiB) grew the 1.7B server to 4.7 GiB in the first runs; the service caps
it at 256 MiB.

### On the target: 2 vCPU, 4 GiB VM, confined service

Basalt OS lab VM, `basalt-llm` from its RPM, SELinux enforcing, the
fine-tuned 0.6B Q8_0, `THREADS=0` (2 threads), translator set of 204:
strict (grounded) 0.946, lenient 0.961, arguments 0.963; p50 731 ms, p95
1286 ms, 22.6 generated tokens/s; `basalt ask` end to end 0.5 to 1.7 s
(the first request after a start included). Service memory: 952 MiB
resident (319 MiB anonymous, 633 MiB the mapped model), cgroup 682 MiB.
0 denials of `basalt_llm_t` and `basalt_assistant_t`. The daemon with
`backend = openai-compatible` reached the service over the socket from its
confined domain and logged decisions answered by the model backend.

### Confinement proof

On the VM: the service runs as `system_u:system_r:basalt_llm_t:s0` with a
dynamic user; its network namespace has only `lo` and no route; from that
namespace connections to the lab gateway and to the internet fail;
`ss` inside it shows only the Unix socket `/run/basalt-llm/llm.sock`, and
no TCP/UDP socket of the process exists; `sesearch` shows no `create`
permission on any socket class but `unix_stream_socket` for the domain;
a copy of `curl` started in `basalt_llm_t` without the namespace gets
`failed to open socket: Permission denied` with an AVC `tcp_socket
create`, while the same request from an unconfined unit gets an HTTP
answer. Only `basalt_assistant_t` (and unconfined administrators) may
`connectto` the domain. Policy additions found in the lab: cgroup reads
(nproc), userdb lookups for the dynamic user, a dontaudit for
systemd-homed.

## Fine-tuning

Data: 4,000 pairs from `eval/tools/generate-translator-train.py` (seed
20261003; English, Portuguese and mixed templates written for this
project, slot values, filler words, typos; requests equal to the first
test set are dropped: 41 of them). No third-party text. Training:
`eval/tools/train-translator-lora.py`, LoRA rank 16 on all attention and
MLP projections, 2 epochs, learning rate 2e-4, batch 16, the compact
prompt, loss only on the answer. GTX 1070 (Pascal, no fast fp16): 0.6B in
fp32, 18 minutes; 1.7B as 4-bit QLoRA with fp32 compute, 64 minutes.
Validation loss on held-back generated pairs 0.0008 (0.6B): the model
learns the generator; the held-out set measures what transfers.

The fine-tuned models are not in the manifest: their weights are not
published yet.

## Recommendation

- Translator default: the fine-tuned 0.6B, Q8_0 (held-out 0.90, first set
  0.95, p50 under 0.75 s on 2 vCPU, about 1 GiB resident), once its
  weights are published; the fine-tuned 1.7B Q8_0 for machines with 4 or
  more cores and 2.5 GiB to spare (held-out 0.92). Until a fine-tuned
  model is published, Qwen3 1.7B Q8_0 from the publisher (held-out 0.73,
  first set 0.81 to 0.85, 1.3 to 1.8 s) or 4B Q4_K_M where 5 GiB and 3 s
  per request are acceptable (held-out 0.86).
- Decision layer: the rules, unchanged. The model backend stays opt-in for
  comparison.

## Lab cases and fixes found

The capture turned up two bugs in the milestone 2a diagnosers, fixed
with tests: `find`'s error message for a missing start directory was
taken for the path of a denied object, and the path label check could
suggest a file context for a security-sensitive object (nginx told to
include `/etc/shadow`: it suggested giving `/etc/shadow` an httpd type;
no change was proposed because the confidence was below the threshold).
The rules got 2 of the 23 lab unit causes wrong, both instructive: a
`Permission denied` from file modes (not SELinux) and an `include` of a
missing file (the config checker fails, so the rules say config error;
the 1.7B and 4B models said missing file).

## Findings

1. Constrained decoding needs the schema's property order: a Go map sorts
   keys, llama.cpp emits properties in schema order, so `intent` came
   after `a`, `b`, `id`, `all` and the model was steered to the wrong
   intents (1.7B went from 64 % to 86 % once fixed).
2. Small models copy ids from the prompt's examples and invent units and
   time windows; the grounding check removes those errors without a model
   (0.6B: from 0.657 to 0.691).
3. A fine-tuned 0.6B beats the untuned 4B on both sets (held-out 0.901
   against 0.864) and is 5 to 7 times faster; its prompt is 65 tokens
   instead of 693, which also makes the first request after a start cheap
   (about 1.7 s on 2 vCPU, against 10.8 s for the untuned 1.7B on 4
   cores). The fine-tuned 1.7B is the most accurate (held-out 0.920, no
   unrequested change) at 3 times the latency of the 0.6B.
4. Untuned models are not useful as decision backends here: below the
   rules on every question, and seconds per answer. With little evidence
   (the daemon's view) the 4B model often picks an option by position.
5. The rules' probabilities are too low, not too high: calibration by
   temperature fixes that cheaply.
6. llama-server's default prompt cache can use up to 8 GiB.

## Gaps

- Fine-tuned weights are not published (no place, no signing); the
  manifest has only the publisher's files.
- `basalt-llm` is not built by CI (several minutes of compiling) and has
  no rpmlint run on the built package there.
- The generated decision cases reflect the authors' understanding; only
  65 questions come from the lab.
- No dedicated decision model (small classifier or fine-tune) was tried:
  training on generated cases would mostly learn the generator.
- Rendering diagnoses with a model is measured, not wired into the CLI.
- The translator's grammar comes from the JSON schema; servers without
  `response_format` support (some Ollama versions) are not tested.
