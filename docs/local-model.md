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
| `basalt ask` | part of `basalt-assistant` (`internal/translate`) |
| model backend | part of `basalt-assistant` (`internal/decide/model.go`) |

Fedora's own `llama-cpp` package was not used: it is built with ROCm and
pulls in about 6 GiB of GPU libraries, against a light server image.

## Install and enable

```sh
sudo dnf install basalt-llm               # also basalt-llm-selinux
basalt-llm-fetch --list
sudo basalt-llm-fetch qwen3-1.7b-q8_0     # downloads, verifies the SHA-256
sudoedit /etc/basalt/llm.conf             # MODEL=qwen3-1.7b-q8_0
sudo systemctl enable --now basalt-llm
sudoedit /etc/basalt/assistant.conf       # [translator] enabled = yes
basalt ask "por que o nginx caiu?"
```

The models in the manifest are the publisher's own GGUF files (the Qwen
organisation), each pinned to a revision and checked by SHA-256; all
three are Apache-2.0. A file given by absolute path (`MODEL=/path/x.gguf`)
is used as is, without a checksum: only for models you built or trust.

| Model | Download | Notes |
|---|---|---|
| `qwen3-0.6b-q8_0` | 610 MiB | fastest; weak translator without fine-tuning |
| `qwen3-1.7b-q8_0` | 1.7 GiB | the default recommendation among the published files |
| `qwen3-4b-q4_k_m` | 2.4 GiB | best of the three untuned; slow on 2 to 4 CPUs |

Fine-tuned translators (`basalt-translator-0.6b` and `-1.7b`, LoRA on
generated request/intent pairs, see the report) were more accurate than
all three and, at 0.6B, several times faster; they are not published yet,
so they are not in the manifest. With such a model set `[translator]
prompt = compact`.

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
   examples, or the compact prompt for a fine-tuned model). Its output is
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

## Build

`packages/basalt-llm/build.sh` (or `make rpm-llm`) builds the two RPMs in a
Fedora container from the llama.cpp release archive, pinned by version and
SHA-256. It is not part of `make rpms` or CI: compiling takes several
minutes.
