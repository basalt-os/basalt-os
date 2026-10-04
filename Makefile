# Basalt OS: packages, signed repository, installer ISO, and a local lab that
# installs and tests it. Configuration comes from .env (copy env.example);
# variables on the make command line override it, e.g.
# `make rpms FEDORA_RELEASE=45`. Run on a Linux host with podman (the lab also
# needs libvirt/KVM, swtpm, OVMF and docker or podman for the HTTP server).

SHELL := /bin/bash
S := scripts
L := scripts/lab

.PHONY: help rpms rpms-lab repo repo-verify iso-fetch iso all lint clean \
        lab-tools lab-keys lab-net lab-repo lab-repo-down lab-install lab-start lab-stop \
        lab-ssh lab-console lab-measure lab-sb-test lab-snapshot-test lab-rollback-test \
        lab-destroy lab-sb-keys lab-tang lab-mok-test lab-sb-custom-test lab-tang-test lab-kernels-test

help: ## Show targets
	@grep -hE '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-20s %s\n", $$1, $$2}'

# --- packages, repository, ISO ------------------------------------------------------

rpms: ## Build basalt-release, basalt-logos, basalt-snapshots, basalt-security (Fedora container)
	$(S)/build-rpms.sh

rpms-lab: ## Same, plus the lab canary package
	$(S)/build-rpms.sh --lab

repo: ## Sign the built RPMs and publish them to the signed repository (REPO_DIR)
	$(S)/repo.sh publish

repo-verify: ## Check package and repository metadata signatures
	$(S)/repo.sh verify

iso-fetch: ## Download and verify the Fedora netinst ISO
	$(S)/iso.sh fetch

iso: ## Build the Basalt OS installer ISO (kickstart + repository embedded)
	$(S)/iso.sh build

all: rpms repo iso ## rpms + repo + iso

