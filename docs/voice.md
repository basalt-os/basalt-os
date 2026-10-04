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

## Licenses of the models

- Whisper (OpenAI weights, ggml conversions by whisper.cpp): MIT.
- Silero VAD v5.1.2 (ggml conversion): MIT.
- Piper voices: only voices trained on public-domain data are listed: LJ
  Speech, Kristin and Norman (LibriVox readings), and John (fine-tuned
  from Kristin). Most other English Piper voices are trained on or
  fine-tuned from the "lessac" voice, whose dataset license does not allow
  redistribution in a product, and some are CC BY-NC-SA. They are not
  listed.

`packages/basalt-voice/tests/manifest-test.sh` (run by `make ci-lint`)
fails when a row is not pinned, has no checksum, or names a Piper voice
outside that allowlist.

## Text to speech

Spoken answers use Piper with one of the voices above. Piper itself is not
packaged yet: without it answers are shown and not spoken. Packaging it
(the C++ program against Fedora's onnxruntime and espeak-ng) is the next
step for this package.

## Build

- `packages/basalt-voice/build.sh [OUT_DIR]` builds the binary and source
  RPMs in a Fedora container (a few minutes: it compiles whisper.cpp).
  `make rpm-voice` runs it.
- It is part of the basalt-testing release set: `scripts/release/build-testing.sh`
  builds it next to basalt-shell (see `docs/publishing.md`). It is not built
  by CI.
