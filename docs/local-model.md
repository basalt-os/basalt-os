# The optional local language model

Status: pre-alpha, milestone 2b, off by default. Measurements and the
reasoning behind the defaults: [milestone-2b-report.md](milestone-2b-report.md).

The system assistant ([assistant.md](assistant.md)) works without any
language model. A small model, running on the machine's CPU with no
network access, can be added for two things:

1. `basalt ask "..."`: a request in natural language (English, Brazilian
   Portuguese, or a mix) is translated into one `basalt` command;
2. a model backend for the decision layer (`[decision] backend =
   openai-compatible`): the model answers the bounded questions the rules
   answer today, with a probability per option.

Neither gives the model any power. It returns text constrained by a schema,
the assistant validates and checks it, and every change still goes
through `basalt apply` with its preview, confirmation, snapshots and audit
record.

## Pieces

| Piece | What it is |
|---|---|
| `basalt-llm` (package, optional) | llama.cpp's `llama-server` 0.5.0 built for the CPU only (all x86-64 variants, the best one is picked at run time), in `/usr/lib64/basalt-llm`; `basalt-llm.service`; `basalt-llm-fetch`; `/etc/basalt/llm.conf`. About 9 MiB as an RPM, no model inside. |
| `basalt-llm-selinux` | SELinux module `basalt_llm`: domain `basalt_llm_t` |
| models | GGUF files in `/var/lib/basalt-llm/models`, downloaded by an administrator with `basalt-llm-fetch` and checked against the SHA-256 in `/usr/share/basalt-llm/models.manifest` |
| model selection | `/usr/libexec/basalt-llm/basalt-llm-select`, used by the service at each start (`MODEL=auto`) and by `basalt-llm-fetch auto` |
| `basalt ask` | part of `basalt-assistant` (`internal/translate`) |
| model backend | part of `basalt-assistant` (`internal/decide/model.go`) |

Fedora's own `llama-cpp` package was not used: it is built with ROCm and
pulls in about 6 GiB of GPU libraries, against a light server image.

## Install and enable

```sh
sudo dnf install basalt-llm               # also basalt-llm-selinux
basalt-llm-fetch --list                   # also says what MODEL=auto picks here
sudo basalt-llm-fetch auto                # downloads it, verifies the SHA-256
sudo systemctl enable --now basalt-llm
sudoedit /etc/basalt/assistant.conf       # [translator] enabled = yes
basalt ask "por que o nginx caiu?"
```

Until the fine-tuned translators are published (below), `basalt-llm-fetch
auto` stops with a message that says so; use a published model instead:

```sh
sudo basalt-llm-fetch qwen3-1.7b-q8_0
sudoedit /etc/basalt/llm.conf             # MODEL=qwen3-1.7b-q8_0
sudo systemctl enable --now basalt-llm
```

## Which model runs

`MODEL` in `/etc/basalt/llm.conf`:

| Value | Model |
|---|---|
| `auto` (default; also an empty value) | the fine-tuned translator that fits this machine, see below |
| `0.6b` | the fine-tuned 0.6B translator, `basalt-translator-0.6b-q8_0` |
| `1.7b` | the fine-tuned 1.7B translator, `basalt-translator-1.7b-q8_0` |
| a manifest name | that model, e.g. `qwen3-1.7b-q8_0` |
| an absolute path | that GGUF file, used as is, without a checksum |

`auto` selects the fine-tuned 1.7B (Q8_0) when all three hold, else the
fine-tuned 0.6B (Q8_0):

| Condition | Threshold | Why |
|---|---|---|
| CPU cores | 4 or more | physical cores, at most the CPUs the service may use; the 1.7B is about 3 times slower than the 0.6B per request ([report](milestone-2b-report.md)) |
| available memory (`MemAvailable`) | 3584 MiB or more | the 1.7B Q8_0 server peaked at 2.36 GiB resident in the measurements (1.76 GiB mapped model, 0.6 GiB buffers and caches); about 1 GiB more stays for the rest of the system |
| the service's memory limit (`MemoryHigh`, `MemoryMax`) | 3072 MiB or more | about 0.6 GiB above that peak; the shipped unit (`MemoryHigh=3G`, `MemoryMax=4G`) qualifies, a drop-in with less selects the 0.6B |

