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
        lab-destroy

help: ## Show targets
	@grep -hE '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-20s %s\n", $$1, $$2}'

# --- packages, repository, ISO ------------------------------------------------------

rpms: ## Build basalt-release, basalt-logos, basalt-snapshots (Fedora container)
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
	          packages/basalt-snapshots/42_basalt_snapshots packages/basalt-logos/06_basalt_theme; do bash -n "$$f" || exit 1; done
	@python3 -m py_compile $(L)/console-unlock.py $(L)/grub-console.py && rm -rf $(L)/__pycache__
	@if command -v shellcheck >/dev/null; then shellcheck -x -S warning $(S)/*.sh $(L)/*.sh packages/basalt-snapshots/basalt-snapshot-dnf packages/basalt-snapshots/basalt-snapshot-boot packages/basalt-snapshots/basalt-snapshots-setup packages/basalt-snapshots/basalt-rollback; else echo "shellcheck not installed, syntax only"; fi
	@if command -v ksvalidator >/dev/null; then ksvalidator -v F44 kickstart/basalt-server.ks; fi
	@echo lint ok

clean: ## Remove build outputs in this tree
	rm -rf build

# --- lab ---------------------------------------------------------------------------

lab-tools: ## Build the lab tool image (virt-fw-vars)
	$(L)/tools.sh

lab-keys: ## Dev repository signing key, lab SSH key, lab site files (LAB_DIR)
	$(L)/keys.sh

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

lab-destroy: ## Remove the VM, its disk, NVRAM and TPM state
	$(L)/vm.sh destroy
