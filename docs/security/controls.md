# Security controls catalog

<!-- Generated from controls.yaml by scripts/ci/security-controls-check.sh --render. Do not edit by hand. -->

Version 5, updated 2026-10-06. Each control has a
stable ID, a requirement, the reason for it, where it is enforced, how it
is verified and its status. Implemented means enforced in the shipped
packages and checked; partial means enforced for part of the scope or
without the full verification; planned means decided or proposed but not
built, so nothing should rely on it yet. How controls change:
[change-policy.md](change-policy.md). How to check them yourself:
[audit-guide.md](audit-guide.md).

## Summary

| Area | Implemented | Partial | Planned | Total |
|---|---|---|---|---|
| BOOT: Secure Boot, kernel lockdown and module signatures | 3 | 1 | 0 | 4 |
| PKG: Packages, repositories and the build and release pipeline | 6 | 0 | 2 | 8 |
| DISK: Disk encryption and unlock | 5 | 0 | 0 | 5 |
| MAC: Mandatory access control (SELinux) and service hardening | 6 | 0 | 3 | 9 |
| GATE: Approvals, confirmations and the approval gate | 6 | 2 | 4 | 12 |
| LEDGER: The audit ledger and audit logs | 8 | 0 | 2 | 10 |
| NET: Network egress and inbound access | 9 | 0 | 1 | 10 |
| AGENT: Confinement of AI agents | 9 | 0 | 0 | 9 |
| AI: Models, knowledge, the assistant and content handling | 6 | 2 | 2 | 10 |
| PRIV: Privacy and data leaving the machine | 3 | 1 | 2 | 6 |
| UPD: Updates, snapshots and rollback | 4 | 1 | 0 | 5 |
| KEY: Signing keys and their custody | 1 | 4 | 0 | 5 |
| GOV: Process, disclosure and the security program itself | 2 | 2 | 0 | 4 |
| All | 68 | 13 | 16 | 97 |

## BOOT: Secure Boot, kernel lockdown and module signatures

### BSC-BOOT-001

Verified boot chain with UEFI Secure Boot.

Status: implemented. Decided in: ADR 0002, ADR 0017.

Requirement: Basalt OS MUST boot through the UEFI Secure Boot chain (firmware, shim, GRUB, kernel) with every stage signature-checked, and MUST NOT ship an unsigned boot component. The installer and the boot test MUST run with Secure Boot enabled.

Rationale: Stops boot kits and tampered kernels from running before the operating system's own protections start, and is the root the disk unlock (PCR 7) depends on.