The choice is made each time the service starts and logged with the
numbers it used (`journalctl -u basalt-llm`), for example:

```
basalt-llm: model basalt-translator-1.7b-q8_0 (auto: 8 CPU cores, 12034 MiB available, memory limit 3072 MiB; 1.7b needs 4 cores, 3584 MiB available and a limit of 3072 MiB)
```

A running server never switches models: a machine that gains or loses
memory gets a different model at the next start, never in the middle of
a run. If `auto` allows the 1.7B but only the 0.6B is downloaded, the
0.6B runs (the log says so); the 1.7B is never used where `auto` chose
the 0.6B unless `MODEL=1.7b` asks for it. `basalt-llm-fetch auto` downloads
the model `auto` selects on the machine it runs on.

The assistant picks its prompt to match: with `[translator] prompt = auto`
(the default) it asks the service which model it serves and uses the
compact prompt for a fine-tuned translator (`basalt-translator-*`) and the
prompt with examples for any other model.

## Models

The publisher models in the manifest are the Qwen organisation's own GGUF
files, each pinned to a revision and checked by SHA-256; all three are
Apache-2.0. A file given by absolute path (`MODEL=/path/x.gguf`) is used
as is, without a checksum: only for models you built or trust. It must be
readable by the service: its SELinux domain reads only files labeled as
models, so keep it in `/var/lib/basalt-llm/models`.

| Model | Download | Notes |
|---|---|---|
| `basalt-translator-0.6b-q8_0` | about 610 MiB | fine-tuned; the default of `auto`; not yet published |
| `basalt-translator-1.7b-q8_0` | about 1.7 GiB | fine-tuned, most accurate; `auto` on 4 or more cores with memory to spare; not yet published |
| `qwen3-0.6b-q8_0` | 610 MiB | fastest; weak translator without fine-tuning |
| `qwen3-1.7b-q8_0` | 1.7 GiB | the recommendation among the published files |
| `qwen3-4b-q4_k_m` | 2.4 GiB | best of the three untuned; slow on 2 to 4 CPUs |

The fine-tuned translators (LoRA on generated request/intent pairs, see
the report) were more accurate than all three publisher models and, at
0.6B, several times faster. Their manifest entries are marked
`unpublished`: the weights require a signed release (the key ceremony,
[key-ceremony.md](key-ceremony.md)), so the entries carry placeholder
checksums and `basalt-llm-fetch` refuses them (fail closed) with a message
that points to the published models. When the weights are released, the
entries get their checksum, size and URL and `auto` works with no change
to an installed configuration.

In a lab, a fine-tuned GGUF you built yourself can be used before then:

```sh
sudo install -m 0644 basalt-translator-0.6b-q8_0.gguf /var/lib/basalt-llm/models/
sudo restorecon -v /var/lib/basalt-llm/models/basalt-translator-0.6b-q8_0.gguf
sudo systemctl restart basalt-llm        # MODEL=auto or MODEL=0.6b
```

A file installed by hand has no checksum to verify against;
`basalt-llm-fetch --verify` lists it as not yet published.

## basalt ask

```
$ basalt ask "por que o nginx caiu?"
Understood as: basalt why nginx
(the normal report of `basalt why nginx` follows)

$ basalt ask "aplica a p-97dfa5"
Understood as: basalt apply p-97dfa5
This changes the system, so it is not run from a request in natural language.
Run it yourself to see the exact commands and confirm them:
  sudo basalt apply p-97dfa5
```

How a request becomes a command:

