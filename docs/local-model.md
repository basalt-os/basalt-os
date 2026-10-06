# The optional local language model

Status: pre-alpha, milestone 2c. Off by default on servers; on the
desktop edition installed and offered to the person when a skill needs it. Measurements and the
reasoning behind the defaults: [milestone-2b-report.md](milestone-2b-report.md)
(translator, decision backend) and [milestone-2c-report.md](milestone-2c-report.md)
(humanize).

The system assistant ([assistant.md](assistant.md)) works without any
language model. A small model, running on the machine's CPU with no
network access, can be added for three things:

1. `basalt ask "..."`: a request in natural language is translated into
   one `basalt` command (the assistant answers in English; the translator
   was also trained on Brazilian Portuguese requests);
2. a model backend for the decision layer (`[decision] backend =
   openai-compatible`): the model answers the bounded questions the rules
   answer today, with a probability per option;
3. humanize (`[humanize] enabled = yes`): the model writes the explanation
   of a finding in its own words, from the finding's facts only (below).

None gives the model any power. It returns text constrained by a schema,
the assistant validates and checks it, and every change still goes
through `basalt apply` with its preview, confirmation, snapshots and audit
record.

## Pieces

| Piece | What it is |
|---|---|
| `basalt-llm` (package; part of the desktop edition, optional on servers) | llama.cpp's `llama-server` 0.5.0 built for the CPU only (all x86-64 variants, the best one is picked at run time), in `/usr/lib64/basalt-llm`; `basalt-llm.service`; `basalt-llm-fetch`; `/etc/basalt/llm.conf`. About 9 MiB as an RPM, no model inside. |
| `basalt-llm-selinux` | SELinux module `basalt_llm`: domain `basalt_llm_t` |
| models | GGUF files in `/var/lib/basalt-llm/models`, downloaded by the desktop's confined download service after the person's consent ([models.md](models.md)) or by an administrator, with `basalt-llm-fetch`, and checked against the SHA-256 in `/usr/share/basalt-llm/models.manifest` |
| model selection | `/usr/libexec/basalt-llm/basalt-llm-select`, used by the service at each start (`MODEL=auto`) and by `basalt-llm-fetch auto` |
| `basalt ask` | part of `basalt-assistant` (`internal/translate`) |
| model backend | part of `basalt-assistant` (`internal/decide/model.go`) |

Fedora's own `llama-cpp` package was not used: it is built with ROCm and
pulls in about 6 GiB of GPU libraries, against a light server image.

## On the desktop: nothing to set up

The desktop edition installs `basalt-llm` without a model. When a skill
needs the model (a summary of a mailbox or a page) and none is
downloaded, a card offers it: "Download the assistant's local model (1.8
GB) from huggingface.co? It runs on this computer.", with Download and
Not now. Settings, Voice and assistant, has the same Download (and
Remove). After Download the confined download service of
[basalt-models](models.md) fetches `basalt-llm-fetch recommended`,
verifies it, turns `[translator]` on in `/etc/basalt/assistant.conf`,
and enables and starts `basalt-llm.service`; the desktop's command bar
and skills use the model at once, and a notification says it is ready.
Nothing is downloaded without the person's consent.

`recommended` is the model `MODEL=auto` selects on the machine (below)
when its weights are published, else its published stand-in: the
publisher's Qwen3 checkpoint of the same size (`qwen3-0.6b-q8_0`,
`qwen3-1.7b-q8_0`). `MODEL=auto` runs the stand-in when it is the
downloaded one, so nothing in `llm.conf` changes.

## On a server (administrators)

```sh
sudo dnf install basalt-llm               # also basalt-llm-selinux
basalt-llm-fetch --list                   # also says what MODEL=auto picks here
sudo basalt-llm-fetch recommended         # downloads it, verifies the SHA-256
sudo systemctl enable --now basalt-llm
sudoedit /etc/basalt/assistant.conf       # [translator] enabled = yes
basalt ask "why did nginx stop?"
```

`basalt-llm-fetch auto` asks for the fine-tuned translator itself, and
until the translators are published (below) it stops with a message
that says so; `recommended` downloads the published stand-in instead.
Other published models work too:

```sh
sudo basalt-llm-fetch qwen3-1.7b-q8_0
sudoedit /etc/basalt/llm.conf             # MODEL=qwen3-1.7b-q8_0
sudo systemctl enable --now basalt-llm
```

`basalt-llm-fetch` also has `--plan NAME` (what a download would fetch,
machine readable, no root), `--list --porcelain`, `--remove NAME`,
`--status FILE` (progress for the desktop) and exit status 3 when the
server cannot be reached (the partial file is resumed by the next run),
4 for a checksum mismatch (the file is deleted) and 5 when the model
directory is not writable.

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
a run. `auto` runs the first downloaded model among its choice and the
fallbacks: for the 1.7B, the 1.7B translator, the 0.6B translator, then
the stand-ins `qwen3-1.7b-q8_0` and `qwen3-0.6b-q8_0`; for the 0.6B, the
0.6B translator, then `qwen3-0.6b-q8_0` (the log says which and why).
The 1.7B is never used where `auto` chose the 0.6B unless `MODEL=1.7b`
asks for it. `basalt-llm-fetch auto` downloads
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
$ basalt ask "why did nginx stop?"
Understood as: basalt why nginx
(the normal report of `basalt why nginx` follows)