Implemented in: `docs/secure-boot.md`, `scripts/ci/boot-test.sh`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `result PASS "secure boot"`
- Manual: audit guide, [BSC-BOOT-001](audit-guide.md#bsc-boot-001)

References: CIS: secure boot settings.

### BSC-BOOT-002

Kernel lockdown and module signature enforcement on by default.

Status: implemented. Decided in: ADR 0017.

Requirement: Installs MUST put lockdown=integrity and module.sig_enforce=1 on the kernel command line unless the administrator explicitly opts out, and the installer MUST warn when they are left off.

Rationale: A machine booted with Secure Boot off still refuses unsigned kernel modules, writes to /dev/mem, unsigned kexec images and hibernation to an unverified image.

Implemented in: `kickstart/basalt-server.ks`, `packages/basalt-installer/internal/plan/validate.go`.

Verified by:

- Test: `packages/basalt-installer/internal/steps/testdata/preview-default.txt`, `lockdown=integrity module.sig_enforce=1`
- Test: `packages/basalt-installer/internal/plan/validate.go`, `kernel lockdown and module signature enforcement are left off`
- Manual: audit guide, [BSC-BOOT-002](audit-guide.md#bsc-boot-002)

### BSC-BOOT-003

Basalt kernel modules signed under the Basalt module CA.

Status: implemented. Decided in: ADR 0017.

Requirement: Every kernel module Basalt builds MUST be signed with a certificate issued by the Basalt kernel module CA. The CA MUST reach the kernel only through a MOK enrollment confirmed by a person at the console. Signing certificates MUST be loaded at boot only when the enrolled CA issued them.

Rationale: Lets Basalt ship drivers that load with Secure Boot and lockdown on, without asking people to turn either off, and keeps the physical presence step that stops remote software from adding trusted keys.

Implemented in: `packages/basalt-security/basalt-module-keys.service`, `packages/basalt-security/basalt-secureboot`, `scripts/release/sign-modules.sh`.

Verified by:

- Lab: `scripts/lab/mok-test.sh`, `mokutil`
- Test: `packages/nvidia/tests/basalt-nvidia-test.sh`, `sig`
- Manual: audit guide, [BSC-BOOT-003](audit-guide.md#bsc-boot-003)

### BSC-BOOT-004

Optional custom Secure Boot keys (custom db mode).

Status: partial. Decided in: ADR 0002.

Requirement: Basalt OS MUST offer a custom db mode in which the firmware trusts only binaries Basalt signed, with a Basalt-signed shim shipped as a package and a documented path back to stock keys.

Rationale: Narrows what can boot on a machine (other vendors' shims, old vulnerable boot loaders) and therefore what can reproduce PCR 7 to unlock the disk.

Implemented in: `docs/secure-boot.md`, `scripts/lab/sb-custom-test.sh`.

Verified by:

- Lab: `scripts/lab/sb-custom-test.sh`, `negative control`

Gap: The procedure is tested in the lab with development keys. Release PK, KEK and db keys exist, but no Basalt-signed shim package is published and custom db mode is not offered outside the lab yet.

## PKG: Packages, repositories and the build and release pipeline

### BSC-PKG-001

Signed packages and signed repository metadata.

Status: implemented. Decided in: ADR 0005.

Requirement: Every Basalt repository definition MUST set gpgcheck=1 and repo_gpgcheck=1 with the OpenBasalt release key shipped in the basalt-release package, enforced by a vendor override that an edited or older repository file cannot weaken; dnf MUST refuse tampered metadata and packages signed by another key.

Rationale: Makes the hosting location irrelevant to integrity: a compromised CDN, mirror or bucket cannot serve modified packages.

Implemented in: `packages/basalt-release/basalt.repo`, `packages/basalt-release/20-basalt-signatures.repo`, `packages/basalt-nonfree-release/20-basalt-nonfree-signatures.repo`, `packages/basalt-release/RPM-GPG-KEY-basalt`.

Verified by:

- Test: `scripts/release/client-test.sh`, `negative: a tampered repomd.xml must be refused`
- Test: `scripts/release/client-test.sh`, `negative: another key must be refused`
- Manual: audit guide, [BSC-PKG-001](audit-guide.md#bsc-pkg-001)

References: NIST SSDF PS.2; OWASP LLM03 Supply Chain.

### BSC-PKG-002

Signing isolated from building.

Status: implemented. Decided in: ADR 0005.

Requirement: Packages MUST be built on a host that holds no keys and signed on a release signer in a container without network, from a subkey-only export (the secret primary key MUST be refused), with key material only on a tmpfs and shredded after use, and the result MUST be verified with the public key alone in a second container.

Rationale: A compromised build host or CI runner cannot sign; a mistake in the signer cannot leak the primary key.

Implemented in: `scripts/release/build.sh`, `scripts/release/sign.sh`, `scripts/release/sign-apt.sh`.

Verified by:

- Test: `scripts/release/sign.sh`, `the export contains the secret PRIMARY key`
- Test: `scripts/release/sign.sh`, `--network none`
- Manual: audit guide, [BSC-PKG-002](audit-guide.md#bsc-pkg-002)

References: NIST SSDF PS.1; NIST SSDF PS.2.

### BSC-PKG-003

Release builds refuse lab overrides.

Status: implemented. Decided in: ADR 0005.

Requirement: A release build MUST refuse development repository keys, lab module certificates, lab repository URLs and lab knowledge manifests, and MUST record SHA256SUMS and build information for what it produced.

Rationale: A lab key or URL can never reach a published package by accident.

Implemented in: `scripts/release/build.sh`.

Verified by:

- Test: `scripts/release/build.sh`, `are refused`
- Manual: audit guide, [BSC-PKG-003](audit-guide.md#bsc-pkg-003)

### BSC-PKG-004

Published artifacts are immutable.

Status: implemented. Decided in: ADR 0005.

Requirement: A published package or repository blob MUST never be replaced by different content; metadata MUST be uploaded after the files it points to; a tree signed with a test key MUST be refused for the public bucket.

Rationale: Prevents silent substitution of a released file and clients seeing metadata that points at missing files.

Implemented in: `scripts/release/upload.sh`, `scripts/release/merge-published.sh`.

Verified by:

- Test: `scripts/release/upload.sh`, `Immutable objects: refuse to replace a different RPM or blob`
- Manual: audit guide, [BSC-PKG-004](audit-guide.md#bsc-pkg-004)

References: NIST SSDF PS.3.

### BSC-PKG-005

Images contain only release-signed packages.

Status: implemented. Decided in: ADR 0005.

Requirement: Installer and live images built from the published repositories MUST verify the metadata signature and every package checksum against the signed metadata before using a package.

Rationale: The installation media inherit the repository's integrity.

Implemented in: `scripts/release/mirror-published.sh`.

Verified by:

- Test: `scripts/release/mirror-published.sh`, `repomd`
- Manual: audit guide, [BSC-PKG-005](audit-guide.md#bsc-pkg-005)

### BSC-PKG-006

Signed installer image checksums.

Status: planned. Decided in: ADR 0005.

Requirement: Every published installer image MUST come with a checksum file signed by the release key, and the download page MUST explain how to check it.

Rationale: People can verify the medium before they boot it; the ISO is the one artifact not covered by dnf's checks.

Gap: No installer image is published with a signed checksum yet; the release workflow that would do it is a disabled skeleton.

### BSC-PKG-007

Separate signing subkey per repository family.

Status: planned. Decided in: ADR 0005.

Requirement: The basalt-tools and third-party repositories MUST be signed with a subkey different from the basalt repository's, so that the compromise of one does not affect the other, and a key change in a third-party repository MUST need confirmation.

Rationale: Limits the blast radius of a signing key compromise.

Gap: Today one packages signing subkey signs every repository (basalt, basalt-tools, basalt-testing, basalt-nonfree, apt). This is a recorded deviation from the decision.

### BSC-PKG-008

CI holds no secrets and pins its actions.

Status: implemented. Decided in: ADR 0005.

Requirement: Continuous integration MUST run with read-only repository permissions, MUST NOT hold signing keys or publish credentials, and MUST pin third-party actions by commit SHA.

Rationale: A compromised workflow or dependency cannot sign or publish.

Implemented in: `.github/workflows/ci.yml`, `.github/workflows/release.yml`.

Verified by:

- CI: `.github/workflows/ci.yml`, `contents: read`
- Manual: audit guide, [BSC-PKG-008](audit-guide.md#bsc-pkg-008)

References: NIST SSDF PO.3; NIST SSDF PS.1.

## DISK: Disk encryption and unlock

### BSC-DISK-001

Full disk encryption on by default.

Status: implemented. Decided in: ADR 0002, ADR 0006.

Requirement: The installer MUST encrypt the system with LUKS2 by default. Turning encryption off, or limiting it to /home, MUST be an explicit choice that states in plain language what stays unencrypted.

Rationale: Protects data at rest against a lost or stolen machine or disk.

Implemented in: `kickstart/basalt-server.ks`, `packages/basalt-installer/internal/plan/validate.go`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"luks key slots"`
- Manual: audit guide, [BSC-DISK-001](audit-guide.md#bsc-disk-001)

References: CIS: filesystem encryption.

### BSC-DISK-002

Recovery key always generated, installer secret removed.

Status: implemented. Decided in: ADR 0002.

Requirement: Every encrypted install MUST enroll a recovery key shown to the person at install time, and MUST remove the temporary passphrase the installer used, leaving only the unlock method and the recovery key.

Rationale: The machine stays recoverable when the automatic unlock fails, and no installer secret remains on disk.

Implemented in: `kickstart/basalt-server.ks`, `packages/basalt-installer`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `*recovery*`
- Lab: `scripts/lab/sb-test.sh`, `recovery`
- Manual: audit guide, [BSC-DISK-002](audit-guide.md#bsc-disk-002)

### BSC-DISK-003

TPM unlock bound to the Secure Boot state (PCR 7).

Status: implemented. Decided in: ADR 0002.

Requirement: Unattended TPM2 unlock MUST be sealed to PCR 7, so a changed Secure Boot state (keys, dbx, Secure Boot off) stops the automatic unlock and asks for the recovery key. Planned changes MUST go through basalt-tpm suspend, which is limited to one boot.

Rationale: A tampered boot chain cannot unlock the disk; routine kernel updates and rollbacks still unlock unattended.

Implemented in: `packages/basalt-security/tpm.conf`, `packages/basalt-security/basalt-tpm`, `packages/basalt-security/basalt-tpm-resume.service`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"tpm2 policy"`
- Lab: `scripts/lab/sb-test.sh`, `recovery`
- Manual: audit guide, [BSC-DISK-003](audit-guide.md#bsc-disk-003)

Gap: PCR 7 does not say which signed kernel ran: with stock keys any Fedora-signed chain reproduces it. Custom db mode or Tang narrow this.

### BSC-DISK-004

Network-bound unlock with a pinned Tang key.

Status: implemented. Decided in: ADR 0002.

Requirement: The tang and tpm2+tang unlock methods MUST pin the Tang server's key thumbprint when given and MUST warn and log when the key is trusted on first use.

Rationale: A disk that must open only on the site network cannot be opened by a look-alike Tang server.

Implemented in: `kickstart/basalt-server.ks`.

Verified by:

- Lab: `scripts/lab/tang-test.sh`, `tang`
- Test: `kickstart/basalt-server.ks`, `Tang key trusted on first use`
- Manual: audit guide, [BSC-DISK-004](audit-guide.md#bsc-disk-004)

### BSC-DISK-005

No swap on disk.

Status: implemented. Decided in: ADR 0002.

Requirement: Swap MUST be zram only, so memory is never written to disk unencrypted, including on installs that encrypt only /home.

Rationale: Keeps secrets in memory (keys, passwords, documents) off the disk.

Implemented in: `kickstart/basalt-server.ks`.

Verified by:

- Test: `kickstart/basalt-server.ks`, `zram-generator`
- Manual: audit guide, [BSC-DISK-005](audit-guide.md#bsc-disk-005)

## MAC: Mandatory access control (SELinux) and service hardening

### BSC-MAC-001

SELinux enforcing from the first boot.

Status: implemented. Decided in: ADR 0009.

Requirement: SELinux MUST be enforcing with the targeted policy on every install, and turning it to permissive or off MUST NOT be offered as a fix by any Basalt tool.

Rationale: Mandatory access control is the boundary that confines services, agents and the assistant even when they are compromised.

Implemented in: `kickstart/basalt-server.ks`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"$tag selinux"`
- Manual: audit guide, [BSC-MAC-001](audit-guide.md#bsc-mac-001)

References: CIS: SELinux enforcing.

### BSC-MAC-002

Every Basalt service in its own SELinux domain.

Status: implemented. Decided in: ADR 0009, ADR 0010.

Requirement: Each Basalt daemon (assistant, ledger, resolver, local model server, agent proxy) MUST run in a dedicated SELinux domain with only the access it needs, shipped as a -selinux package.

Rationale: A bug in one service is contained to what its domain allows.

Implemented in: `packages/basalt-assistant/selinux/basalt_assistant.te`, `packages/basalt-ledger/selinux/basalt_ledger.te`, `packages/basalt-resolver/selinux/basalt_resolver.te`, `packages/basalt-llm/selinux/basalt_llm.te`, `packages/basalt-agent/selinux/basalt_agent.te`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"assistant daemon confined"`
- CI: `scripts/ci/vm-test.sh`, `"resolver and ledger confined"`
- Manual: audit guide, [BSC-MAC-002](audit-guide.md#bsc-mac-002)

### BSC-MAC-003

Zero SELinux denials in normal operation.

Status: implemented. Decided in: ADR 0009.

Requirement: A fresh install doing its normal work (boot, updates, services, allowed agent work) MUST produce zero SELinux denials, so every denial is a signal worth reading.

Rationale: Noise trains people to ignore or disable protection.

Implemented in: `scripts/ci/vm-test.sh`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"$tag avc denials"`
- Lab: `scripts/lab/agent-test.sh`, `none during allowed work`
- Manual: audit guide, [BSC-MAC-003](audit-guide.md#bsc-mac-003)

### BSC-MAC-004

Denials are never turned into policy automatically.

Status: implemented. Decided in: ADR 0004, ADR 0009.

Requirement: No Basalt tool MAY generate policy from denials (no audit2allow action); a denial without a known fix MUST be reported for review, and denials of Basalt's own domains MUST be treated as bugs, never fixed by widening access on the machine.

Rationale: Automatic policy widening is how attackers turn a blocked attempt into an allowed one.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestSuspiciousIsNeverFixed(`
- Test: `packages/basalt-assistant`, `func TestAnalyzePathNeverRelabelsShadow(`
- Manual: audit guide, [BSC-MAC-004](audit-guide.md#bsc-mac-004)

### BSC-MAC-005

Agent family neverallow rules.

Status: implemented. Decided in: ADR 0009.

Requirement: The SELinux base module of the agent family MUST forbid, for every agent domain present or future: reading SSH, GnuPG and certificate keys, shadow, the agent secret store and generic home content; running sudo; loading policy or changing enforcement and booleans; and the setuid, sys_admin, sys_ptrace and DAC override capabilities.

Rationale: A later policy change cannot quietly give an agent credentials or privilege, because the build refuses it.

Implemented in: `packages/basalt-agent/selinux/basalt_agent_base.te`, `packages/basalt-agent/selinux/basalt_agent.te`.

Verified by:

- Test: `packages/basalt-agent/selinux/basalt_agent_base.te`, `neverallow basalt_agent_domain security_t:security { load_policy setenforce setbool };`
- Lab: `packages/basalt-agent/tests/driver.sh`, `selinux`
- Manual: audit guide, [BSC-MAC-005](audit-guide.md#bsc-mac-005)

Gap: Fedora's policy store does not check neverallow rules at install time (expand-check=0), so the rules hold at build time and are checked on a running system by behaviour tests and sesearch.

### BSC-MAC-006

systemd hardening for Basalt services.

Status: implemented. Decided in: ADR 0009, ADR 0010.

Requirement: Basalt system services MUST run with NoNewPrivileges, ProtectSystem=strict, a private /tmp, home directories hidden or read-only, the smallest capability bounding set they need, and no network where they need none.

Rationale: A second fence, independent of SELinux, around each service.

Implemented in: `packages/basalt-ledger/dist/basalt-ledger.service`, `packages/basalt-resolver/dist/basalt-resolver.service`, `packages/basalt-assistant/dist/basalt-assistantd.service`, `packages/basalt-llm/basalt-llm.service`, `packages/basalt-models/basalt-models-fetch@.service`.

Verified by:

- Test: `packages/basalt-ledger/dist/basalt-ledger.service`, `PrivateNetwork=yes`
- Test: `packages/basalt-llm/basalt-llm.service`, `IPAddressDeny=any`
- Manual: audit guide, [BSC-MAC-006](audit-guide.md#bsc-mac-006)

References: CIS: service hardening.

### BSC-MAC-007

Confined user roles for the default user.

Status: planned. Decided in: ADR 0009.

Requirement: The default desktop user SHOULD run in a confined SELinux user (user_u or staff_u) so that code the person runs is not unconfined_t.

Rationale: Today an unconfined process of the same user is outside SELinux's control and could, for example, drive the shell UI.

Gap: Not evaluated yet; users run unconfined_t as on Fedora.

### BSC-MAC-008

Confinement for user applications.

Status: planned. Decided in: ADR 0009.

Requirement: Graphical applications SHOULD run through Flatpak (namespaces and portals) under SELinux, and command-line or daemon applications without upstream policy SHOULD get Basalt policy modules over time.

Rationale: Extends "every app has its confinement" beyond AI agents.

Gap: Only Basalt's own services, agents and desktop workers are confined today.

### BSC-MAC-009

Basalt Shield, protections people can understand.

Status: planned. Decided in: ADR 0022.

Requirement: Basalt OS MUST give people a plain-language view of their protections (blocked attempts translated from SELinux denials, the state of each protection, recent changes from the ledger) in which every change is an approval gate request and the locked list stays locked.

Rationale: A protection people cannot understand gets switched off.

Gap: Name decided; the tool is scheduled after phase 2 of the approval gate.

## GATE: Approvals, confirmations and the approval gate

### BSC-GATE-001

Agents request, only a trusted UI confirms.

Status: implemented. Decided in: ADR 0009, ADR 0020.

Requirement: A confirmation of a desktop proposal MUST be accepted only from the shell UI's SELinux domain, identified by the kernel (SO_PEERCRED and SO_PEERSEC), never by what the client claims; agent domains MUST NOT be able to enter, trace or connect to that domain.

Rationale: A prompt-injected agent cannot approve its own request.

Implemented in: `internal/shell/peer.go` in basalt-shell, `selinux/basalt_shell.te` in basalt-shell, `internal/shell/gate.go` in basalt-shell, `packages/basalt-gate/internal/server/decide.go`.

Verified by:

- Test: `internal/shell/peer_test.go` in basalt-shell, `func TestUICheck(`
- Test: `internal/shell/gate_test.go` in basalt-shell, `func TestGateDecidesAnAgentsRequest(`
- Test: `packages/basalt-gate/internal/server/server_test.go`, `func TestOnlyDecidersDecide(`
- Lab: `lab/demo/security-test.sh` in basalt-shell, `ui`
- Lab: `packages/basalt-agent/tests/attacks.sh`, `shell-socket-confirm`
- Audit suite: `cases/04-confirmation-boundary/probe.sh` in ai-audit-suite, `confirm.shell-socket`
- Manual: audit guide, [BSC-GATE-001](audit-guide.md#bsc-gate-001)

References: OWASP LLM06 Excessive Agency.

### BSC-GATE-002

System changes are typed actions from a closed set.

Status: implemented. Decided in: ADR 0004, ADR 0020.

Requirement: The system assistant MUST express every change as an action from a closed set with strict validators, rebuild the exact command lines from validated parameters every time they are shown or run, and offer no free-form command action.

Rationale: Neither a model nor a forged proposal file can express an arbitrary command.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestCommandsAreExact(`
- Test: `packages/basalt-assistant`, `func TestValidationRejects(`
- Manual: audit guide, [BSC-GATE-002](audit-guide.md#bsc-gate-002)

References: OWASP LLM05 Improper Output Handling; OWASP LLM06 Excessive Agency.

### BSC-GATE-003

A confirmation binds to exactly what was shown.

Status: implemented. Decided in: ADR 0004, ADR 0020.

Requirement: A non-interactive confirmation MUST carry a code derived from the SHA-256 of the proposal and its command lines, so a changed proposal needs a new confirmation; the code MUST never be returned to an MCP client or any other requester.

Rationale: Approve A, run B (replay or swap) is impossible, and an agent never holds a token that confirms.

Implemented in: `packages/basalt-assistant/internal`, `packages/basalt-gate/internal/server/migrate.go`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestFingerprintBindsCommands(`
- Test: `packages/basalt-assistant`, `func TestProtocolAndProposeOnly(`
- Test: `packages/basalt-assistant/internal/gatelink/gatelink_test.go`, `func TestShortCodeIsTheFingerprint(`
- Test: `packages/basalt-gate/internal/server/migrate_test.go`, `func TestRootConfirmsWithTheCode(`
- Manual: audit guide, [BSC-GATE-003](audit-guide.md#bsc-gate-003)

### BSC-GATE-004

Preview, snapshot, verification and audit for every applied change.

Status: implemented. Decided in: ADR 0004, ADR 0006.

Requirement: Applying a system proposal MUST show the full report and exact commands, take a snapshot before and after, run the action's verification, and write an audit record with every output.

Rationale: Every change can be explained and undone.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Lab: `scripts/lab/assistant-test.sh`, `apply`
- Manual: audit guide, [BSC-GATE-004](audit-guide.md#bsc-gate-004)

### BSC-GATE-005

Person-only actions refuse agents.

Status: implemented. Decided in: ADR 0012, ADR 0020.

Requirement: Actions that act in the person's name (inserting text, sending mail, moving files, voice answer settings, power from the command bar) MUST be planned only from the person's own words, and an agent connection MUST NOT be able to propose them.

Rationale: Content and agents cannot make the machine speak or act for the person.

Implemented in: `internal/shell/acting.go` in basalt-shell, `packages/basalt-gate/actions.d/shell.json`.

Verified by:

- Test: `internal/shell/acting_test.go` in basalt-shell, `func TestPersonActionsRefuseAgents(`
- Test: `packages/basalt-gate/internal/server/server_test.go`, `func TestPersonOnlyAndRelays(`
- Test: `packages/basalt-gate/internal/server/migrate_test.go`, `func TestPersonOwnPowerInTheSession(`
- Manual: audit guide, [BSC-GATE-005](audit-guide.md#bsc-gate-005)

### BSC-GATE-006

Confirmation sheets resist spoofing.

Status: implemented. Decided in: ADR 0012, ADR 0020.

Requirement: Confirmation surfaces MUST ignore input for a short arming delay after a request appears, MUST refuse a confirmation that follows synthetic input too closely, MUST show one request at a time, and MUST render all content as plain text.

Rationale: An agent with screen control cannot click or type its own Confirm, and content cannot draw a fake button or link.

Implemented in: `internal/shell/agent.go` in basalt-shell.

Verified by:

- Test: `internal/shell/agent_test.go` in basalt-shell, `func TestCaptureNeedsConfirmationOutsideSession(`
- Manual: audit guide, [BSC-GATE-006](audit-guide.md#bsc-gate-006)

### BSC-GATE-007

One approval gate for every side effect.

Status: partial. Decided in: ADR 0020.

Requirement: Every request for a side effect (from the person by voice or command bar, the assistant, agents, apps, tools, schedules, the phone) MUST go through one local gate that validates it against an action registry, computes its risk class, applies hard limits and deterministic policy, queues what must be asked, and hands executors a single-use claim bound to the proposal digest.

Rationale: One format, one queue, one history and one stop button instead of many separate approval paths.

Implemented in: `packages/basalt-gate`, `packages/basalt-assistant/internal/gatelink/gatelink.go`.

Verified by:

- Test: `packages/basalt-gate/internal/server/server_test.go`, `func TestClaims(`
- Test: `packages/basalt-gate/internal/server/migrate_test.go`, `func TestObserve(`
- Test: `packages/basalt-gate/internal/server/migrate_test.go`, `func TestExecutorClaimWithOtherCalls(`
- Lab: `scripts/lab/gate-test.sh`, `gate`
- Manual: audit guide, [BSC-GATE-007](audit-guide.md#bsc-gate-007)

Gap: The gate is built (phase 1) and the approval paths move to it one by one (phase 2): each path decides through the gate only where /etc/basalt-gate/gate.conf enforces it, and keeps its own confirmation (reporting it to the gate) elsewhere. The gate is not in the default install yet; until a path is enforced, its own approval (GATE-001 to GATE-006, AGENT-008, AI-004) is the one that holds.

### BSC-GATE-008

Hard limits no rule can lift.

Status: planned. Decided in: ADR 0013, ADR 0020.

Requirement: A locked list (SELinux off, replacing Basalt policy, Secure Boot keys, disabling or rewriting the ledger, loosening the gate, removing disk encryption, wiping disks, bulk credential reads, making a user an administrator) MUST be refused by policy and unlockable only for one named request, at the local console, with an administrator password and a second factor.

Rationale: Automation and full AI mode can never disable the protections they run under.

Gap: Part of the gate (phase 1).

### BSC-GATE-009

Pre-approvals never apply to tainted requests.

Status: planned. Decided in: ADR 0019, ADR 0020.

Requirement: Allow rules MUST NOT match a request made after the session read web or personal content, scheduled rules MUST match only requests the gate itself started, and new destinations MUST never be pre-approved.

Rationale: Prompt injection cannot ride on a standing approval.

Gap: Part of the gate (phase 1); no pre-approval exists today, everything asks.

References: OWASP LLM01 Prompt Injection.

### BSC-GATE-010

Emergency stop for all automation.

Status: partial. Decided in: ADR 0012, ADR 0020.

Requirement: One action available from every surface, without authentication, MUST suspend every allow rule, pause schedules, end agent control and freeze or stop agent sessions; resuming MUST be confirmed on a trusted surface.

Rationale: The person can always stop the machine acting on its own.

Implemented in: `internal/shell/agent.go` in basalt-shell.

Verified by:

- Manual: audit guide, [BSC-GATE-010](audit-guide.md#bsc-gate-010)

Gap: Today the stop key and button end an agent's control session only. The full stop is part of the gate.

### BSC-GATE-011

Full AI mode is off, informed, scoped and reversible.

Status: planned. Decided in: ADR 0013, ADR 0020.

Requirement: A mode in which agents act without asking MUST be off by default, and turning it on MUST be a critical request with a risk screen, administrator authentication and typed confirmation, limited in scope and time, with a visible indicator and stop; the hard limits stay on.

Rationale: Autonomy is a choice the person makes knowingly, never a default.

Gap: The mode does not exist; nothing runs without confirmation today.

### BSC-GATE-012

Phone approvals end to end.

Status: planned. Decided in: ADR 0013, ADR 0020.

Requirement: Approvals from a phone MUST be signed by a key in the phone's hardware keystore over the request digest, requests MUST be encrypted to the phone, and any relay MUST see ciphertext only; locked items MUST NOT be approvable from the phone alone.

Rationale: A relay or push service cannot read or forge approvals.

Gap: Future work (gate phase 4).

## LEDGER: The audit ledger and audit logs

### BSC-LEDGER-001

One append-only, hash-chained audit trail.

Status: implemented. Decided in: ADR 0010.

Requirement: Security-relevant events (agent sessions, network decisions, SELinux denials, escalations, logins, snapshots and rollbacks, assistant decisions, driver and model changes) MUST be recorded in one ledger where each record carries the SHA-256 of the previous, with a chain that runs across rotated files.

Rationale: Editing, removing or reordering records breaks the chain and is detected.

Implemented in: `packages/basalt-ledger`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestChainAcrossRotations(`
- Test: `packages/basalt-ledger`, `func TestTamperDetected(`
- CI: `scripts/ci/vm-test.sh`, `basalt-ledger verify`
- Audit suite: `cases/03-ledger-integrity/probe.sh` in ai-audit-suite, `verify.intact`
- Manual: audit guide, [BSC-LEDGER-001](audit-guide.md#bsc-ledger-001)

References: CIS: audit logging.

### BSC-LEDGER-002

No operation rewrites history.

Status: implemented. Decided in: ADR 0010.

Requirement: The ledger MUST offer no operation that changes or removes a record; such requests MUST be refused and the refusal recorded.

Rationale: Clients, including compromised producers, can only add.

Implemented in: `packages/basalt-ledger/internal/server/server.go`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestNoRewrite(`
- Audit suite: `cases/03-ledger-integrity/probe.sh` in ai-audit-suite, `file.rewrite`
- Manual: audit guide, [BSC-LEDGER-002](audit-guide.md#bsc-ledger-002)

### BSC-LEDGER-003

Producers are identified by the kernel.

Status: implemented. Decided in: ADR 0010.

Requirement: Who may write MUST be decided from the peer's kernel identity (SO_PEERCRED, SO_PEERSEC): unprivileged users write only their own records under user producer names, agent and app domains MUST NOT write, network records MUST come only from root in the resolver's domain, and built-in collector names MUST NOT be accepted from the socket.

Rationale: Nobody can forge records that look like they came from another component.

Implemented in: `packages/basalt-ledger/internal/server/server.go`, `packages/basalt-ledger/selinux/basalt_ledger.te`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestProducerRules(`
- Audit suite: `cases/03-ledger-integrity/probe.sh` in ai-audit-suite, `forge.producer`
- Manual: audit guide, [BSC-LEDGER-003](audit-guide.md#bsc-ledger-003)

### BSC-LEDGER-004

Users read only their own records.

Status: implemented. Decided in: ADR 0010.

Requirement: A user MUST see only records of their own uid; reading everything MUST need administrator authentication; the files MUST be root-only.

Rationale: The audit trail does not leak one person's activity to another.

Implemented in: `packages/basalt-ledger/internal/server/server.go`, `packages/basalt-ledger/dist/org.basalt-os.ledger.policy`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestReadIsolation(`
- Manual: audit guide, [BSC-LEDGER-004](audit-guide.md#bsc-ledger-004)

### BSC-LEDGER-005

File attributes, sealed rotation and a separate subvolume.

Status: implemented. Decided in: ADR 0006, ADR 0010.

Requirement: The current ledger file MUST be append-only (chattr +a) and sealed files immutable (+i); rotation MUST append a seal with the file's SHA-256 and continue the chain in the next file; the logs MUST live on a subvolume outside the root snapshots so a rollback never rewinds them.

Rationale: Raises the bar for tampering by root and keeps history across rollbacks.

Implemented in: `packages/basalt-ledger`, `kickstart/basalt-server.ks`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestSealedFileTamperDetected(`
- CI: `scripts/ci/vm-test.sh`, `"audit sealed rotation"`
- Manual: audit guide, [BSC-LEDGER-005](audit-guide.md#bsc-ledger-005)

### BSC-LEDGER-006

Retention is recorded in the chain.

Status: implemented. Decided in: ADR 0010.

Requirement: Removing expired files MUST first append a retention record naming the file, its sequence range, its seal and its SHA-256; verify MUST accept a shortened chain only when such a record vouches for it.

Rationale: Old files can expire without opening a way to delete history silently.

Implemented in: `packages/basalt-ledger`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestRetentionRecordMustMatch(`
- Manual: audit guide, [BSC-LEDGER-006](audit-guide.md#bsc-ledger-006)

### BSC-LEDGER-007

Signed exports with a TPM-held key.

Status: implemented. Decided in: ADR 0010.

Requirement: Exports MUST be signed; on a machine with a TPM the key MUST be held by the TPM and never leave it; a software fallback MUST be labeled as such in every export and in status.

Rationale: An incident report can be checked anywhere for integrity and origin.

Implemented in: `packages/basalt-ledger`.

Verified by:

- Test: `packages/basalt-ledger`, `func TestTPMKey(`
- Test: `packages/basalt-ledger`, `func TestSoftwareKey(`
- CI: `scripts/ci/vm-test.sh`, `"ledger: TPM-signed export"`
- Audit suite: `cases/03-ledger-integrity/probe.sh` in ai-audit-suite, `export.tamper`
- Manual: audit guide, [BSC-LEDGER-007](audit-guide.md#bsc-ledger-007)

### BSC-LEDGER-008

Off-machine anchoring of the chain head.

Status: planned. Decided in: ADR 0010.

Requirement: The ledger SHOULD periodically anchor its chain head to an external immutable store, so that root rewriting the whole chain on the machine is detectable from outside.

Rationale: On the machine the ledger is tamper evident, not tamper proof: root can rewrite every file and recompute the chain.

Gap: Future work.

### BSC-LEDGER-009

Attestation that the export key is TPM-resident.

Status: planned. Decided in: ADR 0010.

Requirement: The export key SHOULD be certified by the TPM's attestation key chained to the manufacturer's endorsement certificate.

Rationale: Today the TPM key proves "signed on this machine" only to someone who already trusts its public key.

Gap: Future work.

### BSC-LEDGER-010

The assistant's own audit log is chained and sealed.

Status: implemented. Decided in: ADR 0004, ADR 0010.

Requirement: The system assistant MUST write every decision, proposal, confirmation, refusal and apply to a root-only, append-only, hash-chained log with sealed rotation, and to the journal for the ledger.

Rationale: What the assistant did is reviewable even without the ledger service.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestChainAppendVerifyTamper(`
- Test: `packages/basalt-assistant`, `func TestVerifyDetectsTamperingAcrossFiles(`
- CI: `scripts/ci/vm-test.sh`, `basalt audit verify`
- Manual: audit guide, [BSC-LEDGER-010](audit-guide.md#bsc-ledger-010)

## NET: Network egress and inbound access

### BSC-NET-001

Default-deny egress for agent sessions, by name.

Status: implemented. Decided in: ADR 0010.

Requirement: Every agent session MUST be default deny in the kernel (nftables matched on the session's cgroup), with addresses admitted only from DNS answers for names on the session's allowlist, on the ports the entry names; everything else MUST be dropped and recorded.

Rationale: A prompt-injected or buggy agent cannot reach arbitrary hosts, and a bug in the proxy cannot either.

Implemented in: `packages/basalt-resolver`, `packages/basalt-agent`.

Verified by:

- Test: `packages/basalt-resolver`, `func TestAllowedNameFillsSet(`
- Test: `packages/basalt-resolver`, `func TestUnlistedRefused(`
- Lab: `scripts/lab/ledger-test.sh`, `egress`
- Audit suite: `cases/02-network-egress/probe.sh` in ai-audit-suite, `egress.unlisted-direct`
- Manual: audit guide, [BSC-NET-001](audit-guide.md#bsc-net-001)

References: OWASP LLM02 Sensitive Information Disclosure; OWASP LLM06 Excessive Agency.

### BSC-NET-002

DNS rebinding and IP literals refused.

Status: implemented. Decided in: ADR 0010.

Requirement: An allowed name resolving to a loopback, private or link-local address MUST be refused unless its entry is marked private, and IP literals MUST NOT be accepted as destinations.

Rationale: An allowed name cannot be pointed at local services, and the allowlist cannot be bypassed by address.

Implemented in: `packages/basalt-resolver`, `packages/basalt-agent`.

Verified by:

- Test: `packages/basalt-resolver`, `func TestRebindingRefused(`
- Test: `packages/basalt-agent`, `func TestPrivateAddressesRefused(`
- Manual: audit guide, [BSC-NET-002](audit-guide.md#bsc-net-002)

Gap: The public audit suite's two rebinding cases need a lab name server and were not run in the October 2026 audit; the unit and lab tests cover it.

### BSC-NET-003

No data through DNS names.

Status: implemented. Decided in: ADR 0010.

Requirement: Names not on the allowlist MUST be refused before any upstream query, and DNS to any other server (ports 53, 853, 5353) MUST be dropped.

Rationale: A session cannot exfiltrate data as DNS labels it chooses.

Implemented in: `packages/basalt-resolver`.

Verified by:

- Test: `packages/basalt-resolver`, `func TestUnlistedRefused(`
- Audit suite: `cases/02-network-egress/probe.sh` in ai-audit-suite, `egress.dns-exfil`
- Manual: audit guide, [BSC-NET-003](audit-guide.md#bsc-net-003)

Gap: Wildcard entries still let a session choose labels under the allowed suffix.

### BSC-NET-004

Allowlists only grow through a person.

Status: implemented. Decided in: ADR 0010.

Requirement: Widening a session or a profile allowlist MUST be done by a person (administrator authentication for one session or for the system scope, preview and confirmation for a user profile) and recorded; the resolver MUST accept allowlist changes only from root.

Rationale: An agent cannot open its own network.

Implemented in: `packages/basalt-resolver`, `packages/basalt-agent/dist/org.basalt-os.agent.policy`, `packages/basalt-agent/internal/cli/gate.go`.

Verified by:

- Test: `packages/basalt-resolver`, `func TestAllowOnlyFromRoot(`
- Test: `packages/basalt-gate/internal/server/migrate_test.go`, `func TestAgentGrantRequests(`
- Lab: `packages/basalt-agent/tests/attacks.sh`, `self-grant`
- Manual: audit guide, [BSC-NET-004](audit-guide.md#bsc-net-004)

### BSC-NET-005

Egress fails closed.

Status: implemented. Decided in: ADR 0010.

Requirement: When the resolver stops, sessions MUST keep their rules and lose DNS (nothing opens up); native agent sessions MUST refuse to start without the resolver; a session whose rules cannot be installed MUST NOT start.

Rationale: Failure never widens access.

Implemented in: `packages/basalt-resolver`, `packages/basalt-agent`.

Verified by:

- Test: `packages/basalt-resolver`, `func TestRestoreAfterRestart(`
- Manual: audit guide, [BSC-NET-005](audit-guide.md#bsc-net-005)

### BSC-NET-006

Direct egress off by default.

Status: implemented. Decided in: ADR 0010.

Requirement: The SELinux boolean basalt_agent_direct_egress MUST default to off, so native agents reach the network only through the session proxy, with SELinux and the kernel sets both in front of every connection.

Rationale: Two independent boundaries instead of one.

Implemented in: `packages/basalt-agent/selinux/basalt_agent.te`.

Verified by:

- Test: `packages/basalt-agent/selinux/basalt_agent.te`, `basalt_agent_direct_egress`
- Manual: audit guide, [BSC-NET-006](audit-guide.md#bsc-net-006)

### BSC-NET-007

Proxy and kernel filter share one allowlist parser.

Status: implemented. Decided in: ADR 0010.

Requirement: The agent proxy and the resolver MUST use byte-identical allowlist parsing code, checked in CI.

Rationale: The two filters can never disagree about what an entry means.

Implemented in: `packages/basalt-agent/internal/allowlist/allowlist.go`, `packages/basalt-resolver/internal/allowlist/allowlist.go`.

Verified by:

- CI: `scripts/ci/lint.sh`, `Shared egress allowlist format`

### BSC-NET-008

Inbound closed except SSH on servers.

Status: implemented. Decided in: ADR 0010.

Requirement: The server edition MUST default to a firewall zone that allows SSH only.

Rationale: A fresh server exposes one service.

Implemented in: `packages/basalt-release/basalt.xml`, `packages/basalt-release/firewalld-basalt.conf`.

Verified by:

- Manual: audit guide, [BSC-NET-008](audit-guide.md#bsc-net-008)

References: CIS: host firewall.

### BSC-NET-009

SSH keys only.

Status: implemented. Decided in: ADR 0006.

Requirement: sshd MUST accept public keys only (no passwords, no keyboard-interactive, no empty passwords), root MUST NOT log in with a password, and agent and X11 forwarding, tunnels and user environment MUST be off.

Rationale: Removes password guessing and reduces what a stolen session can reach.

Implemented in: `packages/basalt-release/10-basalt-hardening.conf`.

Verified by:

- Test: `packages/basalt-release/10-basalt-hardening.conf`, `AuthenticationMethods publickey`
- Manual: audit guide, [BSC-NET-009](audit-guide.md#bsc-net-009)

References: CIS: SSH server configuration.

### BSC-NET-010

Per-process network accounting.

Status: planned. Decided in: ADR 0010.

Requirement: Network decisions inside a session SHOULD be attributed to the program that made them (eBPF accounting).

Rationale: Richer audit: which tool in a session opened what.

Gap: Future work.

## AGENT: Confinement of AI agents

### BSC-AGENT-001

Agents see only their project.

Status: implemented. Decided in: ADR 0009.

Requirement: An AI agent run through basalt-agent MUST get read-write access to its project directory and its own configuration home only, and MUST NOT read SSH or GnuPG keys, keyrings, browser profiles, other projects or other users' files, or write anywhere else in the home directory.

Rationale: A prompt-injected agent cannot steal credentials or plant persistence such as a shell rc change.

Implemented in: `packages/basalt-agent`.

Verified by:

- Lab: `packages/basalt-agent/tests/attacks.sh`, `read-ssh-key`
- Lab: `packages/basalt-agent/tests/attacks.sh`, `append-bashrc`
- Audit suite: `cases/01-agent-confinement/probe.sh` in ai-audit-suite, `fs.read-ssh-key`
- Manual: audit guide, [BSC-AGENT-001](audit-guide.md#bsc-agent-001)

References: OWASP LLM06 Excessive Agency.

### BSC-AGENT-002

Sessions isolated from each other.

Status: implemented. Decided in: ADR 0009.

Requirement: Every agent session MUST get a unique SELinux MCS level, so one session cannot read another session's project, environment, secrets or control socket.

Rationale: Two agents, or two projects, do not leak into each other.

Implemented in: `packages/basalt-agent`.

Verified by:

- Test: `packages/basalt-agent`, `func TestLevels(`
- Lab: `packages/basalt-agent/tests/attacks.sh`, `victim-secret-file`
- Audit suite: `cases/01-agent-confinement/probe.sh` in ai-audit-suite, `session.victim-environ`
- Manual: audit guide, [BSC-AGENT-002](audit-guide.md#bsc-agent-002)

### BSC-AGENT-003

API keys never enter the agent session.

Status: implemented. Decided in: ADR 0009, ADR 0011.

Requirement: Provider keys MUST be held only by the launcher and the session proxy; the agent MUST get a placeholder; the proxy MUST add a key only to requests for that key's provider host over verified TLS and strip credential headers from every other request.

Rationale: A prompt-injected agent can spend through its provider but cannot copy a key out to another host.

Implemented in: `packages/basalt-agent`.

Verified by:

- Test: `packages/basalt-agent`, `func TestRouteInjectsOnlyOnItsHost(`
- Test: `packages/basalt-agent`, `func TestRouteNeedsAllowlistAndTrustedTLS(`
- Lab: `packages/basalt-agent/tests/credattacks.sh`, `secret-store-read`
- Manual: audit guide, [BSC-AGENT-003](audit-guide.md#bsc-agent-003)

References: OWASP LLM02 Sensitive Information Disclosure.

### BSC-AGENT-004

The secret store and the proxy are out of agents' reach.

Status: implemented. Decided in: ADR 0009.

Requirement: The key store MUST carry its own SELinux type that no agent domain may open, read or list (neverallow), and no agent domain may read, trace or signal the session proxy.

Rationale: A later policy change cannot open the key store to agents.

Implemented in: `packages/basalt-agent/selinux/basalt_agent_base.te`, `packages/basalt-agent/selinux/basalt_agent.te`.

Verified by:

- Test: `packages/basalt-agent/selinux/basalt_agent_base.te`, `neverallow basalt_agent_domain basalt_agent_secret_t:file`
- Lab: `packages/basalt-agent/tests/credattacks.sh`, `secret-store-list`
- Manual: audit guide, [BSC-AGENT-004](audit-guide.md#bsc-agent-004)

### BSC-AGENT-005

No privilege escalation from a session.

Status: implemented. Decided in: ADR 0009.

Requirement: Agent sessions MUST run with no_new_privs and MUST NOT run sudo, su or pkexec, gain capabilities, trace or signal the launcher, inject keystrokes into the terminal (TIOCSTI) or reach the container engine.

Rationale: An agent cannot become the person, or root.

Implemented in: `packages/basalt-agent`, `packages/basalt-agent/selinux/basalt_agent_base.te`.

Verified by:

- Lab: `packages/basalt-agent/tests/attacks.sh`, `tiocsti`
- Audit suite: `cases/01-agent-confinement/probe.sh` in ai-audit-suite, `priv.sudo`
- Manual: audit guide, [BSC-AGENT-005](audit-guide.md#bsc-agent-005)

### BSC-AGENT-006

Git hooks and configuration are read-only to agents.

Status: implemented. Decided in: ADR 0009.

Requirement: Inside a project, .git/hooks and .git/config MUST be read-only to the agent.

Rationale: An agent cannot plant a hook that later runs with the person's rights.

Implemented in: `packages/basalt-agent`.

Verified by:

- Lab: `packages/basalt-agent/tests/attacks.sh`, `write-git-hook`
- Manual: audit guide, [BSC-AGENT-006](audit-guide.md#bsc-agent-006)

### BSC-AGENT-007

Shipped profiles keep egress short.

Status: implemented. Decided in: ADR 0009, ADR 0011.

Requirement: Shipped profiles for common agents MUST allow only their provider, login hosts and the shared package registry list; GitHub MUST be opt-in per profile.

Rationale: The secure path is the easy path, and an allowed host that accepts uploads (GitHub) is a deliberate choice.

Implemented in: `packages/basalt-agent/dist`.

Verified by:

- Test: `packages/basalt-agent`, `func TestShippedProfiles(`
- Manual: audit guide, [BSC-AGENT-007](audit-guide.md#bsc-agent-007)

Gap: A profile that allows a package registry also allows publishing to it (accepted risk).

### BSC-AGENT-008

Desktop agents reach the desktop only through the shell daemon.

Status: implemented. Decided in: ADR 0012.

Requirement: The shell's agent domain MUST NOT reach the Wayland, X11, D-Bus session or PipeWire sockets, home files or the network; screenshots and input MUST exist only through the shell daemon, inside a control session the person confirmed, with a visible frame and a stop key.

Rationale: An agent cannot capture the screen or type unseen.

Implemented in: `selinux/basalt_shell.te` in basalt-shell, `internal/shell/agent.go` in basalt-shell.

Verified by:

- Lab: `lab/demo/security-test.sh` in basalt-shell, `Wayland`
- Manual: audit guide, [BSC-AGENT-008](audit-guide.md#bsc-agent-008)

### BSC-AGENT-009

Voice and skill workers run their own tools only.

Status: implemented. Decided in: ADR 0012.

Requirement: The voice service and the skill workers MUST run in their own SELinux domains that may run only their own tools (no shell, interpreter, setuid helper or self-written program), with no_new_privs and network only through a per-job allowlist.

Rationale: Content a worker reads cannot turn it into a general program runner.

Implemented in: `selinux/basalt_shell.te` in basalt-shell.

Verified by:

- Lab: `lab/voice/tests/escape-test.sh` in basalt-shell, `basalt_skill`
- Manual: audit guide, [BSC-AGENT-009](audit-guide.md#bsc-agent-009)

## AI: Models, knowledge, the assistant and content handling

### BSC-AI-001

Knowledge is used only when signed.

Status: implemented. Decided in: ADR 0004, ADR 0007.

Requirement: The assistant MUST use a knowledge index only when its manifest carries a valid signature by the knowledge subkey of the pinned OpenBasalt release key and every file matches the manifest; anything else MUST fall back to the built-in rules.

Rationale: Tampered or poisoned knowledge cannot steer diagnoses.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestVSMKnowledgeSignatureFailsClosed(`
- Test: `packages/basalt-assistant`, `func TestReleaseKeyBindsKnowledgeSubkey(`
- Test: `packages/basalt-assistant`, `func TestSignatureRefusals(`
- Manual: audit guide, [BSC-AI-001](audit-guide.md#bsc-ai-001)

References: OWASP LLM04 Data and Model Poisoning; OWASP LLM03 Supply Chain.

### BSC-AI-002

Model files pinned and checksummed.

Status: implemented. Decided in: ADR 0004, ADR 0011.

Requirement: Every model the system downloads (local language model, speech models) MUST come from a manifest pinned to a publisher revision, over HTTPS only, and MUST be installed only after its SHA-256 matches; unpublished models MUST fail closed.

Rationale: A compromised or changed upstream file is refused.

Implemented in: `packages/basalt-llm`, `packages/basalt-voice`, `packages/basalt-models`.

Verified by:

- Test: `packages/basalt-models/tests/models-test.sh`, `checksum`
- Test: `packages/basalt-voice/tests/manifest-test.sh`, `sha256`
- Test: `packages/basalt-llm/tests/select-test.sh`, `unpublished`
- Manual: audit guide, [BSC-AI-002](audit-guide.md#bsc-ai-002)

References: OWASP LLM03 Supply Chain.

### BSC-AI-003

Model downloads need the person's consent and policy.

Status: implemented. Decided in: ADR 0011, ADR 0012.

Requirement: The desktop MUST download a model only after the person chooses Download in the shell UI, under the administrator's policy (everyone, administrators, nobody), through a confined download service, with every step recorded.

Rationale: Nothing large or new arrives without a person's choice; agents cannot start downloads.

Implemented in: `packages/basalt-models`, `packages/basalt-gate/actions.d/models.json`.

Verified by:

- Test: `packages/basalt-models/tests/models-test.sh`, `nobody`
- Test: `packages/basalt-gate/internal/server/migrate_test.go`, `func TestModelDownloadConsent(`
- Manual: audit guide, [BSC-AI-003](audit-guide.md#bsc-ai-003)

### BSC-AI-004

The model proposes, it never executes.

Status: implemented. Decided in: ADR 0004, ADR 0011, ADR 0019.

Requirement: No language model in Basalt OS MAY execute an action: it may only file a typed proposal that a person confirms; the deterministic validators and confirmation MUST stay in force whatever model is plugged in.

Rationale: A better or remote model gains intelligence, not power.

Implemented in: `packages/basalt-assistant/internal`, `internal/shell/core.go` in basalt-shell.

Verified by:

- Test: `packages/basalt-assistant`, `func TestProtocolAndProposeOnly(`
- Test: `packages/basalt-assistant`, `func TestConfidentAnswerWithoutAFixProposesNothing(`
- Manual: audit guide, [BSC-AI-004](audit-guide.md#bsc-ai-004)

References: OWASP LLM06 Excessive Agency.

### BSC-AI-005

Content is data (prompt injection guard).

Status: partial. Decided in: ADR 0012, ADR 0019.

Requirement: Text read from mail, web pages and files MUST be scanned, cleaned and fenced as untrusted data before any model sees it; the plan MUST come from the person's words, never from content; answers MUST be filtered to plain text without links, markup, code or addresses; findings MUST be shown to the person.

Rationale: Instructions hidden in content are not followed.

Implemented in: `internal/guard/guard.go` in basalt-shell.

Verified by:

- Test: `internal/guard/guard_test.go` in basalt-shell, `func TestScanInstructions(`
- Audit suite: `cases/05-prompt-injection` in ai-audit-suite, `no_action`
- Manual: audit guide, [BSC-AI-005](audit-guide.md#bsc-ai-005)

Gap: The guard and its unit tests exist, but the public suite's 121 live prompt-injection assertions have not run against a real assistant endpoint yet; only the corpus self-check runs.

References: OWASP LLM01 Prompt Injection; OWASP LLM05 Improper Output Handling.

### BSC-AI-006

Local model service has no network.

Status: implemented. Decided in: ADR 0004, ADR 0011.

Requirement: The local language model server MUST run in its own SELinux domain as a dynamic user with no network (PrivateNetwork, IPAddressDeny), no capabilities and a system call filter.

Rationale: A malicious model file or a server bug cannot send data out.

Implemented in: `packages/basalt-llm/basalt-llm.service`, `packages/basalt-llm/selinux/basalt_llm.te`.

Verified by:

- Test: `packages/basalt-llm/basalt-llm.service`, `PrivateNetwork=yes`
- Manual: audit guide, [BSC-AI-006](audit-guide.md#bsc-ai-006)

### BSC-AI-007

The assistant daemon is confined and offline.

Status: implemented. Decided in: ADR 0004.

Requirement: The assistant daemon and its MCP server MUST run in a confined domain that reads what diagnosis needs, writes only its own state, appends to its own log, has no network and no transitions out; what it cannot check it MUST NOT propose as an action.

Rationale: A compromised assistant cannot change the system or send data out.

Implemented in: `packages/basalt-assistant/selinux/basalt_assistant.te`, `packages/basalt-assistant/dist/basalt-assistantd.service`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestConfinedRestoreIsAHint(`
- CI: `scripts/ci/vm-test.sh`, `"assistant daemon confined"`
- Manual: audit guide, [BSC-AI-007](audit-guide.md#bsc-ai-007)

### BSC-AI-008

Evaluation gate for knowledge and models.

Status: partial. Decided in: ADR 0004, ADR 0007.

Requirement: Every knowledge or decision-model release MUST pass the shared evaluation suite (accuracy, calibration, no regression on lab cases, negative cases respected) before it is signed.

Rationale: Quality and safety regressions are caught before release.

Implemented in: `eval`.

Verified by:

- Test: `Makefile`, `eval-check:`
- Manual: audit guide, [BSC-AI-008](audit-guide.md#bsc-ai-008)

Gap: The suite and its checks exist and run by hand; they are not yet a required step of the knowledge release pipeline.

### BSC-AI-009

Knowledge packs and remote knowledge, permission first.

Status: planned. Decided in: ADR 0018.

Requirement: Optional knowledge packs and remote knowledge answers MUST be signed by the publisher and verified on the client, fetched only with the person's permission, and treated as untrusted content that can only lead to proposals.

Rationale: Remote knowledge cannot be forged by a server, a mirror or the network.

Gap: The open protocol and its conformance tests exist as a separate project; the Basalt client is not integrated.

### BSC-AI-010

Natural-language assistant safety.

Status: planned. Decided in: ADR 0019.

Requirement: An agent loop over tools MUST keep per-request budgets, track taint (system, web, personal) and refuse to send anything shaped like a secret, address or personal data off the machine after reading content, and record each step without the content read.

Rationale: A capable model with tools stays inside the same safety model.

Gap: Proposed; a prototype exists outside the main branch.

## PRIV: Privacy and data leaving the machine

### BSC-PRIV-001

Nothing leaves the machine without a person.

Status: implemented. Decided in: ADR 0004, ADR 0007.

Requirement: Data MUST leave the machine only when a person starts it; feedback MUST be opt-in for every part, show the exact payload, scrub personal data, and bind a non-interactive confirmation to the payload's hash.

Rationale: Reports and logs do not leak host names, user names, addresses or secrets.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestCodeBindsThePayload(`
- Test: `packages/basalt-assistant`, `func TestScrub(`
- Manual: audit guide, [BSC-PRIV-001](audit-guide.md#bsc-priv-001)

References: OWASP LLM02 Sensitive Information Disclosure.

### BSC-PRIV-002

Remote models are opt-in and redacted.

Status: partial. Decided in: ADR 0004, ADR 0011, ADR 0019.

Requirement: Sending content to a remote model MUST be off by default, allowed by the administrator, chosen by the person, shown before sending, and redacted of secrets; the built-in model MUST keep working offline.

Rationale: Using a cloud model is a visible, informed choice.

Implemented in: `packages/basalt-assistant/internal`.

Verified by:

- Test: `packages/basalt-assistant`, `func TestRemoteEndpointRefused(`
- Test: `packages/basalt-assistant`, `func TestRedact(`
- Manual: audit guide, [BSC-PRIV-002](audit-guide.md#bsc-priv-002)

Gap: Implemented and tested for the system assistant's explanation layer; the desktop's remote tier (what is sent, per-conversation permission) is part of the proposed natural-language assistant.

### BSC-PRIV-003

No telemetry.

Status: implemented. Decided in: ADR 0004.

Requirement: Basalt components MUST NOT send usage data, crash reports or identifiers on their own; the only reports are the ones a person sends.

Rationale: Privacy by default.

Implemented in: `packages/basalt-assistant/dist/basalt-assistantd.service`.

Verified by:

- Manual: audit guide, [BSC-PRIV-003](audit-guide.md#bsc-priv-003)

Gap: Fedora's own repository files keep countme=1 (an anonymous weekly counter Fedora uses to estimate users); Basalt does not change it yet and says so.

### BSC-PRIV-004

Secrets and content never in records.

Status: implemented. Decided in: ADR 0009, ADR 0010, ADR 0019.

Requirement: Audit records MUST name credentials (name, host, port), never hold their values, and MUST NOT hold the content an assistant read or the text of its answers.

Rationale: The audit trail is safe to export and share.

Implemented in: `packages/basalt-agent`.

Verified by:

- Lab: `packages/basalt-agent/tests/credattacks.sh`, `LAB`
- Manual: audit guide, [BSC-PRIV-004](audit-guide.md#bsc-priv-004)

### BSC-PRIV-005

Minimized remote knowledge queries.

Status: planned. Decided in: ADR 0018.

Requirement: Remote knowledge queries MUST carry identifiers only (intent, component, hardware IDs, versions, error codes), free text only with the person's consent for that question, no account and no machine identifier.

Rationale: Asking for help does not profile the person or the machine.

Gap: Protocol defined; Basalt client not integrated.

### BSC-PRIV-006

Conversations are not stored by default.

Status: planned. Decided in: ADR 0019.

Requirement: Assistant conversations MUST live in memory and be forgotten after a period of inactivity; keeping one MUST be opt-in per conversation, in a private file.

Rationale: What a person asks stays private unless they choose otherwise.

Gap: Proposed with the natural-language assistant.

## UPD: Updates, snapshots and rollback

### BSC-UPD-001

Snapshot around every package transaction.

Status: implemented. Decided in: ADR 0006.

Requirement: Every dnf transaction on an installed system MUST be wrapped in a pre and post root snapshot.

Rationale: A bad update, or a malicious package, can be rolled back.

Implemented in: `packages/basalt-snapshots/basalt-snapper.actions`, `packages/basalt-snapshots/basalt-snapshot-dnf`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"snapper pre/post"`
- Lab: `scripts/lab/snapshot-test.sh`, `snapper`
- Manual: audit guide, [BSC-UPD-001](audit-guide.md#bsc-upd-001)

Gap: A snapshot failure is logged and does not block an update (by design), and the root snapshot configuration is created by the installer.

### BSC-UPD-002

Rollback without losing data.

Status: implemented. Decided in: ADR 0006.

Requirement: Rollback MUST be possible from the running system and from the boot menu, always with a preview and confirmation, and MUST NOT touch the data subvolumes (home, logs, databases, containers, srv).

Rationale: Recovering the system never costs user data or the audit trail.

Implemented in: `packages/basalt-snapshots/basalt-rollback`, `packages/basalt-snapshots/basalt-snapshot-boot`.

Verified by:

- CI: `scripts/ci/vm-test.sh`, `"basalt-rollback dry run"`
- CI: `scripts/ci/vm-test.sh`, `"snapshot in boot menu"`
- Lab: `scripts/lab/rollback-test.sh`, `rollback`
- Manual: audit guide, [BSC-UPD-002](audit-guide.md#bsc-upd-002)

### BSC-UPD-003

Never boot a kernel without its signed driver module.

Status: implemented. Decided in: ADR 0017.

Requirement: On machines with the NVIDIA driver, a new kernel MUST be held until the matching signed module is published; a failed driver start MUST fall back to the open driver with a recorded reason.

Rationale: Updates never leave a machine without display or with Secure Boot off.

Implemented in: `packages/nvidia/basalt-nvidia`.

Verified by:

- Test: `packages/nvidia/tests/basalt-nvidia-test.sh`, `kernel`
- Manual: audit guide, [BSC-UPD-003](audit-guide.md#bsc-upd-003)

### BSC-UPD-004

Release upgrades tested before they are announced.

Status: partial. Decided in: ADR 0006.

Requirement: Each Fedora release upgrade path MUST be tested in virtual machines (snapshot, upgrade, boot, rollback) before it is announced.

Rationale: The biggest update of the year is not a gamble.

Implemented in: `scripts/lab/upgrade-test.sh`, `scripts/lab/upgrade-rollback-test.sh`.

Verified by:

- Lab: `scripts/lab/upgrade-rollback-test.sh`, `rollback`

Gap: Lab scripts exist and were run by hand; not yet a release gate in CI.

### BSC-UPD-005

Weekly rebuild and boot test.

Status: implemented. Decided in: ADR 0006.

Requirement: CI MUST rebuild and boot-test the system at least weekly, so a Fedora change that breaks an install or a control shows up without a commit.

Rationale: Drift in the base distribution is caught early.

Implemented in: `.github/workflows/ci.yml`.

Verified by:

- CI: `.github/workflows/ci.yml`, `cron:`
- Manual: audit guide, [BSC-UPD-005](audit-guide.md#bsc-upd-005)

## KEY: Signing keys and their custody

### BSC-KEY-001

Release key primary is offline and certify-only.

Status: partial. Decided in: ADR 0005.

Requirement: The OpenBasalt release key's primary MUST be certify-only and kept offline; only signing subkeys MAY be used by the release signer, and no release key MAY be stored in CI.

Rationale: Day-to-day signing never exposes the root of trust.

Implemented in: `docs/key-ceremony.md`, `scripts/release/sign.sh`.

Verified by:

- Test: `scripts/release/sign.sh`, `302461D26520E077D07FFCA9AA27C62C36CCFC4B`
- Manual: audit guide, [BSC-KEY-001](audit-guide.md#bsc-key-001)

Gap: The primary is certify-only, signing uses subkey exports only and CI holds no key. The primary is held in encrypted storage, not on an offline hardware token yet (see BSC-KEY-004).

References: NIST SSDF PS.1.

### BSC-KEY-002

Kernel module CA private key offline.

Status: partial. Decided in: ADR 0017.

Requirement: The kernel module CA's private key MUST be offline only and used only to issue signing certificates; module signing certificates MUST be rotated by package update before they expire.

Rationale: A leaked signing certificate can be replaced without asking every machine owner to enroll again.

Implemented in: `packages/basalt-security/basalt-module-ca.der`, `packages/basalt-security/basalt-module-signing.der`.

Verified by:

- Manual: audit guide, [BSC-KEY-002](audit-guide.md#bsc-key-002)

Gap: The CA key is used only to issue certificates and the first signing certificates are valid to 2028; the CA key is held in encrypted storage, not offline on a hardware token yet (see BSC-KEY-004).

### BSC-KEY-003

Release Secure Boot keys (PK, KEK, db).

Status: partial. Decided in: ADR 0002.

Requirement: Release PK, KEK and db keys for custom db mode MUST be created in a key ceremony and kept offline.

Rationale: Needed before custom db mode can be offered outside the lab.

Implemented in: `docs/key-ceremony.md`.

Verified by:

- Manual: audit guide, [BSC-KEY-003](audit-guide.md#bsc-key-003)

Gap: Created (with signed update files for setup mode) and held in encrypted storage, not offline on hardware tokens yet; not used by any published package.

### BSC-KEY-004

Key ceremony with two people and a signed log.

Status: partial. Decided in: ADR 0005.

Requirement: Release keys MUST be created on an air-gapped machine with two people (one operates, one verifies every fingerprint aloud), backups on two tokens in different places, passphrases split, and a log signed by both.

Rationale: No single person or machine can create or misuse a release key unnoticed.

Implemented in: `docs/key-ceremony.md`.

Verified by:

- Manual: audit guide, [BSC-KEY-004](audit-guide.md#bsc-key-004)

Gap: The procedure is documented, but the existing keys were generated by one operator in a network-isolated container on a general-purpose computer rather than on a dedicated air-gapped machine with two people, and are held in encrypted storage rather than on hardware tokens. Repeating the ceremony to the documented standard is planned.

### BSC-KEY-005

Published fingerprints and a revocation plan.

Status: implemented. Decided in: ADR 0005.

Requirement: Fingerprints of every release key and certificate MUST be published in more than one place, and the response to each kind of key compromise MUST be documented and include a public announcement.

Rationale: People can check keys out of band, and a compromise has a known response.

Implemented in: `SECURITY.md`, `docs/key-ceremony.md`, `docs/secure-boot.md`.

Verified by:

- Test: `SECURITY.md`, `3601 7348 42BD 4E48 2D19  DE4A E4EE D5EC A395 B302`
- Manual: audit guide, [BSC-KEY-005](audit-guide.md#bsc-key-005)

Gap: The revocation procedures have not been rehearsed, and the release key's third signing subkey (for release files) is not yet listed next to the packages and knowledge subkeys.

## GOV: Process, disclosure and the security program itself

### BSC-GOV-001

Private vulnerability reporting.

Status: implemented. Decided in: ADR 0005.

Requirement: Every public Basalt OS repository MUST offer private vulnerability reporting and a SECURITY.md with scope, timelines and supported versions.

Rationale: People who find a problem have a safe way to tell us.

Implemented in: `SECURITY.md`.

Verified by:

- Test: `SECURITY.md`, `security/advisories/new`
- Manual: audit guide, [BSC-GOV-001](audit-guide.md#bsc-gov-001)

### BSC-GOV-002

Controls change only with a decision record.

Status: implemented. Decided in: ADR 0020.

Requirement: A change to a control's requirement, scope or status MUST come with a design decision (ADR) and an update of this catalog in the same change, and the catalog MUST pass its CI check.

Rationale: What we promise and what we build cannot drift apart silently.

Implemented in: `docs/security/change-policy.md`, `scripts/ci/security-controls-check.sh`.

Verified by:

- CI: `scripts/ci/lint.sh`, `security-controls-check.sh`
- Manual: audit guide, [BSC-GOV-002](audit-guide.md#bsc-gov-002)

### BSC-GOV-003

Public adversarial testing every release.

Status: partial. Decided in: ADR 0009, ADR 0010.

Requirement: Every release MUST be tested with the public AI audit suite on a fresh install, and the results published, including cases not run.

Rationale: Security claims are measured in the open, not asserted.

Implemented in: `run-audit.sh` in ai-audit-suite.

Verified by:

- Manual: audit guide, [BSC-GOV-003](audit-guide.md#bsc-gov-003)

Gap: Run by hand so far (published results for 2026-10-05); not yet wired into the release process, and the live prompt-injection and DNS rebinding cases still need their lab endpoints.

### BSC-GOV-004

Repository protections on the public code repositories.

Status: partial. Decided in: ADR 0005.

Requirement: Public Basalt OS repositories MUST have secret scanning with push protection and security updates for dependencies turned on.

Rationale: Keeps credentials out of history and known-vulnerable dependencies out of builds.

Implemented in: `.github/dependabot.yml`.

Verified by:

- Manual: audit guide, [BSC-GOV-004](audit-guide.md#bsc-gov-004)

Gap: Not enabled on every public repository yet.

## Mapping to known references

These tables help readers who know the references find the related
controls. They are not a claim of compliance or certification.

### OWASP Top 10 for LLM Applications (2025)

| Item | Controls |
|---|---|
| LLM01 Prompt Injection | BSC-AI-005, BSC-GATE-009, BSC-GATE-001, BSC-AI-004 |
| LLM02 Sensitive Information Disclosure | BSC-AGENT-003, BSC-NET-001, BSC-PRIV-001, BSC-PRIV-004 |
| LLM03 Supply Chain | BSC-PKG-001, BSC-AI-001, BSC-AI-002 |
| LLM04 Data and Model Poisoning | BSC-AI-001, BSC-AI-008 |
| LLM05 Improper Output Handling | BSC-GATE-002, BSC-AI-005 |
| LLM06 Excessive Agency | BSC-GATE-001, BSC-GATE-002, BSC-AGENT-001, BSC-AI-004, BSC-GATE-008 |
| LLM07 System Prompt Leakage | BSC-GATE-002, BSC-AI-004 |
| LLM08 Vector and Embedding Weaknesses | BSC-AI-001, BSC-AI-009 |
| LLM09 Misinformation | BSC-AI-008, BSC-GATE-004 |
| LLM10 Unbounded Consumption | BSC-AI-010, BSC-AGENT-003 |

### NIST SP 800-218 Secure Software Development Framework (SSDF)

| Item | Controls |
|---|---|
| PO.3 Implement supporting toolchains | BSC-PKG-008, BSC-GOV-002 |
| PS.1 Protect all forms of code | BSC-PKG-002, BSC-KEY-001, BSC-GOV-004 |
| PS.2 Provide a mechanism for verifying release integrity | BSC-PKG-001, BSC-PKG-006, BSC-KEY-005 |
| PS.3 Archive and protect each release | BSC-PKG-004 |
| PW.7 and PW.8 Review and test for vulnerabilities | BSC-MAC-003, BSC-GOV-003 |
| RV.1 Identify and confirm vulnerabilities | BSC-GOV-001 |

### CIS-style host hardening (benchmark topics, not a CIS benchmark result)

| Item | Controls |
|---|---|
| Secure boot settings | BSC-BOOT-001, BSC-BOOT-002 |
| Mandatory access control | BSC-MAC-001, BSC-MAC-002 |
| Filesystem encryption and swap | BSC-DISK-001, BSC-DISK-005 |
| Host firewall | BSC-NET-008 |
| SSH server configuration | BSC-NET-009 |
| Logging and auditing | BSC-LEDGER-001, BSC-LEDGER-005 |
| Software updates and package integrity | BSC-PKG-001, BSC-UPD-001 |