1. The model receives the request and a prompt (instructions with
   examples, or the compact prompt for a fine-tuned model; `prompt = auto`
   picks by the model's name). Its output is
   constrained by a JSON schema (llama.cpp turns it into a grammar): one
   of ten intents, each with exactly its arguments (`status`, `why` +
   unit, `fix_selinux` + time window, `snapshots` + list/diff/rollback and
   numbers, `disk`, `pending` + all, `show` + id, `apply` + id, `clarify`,
   `none`). Nothing else can be produced.
2. The answer is validated again (unit names, `p-` ids, durations).
3. It is checked against the request: a proposal id or snapshot number the
   person did not write, or a unit that matches no word of the request
   (small typos allowed), turns the answer into `clarify`; an invented time
   window falls back to the default. Small models copy ids from examples
   and invent units; this check does not depend on the model.
4. `clarify` asks the person for the missing piece; `none` says the request
   is outside what the assistant does (installing software, restarting or
   stopping services, disabling SELinux, any shell command).
5. Read-only intents run as if typed. `apply` and a snapshot rollback are
   only printed: the person runs them and confirms the exact commands.
6. As root, each request is recorded in the audit log (`ask`): the request,
   the model's answer, the intent acted on and why grounding changed it.

`--dry-run` only prints the command.

## Decision-layer model backend

```ini
[decision]
backend = openai-compatible
endpoint = unix:/run/basalt-llm/llm.sock

[calibration]
unit.cause = 2.46        # temperatures fitted with the evaluation suite
```

The options of a question are numbered, the output is constrained to one
number (a single token), and the probability of each option is read from
the log-probabilities of that token, renormalized over the valid options;
a per-question temperature rescales them before the threshold is applied.
The model sees the question, the diagnosers' findings, the journal lines
of a unit failure and the sizes of a disk report. It answers `unit.cause`,
`avc.class`, `dnf.next` and `disk.cause`; severity, notification and
routing stay with the rules. If the model is not reachable the rules
answer, marked `(fallback)` in the audit log.

The rules remain the default: on the shared evaluation suite they were
more accurate than every untuned model measured, and a model answer costs
seconds where the rules cost microseconds. The backend is there to
measure and compare models (yours included) with `basalt-eval decide`.

## Confinement

Three independent fences keep the model service off the network:

- systemd: `PrivateNetwork=yes` (an empty network namespace, only its own
  loopback), `RestrictAddressFamilies=AF_UNIX`, `IPAddressDeny=any`;
- SELinux: `basalt_llm_t` may create Unix stream sockets only; no TCP,
  UDP, raw, packet, ICMP, SCTP or netlink route socket, no port
  permission, no transitions out of the domain;
- the server listens only on `/run/basalt-llm/llm.sock`; the directory is
  `0750` and SELinux lets only `basalt_assistant_t` and unconfined
  administrators connect.

Further hardening: `DynamicUser`, no capabilities, `ProtectSystem=strict`,
`ProtectHome`, `PrivateDevices`, `PrivateUsers`, `MemoryDenyWriteExecute`,
`SystemCallFilter=@system-service` minus privileged calls, read-only
models, `Nice=10`, `CPUWeight=50`, `MemoryMax=4G`. The model files are
`basalt_llm_model_t` and only readable by the domain.

Checked in the lab (enforcing): the service's namespace has only `lo` and
no routes; a connection from inside it to the lab gateway and to the
internet fails; `sesearch` shows no socket creation other than Unix
stream for the domain; a copy of `curl` run in `basalt_llm_t` without the
namespace gets `failed to open socket: Permission denied` and an AVC for
`tcp_socket create`, while the same request from an unconfined unit
reaches the server; 0 denials of `basalt_llm_t` and `basalt_assistant_t`
while serving the assistant.

The only network access needed is the model download, done by an
administrator with `basalt-llm-fetch` (HTTPS only, checksum verified,
partial downloads resumed, a mismatching file deleted).

## Resources

The service is a background process: `THREADS=0` uses every CPU it may
(at most 8) at a low priority; `CTX_SIZE=2048` is enough for the
assistant's prompts; `CACHE_RAM=256` bounds the prompt cache
(llama-server's own default is 8 GiB). The first request after a start
reads the whole prompt; later ones reuse its cached prefix.

## Tests

`make llm-test` (`packages/basalt-llm/tests/select-test.sh`, also part of
`make ci-lint`) checks the selection: the thresholds at and just below
each limit, CPU topologies with and without SMT, memory and cgroup limits
read from fake files, the start script's resolution and log line, and that
every unpublished entry fails closed in `basalt-llm-fetch`.

## Build

`packages/basalt-llm/build.sh` (or `make rpm-llm`) builds the two RPMs in a
Fedora container from the llama.cpp release archive, pinned by version and
SHA-256. It is not part of `make rpms` or CI: compiling takes several
minutes.