lint: ## Shell and Python syntax checks (shellcheck when installed)
	@for f in $(S)/*.sh $(L)/*.sh packages/basalt-snapshots/basalt-snapshot-dnf packages/basalt-snapshots/basalt-snapshot-boot \
	          packages/basalt-snapshots/basalt-snapshots-setup packages/basalt-snapshots/basalt-rollback \
	          packages/basalt-security/basalt-tpm packages/basalt-security/basalt-secureboot \
	          packages/basalt-snapshots/42_basalt_snapshots packages/basalt-logos/06_basalt_theme; do bash -n "$$f" || exit 1; done
	@python3 -m py_compile $(L)/console-unlock.py $(L)/grub-console.py $(L)/mok-console.py $(L)/serial-watch.py && rm -rf $(L)/__pycache__
	@if command -v shellcheck >/dev/null; then shellcheck -x -S warning $(S)/*.sh $(L)/*.sh packages/basalt-snapshots/basalt-snapshot-dnf packages/basalt-snapshots/basalt-snapshot-boot packages/basalt-snapshots/basalt-snapshots-setup packages/basalt-snapshots/basalt-rollback packages/basalt-security/basalt-tpm packages/basalt-security/basalt-secureboot; else echo "shellcheck not installed, syntax only"; fi
	@if command -v ksvalidator >/dev/null; then ksvalidator -v F44 kickstart/basalt-server.ks; fi
	@echo lint ok

clean: ## Remove build outputs in this tree
	rm -rf build

# --- lab ---------------------------------------------------------------------------

lab-tools: ## Build the lab tool image (virt-fw-vars)
	$(L)/tools.sh

lab-keys: ## Dev repository signing key, lab SSH key, lab site files (LAB_DIR)
	$(L)/keys.sh

lab-sb-keys: ## Dev Secure Boot keys: PK, KEK, db, module CA and signing certificate (LAB_DIR/sb-keys)
	$(L)/sb-keys.sh

lab-tang: lab-net ## Serve a Tang server to the lab network
	$(L)/tang-serve.sh up

lab-net: ## Define and start the isolated lab network
	$(L)/vm.sh net-up

lab-repo: lab-net ## Serve REPO_DIR to the lab network over HTTP
	$(L)/repo-serve.sh up

lab-repo-down: ## Stop the repository HTTP server
	$(L)/repo-serve.sh down

lab-install: ## Unattended install of the lab ISO into a new VM, first boot, recovery key saved
	$(L)/install.sh

lab-start: ## Start the VM and wait for SSH
	$(L)/vm.sh start

lab-stop: ## Shut the VM down
	$(L)/vm.sh stop

lab-ssh: ## SSH into the VM as root
	$(L)/vm.sh ssh

lab-console: ## Attach to the VM serial console
	$(L)/vm.sh console

lab-measure: ## Boot time, idle memory, installed size, SELinux denials
	$(L)/measure.sh

lab-sb-test: ## Secure Boot changes block TPM unlock; the recovery key works
	$(L)/sb-test.sh

lab-snapshot-test: ## dnf install/upgrade make pre/post snapshots; packages persist across reboot
	$(L)/snapshot-test.sh

lab-rollback-test: ## Broken updates rolled back from the system and from the GRUB menu
	$(L)/rollback-test.sh

lab-mok-test: ## MOK mode: enroll the module CA through MokManager, module signature matrix
	$(L)/mok-test.sh

lab-sb-custom-test: ## Custom db mode: own PK/KEK/db, re-signed shim, TPM suspend and reenroll
	$(L)/sb-custom-test.sh

lab-tang-test: ## Tang or TPM2+Tang unlock on a VM installed with basalt.unlock=tang|tpm2+tang
	$(L)/tang-test.sh

lab-kernels-test: ## Orphaned kernels after a rollback: detection and cleanup
	$(L)/kernels-test.sh

lab-destroy: ## Remove the VM, its disk, NVRAM and TPM state
	$(L)/vm.sh destroy

# --- CI (scripts/ci; the same commands run in GitHub Actions) ----------------------

.PHONY: ci-lint ci-build ci-boot-test

ci-lint: ## Static checks in a Fedora container: make lint, ShellCheck, rpmlint, ksvalidator, actionlint
	$(S)/ci/lint.sh --container

ci-build: ## CI build: packages, UNSIGNED repository, ISO, checksums (build/ci-out)
	$(S)/ci/build.sh all

ci-boot-test: ## Install a test ISO in QEMU/KVM (Secure Boot, swtpm) with throwaway keys, smoke checks
	$(S)/ci/boot-test.sh all

# --- system assistant (packages/basalt-assistant) -----------------------------------

.PHONY: rpm-assistant assistant-test lab-assistant-test rpm-agent agent-test lab-agent-test rpm-llm llm-test eval-rules eval-check lab-eval-capture

rpm-assistant: ## Build only basalt-assistant (+ -selinux, source) into the RPM directory
	packages/basalt-assistant/build.sh

assistant-test: ## go vet + go test of basalt-assistant in a Fedora container
	packages/basalt-assistant/build.sh test

rpm-agent: ## Build only basalt-agent (+ -selinux, source) into the RPM directory
	packages/basalt-agent/build.sh

agent-test: ## go vet + go test of basalt-agent in a Fedora container
	packages/basalt-agent/build.sh test

lab-agent-test: ## basalt-agent on a lab VM: profiles, container+native modes, escape-test matrix, 0 AVC during allowed work (scripts/lab/agent-test.sh)
	$(L)/agent-test.sh

lab-assistant-test: ## Assistant on a lab VM: events, proposals, confirmed applies, rollback, confinement (scripts/lab/assistant-test.sh)
	$(L)/assistant-test.sh

# --- optional local model service (packages/basalt-llm) and evaluation suite --------

rpm-llm: ## Build basalt-llm (+ -selinux): llama.cpp server for the CPU, unit without network, SELinux domain (not in CI)
	packages/basalt-llm/build.sh

llm-test: ## Unit tests of basalt-llm's model selection (MODEL=auto, aliases, unpublished models fail closed)
	packages/basalt-llm/tests/select-test.sh

eval-rules: ## Decision-layer evaluation suite (eval/cases) against the rules backend (needs Go)
	@cd packages/basalt-assistant && go run ./tools/basalt-eval decide -cases ../../eval/cases

eval-check: ## Every case in eval/cases: schema, expected actions pass the action validators, snapshot evidence (needs Go)
	@cd packages/basalt-assistant && go run ./tools/basalt-eval check -cases ../../eval/cases

lab-eval-capture: ## Record labeled decision cases on a lab VM (scripts/lab/eval-capture.sh)
	$(L)/eval-capture.sh
