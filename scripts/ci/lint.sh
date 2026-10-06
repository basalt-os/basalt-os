#!/usr/bin/env bash
# Static checks for CI and for local use.
#
#   scripts/ci/lint.sh              run the checks here (needs ShellCheck, rpmlint,
#                                   pykickstart, python3, make; actionlint optional)
#   scripts/ci/lint.sh --container  run them in a throwaway Fedora container that
#                                   has the tools (what CI does)
#
# Checks: `make lint` (shell and Python syntax, ShellCheck, ksvalidator),
# the basalt-nvidia tests,
# ShellCheck on the CI scripts, the basalt-llm model selection tests, the upload.sh tests
# (a local fake S3 endpoint), rpmlint on every package spec (filters in
# scripts/ci/rpmlint.toml), ksvalidator on the kickstart for this Fedora
# release, actionlint on the workflows.
source "$(dirname "$0")/../lib.sh"

# actionlint, pinned by version and SHA-256 of the release archive.
ACTIONLINT_VERSION=1.7.12
ACTIONLINT_SHA256=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8

if [[ "${1:-}" == --container ]]; then
  log "running the checks in $FEDORA_IMAGE"
  in_fedora -v "$REPO_ROOT:/src:ro" -w /src \
    -e FEDORA_RELEASE="$FEDORA_RELEASE" -e ACTIONLINT_VERSION="$ACTIONLINT_VERSION" -e ACTIONLINT_SHA256="$ACTIONLINT_SHA256" \
    "$FEDORA_IMAGE" bash -euc '
      dnf -q -y install make ShellCheck rpmlint pykickstart python3 tar gzip git zsh fish openssl libxml2 awscli2 bc >/dev/null 2>&1 ||
        { echo "dnf install failed" >&2; exit 1; }
      tmp=$(mktemp -d)
      url="https://github.com/rhysd/actionlint/releases/download/v${ACTIONLINT_VERSION}/actionlint_${ACTIONLINT_VERSION}_linux_amd64.tar.gz"
      if curl -fsSL -o "$tmp/a.tgz" "$url" && echo "$ACTIONLINT_SHA256  $tmp/a.tgz" | sha256sum -c --quiet -; then
        tar -xzf "$tmp/a.tgz" -C /usr/local/bin actionlint
      else
        echo "actionlint download or checksum failed" >&2; exit 1
      fi
      # The tree is mounted read-only: work on a copy so make lint can clean up.
      cp -a /src /tmp/src && cd /tmp/src && scripts/ci/lint.sh'
  exit $?
fi

cd "$REPO_ROOT" || die "cannot enter $REPO_ROOT"
fail=0
step() { printf '\n--- %s\n' "$*" >&2; }

step "make lint (syntax, ShellCheck, ksvalidator)"
make --no-print-directory lint || fail=1

