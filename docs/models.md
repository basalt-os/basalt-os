# Model downloads from the desktop (basalt-models)

Basalt OS features work out of the box: nobody should have to run a
command to use voice or the assistant's local model. The models are too
large to ship in the image and come from their publishers, so the
desktop asks the person once, at the moment they need one, and then
downloads it for them:

- push to talk with no speech model for the person's language: the
  voice card says "Voice needs to download the speech model for
  Português (Brasil) (60 MB) from huggingface.co. Download now?", with
  Download and Not now (English gets `ggml-base.en`, any other language
  the multilingual `ggml-base-q5_1`, both with the Silero voice activity
  detector);
- a skill that would summarize (a mailbox, a page) when no language
  model is downloaded: a card offers the assistant's local model, the
  one `basalt-llm` picks for the computer, with its size;
- Settings, Voice and assistant: every speech model of the manifest and
  the local model, with Download and Remove.

Nothing is downloaded before the person chooses Download. The card shows
the progress while the person keeps working, waits when the network is
down and starts again by itself when it is back, says so when a file
arrived damaged (it is deleted, nothing is installed), and a notification
says when the model is ready. The model works at once: no logout, no
restart.

## How a download runs

```
 the person: Download (shell UI only)
        |
 basalt-shelld --pkexec--> /usr/libexec/basalt-models/request download voice english
        |                    polkit: org.basalt-os.models.download
        |                    (active local session, no password)
        |                    checks the target against the manifest
        |                    applies /etc/basalt/models.conf
        |                    ledger: model.download.request (who agreed, what, size)
        |                    systemctl start basalt-models-fetch@voice-english.service
        |
        |  reads /run/basalt-models/voice-english.state (names and sizes only)
        v
 basalt-models-fetch@voice-english.service (confined, see below)
        basalt-voice-fetch --status FILE english   or   basalt-llm-fetch --status FILE recommended
        pinned URL (https only), SHA-256 checked, SELinux label restored
        ledger: model.download (ok or error, with the reason)
        assistant's model only, after a verified download (privileged step):
        [translator] enabled = yes, basalt-llm.service enabled and started,
        ledger: model.enable
```

- Only the shell UI's own connection may accept an offer or remove a
  model (`models.download`, `models.remove`); an agent connected to the
  shell can neither start a download nor see the offers.
- The request program accepts only a set (`english`, `multilingual`,
  `recommended`) or a model name of a manifest, refuses an unpublished
  model, and starts the service; it never downloads anything itself.
- The download service runs as root without any capability (it owns
  the model directories), with a read-only system
  (`ProtectSystem=strict`), only the two model directories and its
  progress directory writable, no home folders, private `/tmp` and
  devices, `NoNewPrivileges`, a system call filter, `RestrictNamespaces`,
  `MemoryDenyWriteExecute`, and Unix and IP sockets only. It runs at a
  low priority (idle I/O).
- The files come from the URLs of the manifests, each pinned to a
  publisher revision, over HTTPS only, and are moved into place only
  after their SHA-256 matches. A file that does not match is deleted.

## Who may download

`/etc/basalt/models.conf`:

| Value | Who |
|---|---|
| `downloads = everyone` (default) | any person in an active local session, without a password (polkit `allow_active`; remote and inactive sessions are refused) |
| `downloads = administrators` | members of `admin_group` (`wheel` by default); anyone else is asked for an administrator's password (polkit action `org.basalt-os.models.download-admin`, `auth_admin_keep`) |
| `downloads = nobody` | the desktop never downloads; the cards say downloads are off and to ask the administrator |

An unknown value counts as `nobody`. The request program applies the
same decision as the desktop, so polkit's answer alone never starts a
download that the policy forbids.

## The audit ledger

| Event | Producer | When |
|---|---|---|
| `model.download.consent` | basalt-shell (the person's uid) | the person chose Download |
| `model.download.request` | basalt-models (uid of the person who asked) | the request was allowed or refused (with the reason) |
| `model.download` | basalt-models | the download ended: verified (`ok`) or failed (`error`, with the reason: network, checksum, storage) |
| `model.enable` | basalt-models | the assistant's model service was turned on |
| `model.remove` | basalt-models | a model was removed |

Each record has what was downloaded (`what`, `models`), the size in bytes
and the person (`user`, `uid`). `basalt-ledger` shows them in plain
English, for example "basalt agreed to download english
(ggml-base.en,ggml-silero-v5.1.2, 148849309 bytes) from the desktop".

## For administrators

```sh
sudoedit /etc/basalt/models.conf                 # downloads = everyone | administrators | nobody
systemctl list-units 'basalt-models-fetch@*'     # downloads running now
cat /run/basalt-models/voice-english.state       # the progress the desktop shows
basalt-ledger --producer basalt-models           # who downloaded what
sudo basalt-voice-fetch english                  # the same downloads by hand (docs/voice.md)
sudo basalt-llm-fetch recommended                # (docs/local-model.md)
```

## Tests

`make models-test` (`packages/basalt-models/tests/models-test.sh`, also
part of `make ci-lint`): the policy decision for every value, polkit
action and administrator approval; the polkit actions' file; request
checks and refusals with their exit status; the request and progress
files and the ledger records; and the download state machine with the
real `basalt-voice-fetch` and `basalt-llm-fetch` against a local HTTPS
server: progress while downloading, done, a checksum mismatch (deleted,
`error=checksum`), the server unreachable (`error=network`, the partial
file kept) and the retry resuming it, and the assistant's model turned on
only after its download.
