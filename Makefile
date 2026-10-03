# Basalt OS image: build, sign, publish, and a local lab to install and update it.
# Configuration comes from .env (copy env.example); variables given on the make
# command line override it, e.g. `make build BASALT_VERSION=0.0.2`.
# Run on the build host (podman, skopeo; the lab also needs libvirt/KVM, swtpm, OVMF).

SHELL := /bin/bash
S := scripts
L := scripts/lab

.PHONY: help build publish verify release ci \
        lab-tools lab-keys lab-registry lab-registry-status lab-registry-down \
        lab-net lab-installer lab-vm lab-install lab-detach lab-boot lab-auth \
        lab-ssh lab-console lab-measure lab-destroy lint clean

help: ## Show targets
	@grep -hE '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-22s %s\n", $$1, $$2}'

# --- image ------------------------------------------------------------------

build: ## Build the image into local storage (BASALT_VERSION)
	$(S)/build.sh

publish: ## Push, sign by digest (cosign key), move the channel tag
	$(S)/publish.sh

verify: ## Verify signature and policy for the channel tag (or TAG=...)
	$(S)/verify.sh $(TAG)

release: build publish verify ## build + publish + verify (local CI)

ci: lint release ## What a CI run does on a private registry

lint: ## Shell and Python syntax checks (shellcheck when installed)
	@for f in $(S)/*.sh $(L)/*.sh image/*.sh rootfs/usr/bin/basalt-install; do bash -n "$$f" || exit 1; done
	@python3 -m py_compile $(L)/console-unlock.py && rm -rf $(L)/__pycache__
	@if command -v shellcheck >/dev/null; then shellcheck -x -S warning $(S)/*.sh $(L)/*.sh image/*.sh rootfs/usr/bin/basalt-install; else echo "shellcheck not installed, syntax only"; fi
	@echo lint ok

# --- lab: registry, keys --------------------------------------------------------

lab-tools: ## Fetch cosign and build the lab tool image (virt-fw-vars)
	$(L)/tools.sh

lab-keys: ## Generate the dev cosign key pair and lab SSH key (LAB_DIR/keys, 0600)
	$(L)/keys.sh

lab-registry: ## Start the lab registry (TLS from a lab CA, htpasswd)
	$(L)/registry.sh up

lab-registry-status: ## Show the lab registry and its catalog
	$(L)/registry.sh status

lab-registry-down: ## Stop the lab registry (data kept)
	$(L)/registry.sh down

# --- lab: VM ----------------------------------------------------------------------

lab-net: ## Define and start the isolated lab network
	$(L)/vm.sh net-up

lab-installer: ## Write the installer disk from the local image
	$(L)/vm.sh installer-disk

lab-vm: ## Create the VM (Secure Boot OVMF + swtpm) and boot the installer
	$(L)/vm.sh create

lab-install: ## basalt-install onto the target disk inside the VM (LUKS2 + TPM2 + recovery key)
	$(L)/install-target.sh

lab-detach: ## Remove the installer disk (VM shut down)
	$(L)/vm.sh detach-installer

lab-boot: ## Start the VM and wait for SSH
	$(L)/vm.sh start

lab-auth: ## Give the installed system lab registry pull credentials
	$(L)/vm.sh registry-auth

lab-ssh: ## SSH into the VM as root
	$(L)/vm.sh ssh

lab-console: ## Attach to the VM serial console
	$(L)/vm.sh console

lab-measure: ## Image size, boot time, idle memory, SELinux denials
	$(L)/measure.sh

lab-destroy: ## Remove the VM, its NVRAM and TPM state (disks kept)
	$(L)/vm.sh destroy

clean: ## Remove build outputs in this tree
	rm -rf build