step "ShellCheck: CI and release scripts, assistant and model service scripts, their lab scripts"
shellcheck -x -S warning scripts/ci/*.sh packages/basalt-assistant/build.sh scripts/lab/assistant-*.sh scripts/lab/eval-capture.sh \
  packages/basalt-agent/build.sh scripts/lab/agent-test.sh packages/basalt-agent/tests/normal.sh packages/basalt-agent/tests/attacks.sh packages/basalt-agent/tests/driver.sh \
  packages/basalt-agent/tests/credwork.sh packages/basalt-agent/tests/credattacks.sh \
  packages/basalt-resolver/build.sh packages/basalt-ledger/build.sh scripts/lab/ledger-test.sh packages/basalt-ledger/tests/*.sh \
  packages/basalt-llm/build.sh packages/basalt-llm/basalt-llm-start packages/basalt-llm/basalt-llm-fetch \
  packages/basalt-llm/basalt-llm-select packages/basalt-llm/tests/select-test.sh scripts/data-package.sh scripts/release/*.sh || fail=1

step "Shared egress allowlist format: basalt-agent and basalt-resolver carry the same parser"
for f in allowlist.go allowlist_test.go; do
  cmp -s "packages/basalt-agent/internal/allowlist/$f" "packages/basalt-resolver/internal/allowlist/$f" ||
    { echo "packages/basalt-resolver/internal/allowlist/$f differs from basalt-agent's copy" >&2; fail=1; }
done

step "basalt-prompt: ShellCheck and tests (bash, and zsh and fish when installed)"
LC_ALL=C.UTF-8 shellcheck -x -S warning packages/basalt-prompt/basalt-prompt.bash packages/basalt-prompt/basalt-prompt.sh \
  packages/basalt-prompt/basalt-prompt packages/basalt-prompt/tests/prompt-test.sh || fail=1
bash -n packages/basalt-prompt/basalt-prompt.bash || fail=1
if command -v zsh >/dev/null; then zsh -n packages/basalt-prompt/basalt-prompt.zsh || fail=1; fi
if command -v fish >/dev/null; then fish --no-execute packages/basalt-prompt/basalt-prompt.fish || fail=1; fi
LC_ALL=C.UTF-8 packages/basalt-prompt/tests/prompt-test.sh || fail=1

step "basalt-llm: model selection tests"
packages/basalt-llm/tests/select-test.sh || fail=1

step "Release upload: ShellCheck, upload.sh against a local fake S3 endpoint (skipped without the AWS CLI)"
shellcheck -x -S warning scripts/release/tests/upload-test.sh || fail=1
python3 -m py_compile scripts/release/tests/fake-s3.py && rm -rf scripts/release/tests/__pycache__ || fail=1
scripts/release/tests/upload-test.sh || fail=1

step "basalt-voice: ShellCheck, speech model manifest (pinned, checksummed, public-domain Piper voices only)"
shellcheck -x -S warning packages/basalt-voice/build.sh packages/basalt-voice/basalt-voice-fetch packages/basalt-voice/tests/manifest-test.sh || fail=1
packages/basalt-voice/tests/manifest-test.sh || fail=1

step "basalt-models: ShellCheck, tests (policy decision, requests, downloads against a local HTTPS server)"
shellcheck -x -S warning packages/basalt-models/basalt-models-request packages/basalt-models/basalt-models-request-admin \
  packages/basalt-models/basalt-models-run packages/basalt-models/tests/models-test.sh || fail=1
packages/basalt-models/tests/models-test.sh || fail=1

step "NVIDIA driver (basalt-nonfree): ShellCheck, basalt-nvidia tests (trial, check, fallback, kernel guard)"
shellcheck -x -S warning packages/nvidia/check-identical.sh packages/nvidia/basalt-nvidia/basalt-nvidia \
  packages/nvidia/basalt-nvidia/99-zz-basalt-nvidia.install packages/nvidia/basalt-nvidia/basalt-nvidia-run \
  packages/nvidia/tests/basalt-nvidia-test.sh scripts/lab/nvidia-test.sh || fail=1
packages/nvidia/tests/basalt-nvidia-test.sh || fail=1

step "Python syntax: evaluation suite tools, ledger lab fixtures"
python3 -m py_compile eval/tools/*.py && rm -rf eval/tools/__pycache__ || fail=1
python3 -m py_compile packages/basalt-ledger/tests/*.py && rm -rf packages/basalt-ledger/tests/__pycache__ || fail=1

step "Python syntax: assistant policy query helper"
python3 -c "import ast, sys; ast.parse(open(sys.argv[1]).read(), sys.argv[1])" packages/basalt-assistant/dist/basalt-policy-query || fail=1

step "ksvalidator: kickstart (F$FEDORA_RELEASE syntax)"
# The %include files are generated by %pre at install time; they are not
# followed here (no -i), only the kickstart itself is validated.
ksvalidator -v "F$FEDORA_RELEASE" kickstart/basalt-server.ks || fail=1

step "rpmlint: package specs"
mapfile -t specs < <(find packages -name '*.spec' -not -path 'packages/lab/*' | sort)
rpmlint -c scripts/ci/rpmlint.toml "${specs[@]}" || fail=1

if command -v actionlint >/dev/null; then
  step "actionlint: workflows"
  actionlint -no-color .github/workflows/*.yml || fail=1
else
  log "actionlint not installed, workflows not checked"
fi

step "basalt-installer: ShellCheck and Python syntax (build, live image, lab test scripts)"
shellcheck -x -S warning packages/basalt-installer/build.sh packages/basalt-installer/live/build-live.sh \
  packages/basalt-installer/live/mkosi.finalize.chroot packages/basalt-installer/live/mkosi.extra/usr/libexec/basalt-installer/ui-select packages/basalt-installer/live/mkosi.extra/usr/libexec/basalt-installer/ui-start \
  packages/basalt-installer/tests/*.sh || fail=1
python3 -m py_compile packages/basalt-installer/tests/*.py && rm -rf packages/basalt-installer/tests/__pycache__ || fail=1

[[ $fail == 0 ]] || die "lint failed"
log "all checks passed"
