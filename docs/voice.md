# Desktop voice: basalt-voice

`basalt-voice` is the speech-to-text part of the desktop's push to talk.
The voice service itself (`basalt-voiced`, its SELinux domain
`basalt_voice_t` and the user unit `basalt-voice.service`) ships with
[basalt-shell](https://github.com/basalt-os/basalt-shell); see its
`docs/voice.md` for how push to talk, the skills and their security model
work.

## What the package holds

- whisper.cpp's `whisper-cli`, built from the upstream release archive
  (version and SHA-256 pinned in `packages/basalt-voice/build.sh` and the
  spec) for the CPU only. Every x86-64 variant is built and the best one
  is picked at run time. Files live in `/usr/lib64/basalt-voice`
  (RUNPATH `$ORIGIN`), so they never shadow a system library.
- `basalt-voice-fetch`, the only program that downloads speech models.
- `/usr/share/basalt-voice/models.manifest`, the list of models it may
  download.

No model is packaged. Fedora's `whisper-cpp` is not used: it ships the
library only (no `whisper-cli`) and pulls the ROCm runtime.

## Models: how they are fetched

An administrator runs `basalt-voice-fetch` once:

```sh
sudo basalt-voice-fetch default      # ggml-base.en, Silero VAD, en_US-ljspeech-medium (+ .json)
sudo basalt-voice-fetch multilingual # ggml-small-q5_1 and Silero VAD: speech in other languages
basalt-voice-fetch --list            # every model of the manifest, size, license, present or not
sudo basalt-voice-fetch ggml-small.en
sudo basalt-voice-fetch --verify     # re-check the files already downloaded
```

- Each manifest row is `name sha256 size license url`. Every URL is
  pinned to a commit of the publisher's Hugging Face repository
  (`/resolve/<commit>/`), so a moved tag or branch cannot change what is
  downloaded.
- The download is https only (`curl --proto =https`). The file is written
  as `.part`, checked against the SHA-256 of the manifest, and only then
  moved into `/var/lib/basalt-voice/models` with its SELinux label. A
  mismatch deletes the file and fails.
- The voice service has no network at all (no inet sockets in its SELinux
  domain, `RestrictAddressFamilies=AF_UNIX` in its unit). Fetching is the
  only network step and it is never done by the service.

## Speech models and languages

Whisper models named `.en` understand English only. The others are
multilingual and are what a speech language other than English needs:

| Model | Download | Languages | Speech to text in the lab (10 vCPU) |
|---|---|---|---|
| `ggml-base.en`, `ggml-base.en-q5_1` | 141 MiB, 56 MiB | English | about 1.4 s per request |
| `ggml-small.en`, `ggml-small.en-q5_1` | 465 MiB, 181 MiB | English | |
| `ggml-base`, `ggml-base-q5_1` | 141 MiB, 56 MiB | multilingual | about 1.3 s (q5_1) |
| `ggml-small`, `ggml-small-q5_1` | 465 MiB, 181 MiB | multilingual | about 3.8 s (q5_1), fewer mistakes |
| `ggml-large-v3-turbo-q5_0` | 547 MiB | multilingual | slow on a CPU |

Each person picks their own speech language, speech model and voice in
the desktop's Settings (Voice and assistant), within the administrator's
policy in `/etc/basalt/voice.conf` (`BASALT_VOICE_LANGUAGE`,
`BASALT_VOICE_ALLOWED_MODELS`, `BASALT_VOICE_MAX_MODEL_MB`); the voice
service checks every choice on every request. See basalt-shell's
`docs/voice.md` for the settings and the measurements.

## Licenses of the models

- Whisper (OpenAI weights, ggml conversions by whisper.cpp): MIT.
- Silero VAD v5.1.2 (ggml conversion): MIT.
- Piper voices: only voices trained on public-domain data are listed: LJ
  Speech, Kristin and Norman (LibriVox readings), and John (fine-tuned
  from Kristin). Most other English Piper voices are trained on or
  fine-tuned from the "lessac" voice, whose dataset license does not allow
  redistribution in a product, and some are CC BY-NC-SA. They are not
  listed.

Voices for other languages: none is listed. At the pinned revision of
the Piper voices, every Portuguese voice is fine-tuned from "lessac"
(Brazilian cadu, faber and jeff; European tugão) or from "ryan", whose
data is CC BY-NC-SA (Brazilian edresson, itself trained on CC BY data).
Most Spanish, French, German and Italian voices are fine-tuned from
lessac or ryan too, and those trained from scratch use data under CC BY,
under a license of their own, or of an origin the dataset does not
state. Answers in those languages are shown and not spoken (the desktop
says so); a voice is added only when its training data and its base
voice meet the rule above.

`packages/basalt-voice/tests/manifest-test.sh` (run by `make ci-lint`)
fails when a row is not pinned, has no checksum, names a Piper voice
outside that allowlist, when the default or multilingual sets of
`basalt-voice-fetch` are missing, or when the Whisper models come from
more than one revision.

## Text to speech

Spoken answers use Piper with one of the voices above, and only for
answers in the voice's own language. Piper itself is not packaged yet:
without it answers are shown and not spoken. Packaging it
(the C++ program against Fedora's onnxruntime and espeak-ng) is the next
step for this package.

## Build

- `packages/basalt-voice/build.sh [OUT_DIR]` builds the binary and source
  RPMs in a Fedora container (a few minutes: it compiles whisper.cpp).
  `make rpm-voice` runs it.
- It is part of the basalt-testing release set: `scripts/release/build-testing.sh`
  builds it next to basalt-shell (see `docs/publishing.md`). It is not built
  by CI.