$ basalt ask "apply p-97dfa5"
Understood as: basalt apply p-97dfa5
That would change the system, so I don't run it from a request in your own words.
Run it yourself: you will see the exact commands and confirm them.
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

## Humanize: the model writes the explanation

```ini
[humanize]
enabled = yes
endpoint = unix:/run/basalt-llm/llm.sock   # the default
#model =
#prompt = auto        # compact for basalt-render-* models, full instructions otherwise
#max_chars = 600
#timeout = 30s
#stream = yes
```

The model gets one finding as JSON: its kind and cause, the values the
diagnosers found (unit, file, line, snapshot, port, sizes) and the planned
changes as typed actions with their parameters. It writes only the
explanation paragraph; the template keeps everything that can be acted on
(commands, risk, undo, the apply line). Its text is kept only if every
number, path, file name, unit, SELinux type or boolean and proposal id in
it is in the facts, it has no command word, markup, address or forbidden
advice, it names the unit, and it stays under `max_chars`; otherwise the
template's paragraph is shown ([assistant.md](assistant.md#humanize-optional)).
Code quotes and bold around names are removed before the check; the names
inside are still checked. The same rules apply to every model, local or
remote, small or large.

The local model service serves one model at a time: the translator and
humanize share it. For both at once, run humanize against a second server
(below) or keep the translator off.

### Bigger or remote models

Any OpenAI-compatible endpoint works:

| Endpoint | Example | Notes |
|---|---|---|
| the local service | `unix:/run/basalt-llm/llm.sock` | default; no network |
| a bigger local server | `http://127.0.0.1:8080/v1` (llama.cpp, Ollama, vLLM) | loopback only, no key |
| a remote provider | a hosted API or a self-hosted server on another machine | opt-in, below |

A remote endpoint is refused unless the person opts in:

```ini
[humanize]
enabled = yes
endpoint = https://api.example.com/v1
model = their-model-name                   # required for a remote endpoint
allow_remote = yes
api_key_file = /etc/basalt/humanize.key    # root-only; sent only to that endpoint
```

For a remote endpoint:

- secrets, tokens, keys (key=value secrets, bearer tokens, private key
  blocks, long random strings), e-mail addresses, IP addresses and user
  names in home directory paths are replaced by `[redacted]` in the facts
  before they are sent; unit names, system paths, SELinux types, numbers
  and package names stay, because they are what the text is about;
- the full request body is printed before it leaves the machine (the key
  travels in an `Authorization` header and is never printed or logged);
- a text that repeats a redacted marker is rejected;
- llama.cpp-only request options are left out (hosted APIs refuse unknown
  parameters);
- the audit record of each humanized text says that the endpoint was
  remote and how many values were redacted.

Without `allow_remote = yes`, or without a model name, the assistant
prints the template with a note on stderr; nothing is sent.
`internal/explain` has a test that runs the whole path against a mock
OpenAI-compatible server under a remote name: redaction, the request
shown, the key in the header only, streaming, and the fallback.

### A model trained for it

`basalt-render-0.6b-q8_0` and `basalt-render-1.7b-q8_0` are Qwen 3 models
fine-tuned (LoRA) on fact and prose pairs made by `basalt-eval
render-data`: facts sampled from fixed pools, prose from the assistant's
own templates with random equivalent phrasings (no third-party text).
Training: `eval/tools/train-render-lora.py`. Like the fine-tuned
translators they are not published yet; in a lab, install a GGUF you
built as shown above and set `MODEL` to its path. Measurements and the
recommendation (templates stay the default):
[milestone-2c-report.md](milestone-2c-report.md).

## The desktop's assistant

The Basalt desktop shell uses the same local model for its command bar
and its skills (the `[translator]` settings). Each person may choose,
in the desktop's Settings (Voice and assistant), the language the
assistant answers in and, only where the administrator allows it, a
remote model instead of the local one:

```ini
# /etc/basalt/desktop-models.conf (read by the desktop shell)
[policy]
allow_remote = no          # yes: people may opt in to a model listed below

[remote example]
label = Example provider
endpoint = https://api.example.com/v1
model = their-model-name
```

A remote model needs both `allow_remote = yes` here and the person's own
opt-in, and only the listed endpoints (https) can be chosen; each person
keeps their key in a private file of their own. When the answer language
is not English, the desktop adds one line to the model's system prompt:
answer the person in that language and keep every identifier, key, enum
value, path and name in English, as specified. The schemas and the
checks of the answers are the same in every language. In the lab,
qwen3-1.7b answered in Brazilian Portuguese as asked; its summaries were
understandable but not always faithful (a weekday translated wrong once,
left in English once). basalt-shell's `docs/voice.md` has the details.

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

The only network access needed is the model download, done by
`basalt-llm-fetch` (HTTPS only, checksum verified, partial downloads
resumed, a mismatching file deleted), run by the desktop's confined
download service after the person's consent or by an administrator.

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
