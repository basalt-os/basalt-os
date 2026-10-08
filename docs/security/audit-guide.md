# Audit guide

How anyone can check the Basalt OS security controls, on a running system
and in the source repositories. Our own periodic audit follows this page
step by step; you can too. Each section is named after a control ID from
[controls.md](controls.md) and says what to run and what you should see.

Conventions:

- Commands marked `#` run as root on the machine being audited; `$` as an
  ordinary user; `repo$` in a checkout of
  [basalt-os/basalt-os](https://github.com/basalt-os/basalt-os); `shell$`
  in a checkout of
  [basalt-os/basalt-shell](https://github.com/basalt-os/basalt-shell).
- Audit a machine installed with the Basalt installer (or the kickstart)
  and updated to the latest published packages. A machine where Basalt
  packages were added to another Fedora install misses the installer's
  parts (encryption, kernel arguments, subvolumes, snapshot configuration)
  and the result should say so.
- Never audit with real secrets on a test machine. The public AI audit
  suite (below) uses fake fixtures marked LAB-FAKE.
- Record for every control: pass, fail, not applicable (with the reason) or
  not run (with the reason), and the evidence (command output, trimmed).
  Never paste key material, recovery keys or passphrases into a report.

## Quick runs

Most checks are automated somewhere. Run these first; the sections below
explain what each control needs on top.

```
repo$ scripts/ci/security-controls-check.sh          # catalog against the code
repo$ make ci-lint                                   # static checks, unit tests that run in lint
repo$ make agent-test ledger-test resolver-test assistant-test   # Go unit tests (Fedora container)
repo$ make ci-boot-test                              # install in QEMU with Secure Boot and a TPM, smoke checks
```

The public adversarial suite,
[basalt-os/ai-audit-suite](https://github.com/basalt-os/ai-audit-suite),
attacks a running machine from inside a confined agent session (categories
1 confinement, 2 egress, 3 ledger, 4 confirmation, 5 prompt injection).
Follow its `docs/reproduce.md`. Two practical notes: the probe and its
targets file must be readable from inside the agent session (a system
directory such as /opt works, /tmp and the home directory do not), and the
probe's output must go through a pipe, because an agent domain cannot write
to a file the runner opened for it. Expected result: no failures; cases
reported as not run must each have a stated reason.

## Release checklist

The subset that must pass before any release is published. The release is
blocked by any failure; a not-run item needs a written reason in the
release notes.

1. `scripts/ci/security-controls-check.sh` passes, and every control whose
   status changed has its decision referenced (BSC-GOV-002).
2. CI is green on the release commit, including the boot test
   (BSC-BOOT-001, BSC-DISK-001 to 003, BSC-MAC-001 to 003, BSC-LEDGER-001,
   005, 007, BSC-UPD-001, 002).
3. The packages were built by `scripts/release/build.sh`, signed by
   `scripts/release/sign.sh` and checked by `scripts/release/client-test.sh`
   (BSC-PKG-001 to 004).
4. Package signatures name the packages subkey and the public key in
   basalt-release equals the published one (BSC-PKG-001, BSC-KEY-001).
5. The public AI audit suite ran on a fresh install of the release
   candidate, results published with the release (BSC-GOV-003).
6. The lab matrices for what changed ran on a VM in enforcing mode:
   `scripts/lab/agent-test.sh`, `scripts/lab/ledger-test.sh`,
   `scripts/lab/assistant-test.sh` (BSC-AGENT-*, BSC-NET-*, BSC-GATE-*).
7. Kernel module certificates are not within 90 days of expiry
   (BSC-KEY-002).
8. SECURITY.md and the fingerprints in it are current (BSC-GOV-001,
   BSC-KEY-005).

## BOOT

### BSC-BOOT-001

```
# mokutil --sb-state
# basalt-secureboot status
```

Expected: `SecureBoot enabled`, setup mode no. In CI: the boot test's
"secure boot" check passes.

### BSC-BOOT-002

```
# cat /sys/kernel/security/lockdown
# tr ' ' '\n' </proc/cmdline | grep -E 'lockdown|sig_enforce'
```

Expected: `[integrity]` selected, and both `lockdown=integrity` and
`module.sig_enforce=1` on the command line (unless the administrator chose
`basalt.lockdown=0` at install, which the installer warned about).

### BSC-BOOT-003

```
# mokutil --list-enrolled | grep -i subject
# keyctl list %:.machine
# systemctl show -p Result --value basalt-module-keys
# openssl x509 -inform DER -in /usr/share/basalt/secureboot/basalt-module-ca.der -noout -subject -fingerprint -sha256
```

Expected: where Basalt-signed modules are used (the NVIDIA driver), the
OpenBasalt Kernel Module CA is enrolled, appears in `.machine`, and its
fingerprint equals the one in docs/secure-boot.md; basalt-module-keys
succeeded. In the repository: `scripts/lab/mok-test.sh` passes on a lab VM.

### BSC-BOOT-004

Lab only today: `scripts/lab/sb-custom-test.sh` on a VM (custom keys,
re-signed shim, a stock shim refused, TPM suspend and re-enroll). Expected:
all steps pass. On a real machine: not applicable until release keys and
a signed shim package exist.

## PKG

### BSC-PKG-001

```
# rpm -V basalt-release; ls /etc/yum.repos.d/*.rpmnew 2>/dev/null
# for r in basalt basalt-tools basalt-testing; do dnf --dump-repo-config=$r | grep -E '^(gpgcheck|repo_gpgcheck|gpgkey) '; done
# ls /etc/dnf/repos.override.d/
# rpm -q gpg-pubkey --qf '%{version} %{summary}\n' | grep -i basalt
# rpm -Kv basalt-release | grep -i 'key id'
```

Expected: every Basalt repository with `gpgcheck = 1`, `repo_gpgcheck = 1`
and the shipped key in the configuration dnf uses. These come from
`/usr/share/dnf5/repos.override.d/20-basalt-signatures.repo` (basalt-release,
replaced by every upgrade), which dnf applies after the repository files, so
an older or edited `basalt.repo` kept by the package manager (a changed
file or a `.rpmnew` next to it) no longer drops `repo_gpgcheck`. A file
named `20-basalt-signatures.repo` under `/etc/dnf/repos.override.d` masks
it and a `99-config_manager.repo` there can change any option: both are
deliberate administrator changes, so look at what they set. Also: the
OpenBasalt release key imported, package signatures by key ID `aa27c62c36ccfc4b` (the packages
subkey). In the repository: `scripts/release/client-test.sh` refuses a
tampered `repomd.xml` and another key.

### BSC-PKG-002

```
repo$ grep -n -- '--network none' scripts/release/sign.sh
repo$ OB_REPO=basalt scripts/release/sign.sh --test-key IN_DIR OUT_DIR
```

Expected: signing runs in a container without network; a dry run with a
throwaway key completes and its verification step passes; an export that
holds the secret primary key is refused.

### BSC-PKG-003

```
repo$ grep -n 'refuse' scripts/release/build.sh
```

Expected: `build.sh` refuses `.env` lab overrides (development key, lab
certificates, lab URLs) and writes `SHA256SUMS` and `BUILD-INFO.txt`.

### BSC-PKG-004

```
repo$ scripts/release/upload.sh --dry-run OUT_DIR
```

Expected: the plan uploads packages and blobs before metadata, skips
identical published files, and stops on a published file with different
content.

### BSC-PKG-005

Expected: images built with `LIVE_SOURCE=obpkg` log the signature check of
each `repomd.xml` and the checksum of every package
(`scripts/release/mirror-published.sh`).

### BSC-PKG-008

```
repo$ grep -n 'permissions' -A2 .github/workflows/*.yml
repo$ grep -n 'uses:' .github/workflows/*.yml
```

Expected: `contents: read` at the top of every workflow, every action
pinned by a 40-character commit SHA, no secret used for signing or
publishing.

## DISK

### BSC-DISK-001

```
# lsblk -o NAME,TYPE,FSTYPE,MOUNTPOINTS
# cryptsetup luksDump "$(blkid -t TYPE=crypto_LUKS -o device | head -1)" | grep -E '^Version'
```

Expected: the root btrfs sits on a LUKS2 device. An unencrypted install is
a pass only if the person chose it explicitly (servers in a controlled
environment); write that down.

### BSC-DISK-002

```
# systemd-cryptenroll "$(blkid -t TYPE=crypto_LUKS -o device | head -1)"
# ls /root/.basalt-luks.pass
```

Expected: exactly the unlock method (tpm2, or a clevis token) and one
`recovery` slot, no password slot left by the installer, and no
`.basalt-luks.pass` file.

### BSC-DISK-003

```
# cryptsetup luksDump "$(blkid -t TYPE=crypto_LUKS -o device | head -1)" | grep tpm2-hash-pcrs
# grep TPM_PCRS /etc/basalt/tpm.conf
# basalt-tpm status
```

Expected: `tpm2-hash-pcrs: 7`, `TPM_PCRS=7`, and the machine booted without
a prompt. Lab: `scripts/lab/sb-test.sh` shows that changing the Secure Boot
state stops the automatic unlock and the recovery key works.

### BSC-DISK-004

```
# clevis luks list -d "$(blkid -t TYPE=crypto_LUKS -o device | head -1)"
# journalctl -b -g 'Tang key trusted on first use'
```

Expected (tang and tpm2+tang installs only): the binding names the site's
Tang server; compare its thumbprint with `tang-show-keys` on the server.
A trust-on-first-use warning in the install log means the thumbprint was
not pinned and must be compared now.

### BSC-DISK-005

```
# swapon --show
```

Expected: only `/dev/zram*` devices.

## MAC

### BSC-MAC-001

```
# getenforce
# sestatus | grep -E 'Loaded policy|Mode from config'
```

Expected: `Enforcing`, policy `targeted`, mode from config `enforcing`.

### BSC-MAC-002

```
# ps -eo label,comm | grep basalt
# semodule -l | grep ^basalt
```

Expected: basalt-assistantd in `basalt_assistant_t`, basalt-ledgerd in
`basalt_ledger_t`, basalt-resolver in `basalt_resolver_t` (and
`basalt_llm_t` when the local model runs); the matching modules loaded.

### BSC-MAC-003

```
# ausearch --input-logs -m AVC,USER_AVC,SELINUX_ERR -ts boot | grep -c '^type='
# journalctl -b _TRANSPORT=audit -g avc -o cat | grep -o 'scontext=[^ ]* tcontext=[^ ]* tclass=[^ ]*' | sort | uniq -c
```

Expected: zero on a machine doing normal work. Denials that come from an
escape test you ran (agent domains reading what they must not) are
expected; any other denial is a finding.

### BSC-MAC-004

```
repo$ grep -rn audit2allow packages/basalt-assistant --include=*.go | grep -v _test
repo$ make assistant-test
```

Expected: no code path generates policy; the tests
`TestSuspiciousIsNeverFixed` and `TestAnalyzePathNeverRelabelsShadow` pass.

### BSC-MAC-005

```
# sesearch -A -s basalt_agent_t -t ssh_home_t -c file -p read
# sesearch -A -s basalt_agent_t -t shadow_t -c file -p read
# sesearch -A -s basalt_agent_t -t basalt_agent_secret_t
# grep expand-check /etc/selinux/semanage.conf
```

Expected: no allow rule for reading those types (a generic `map` rule from
the base policy is not a read). `expand-check=0` is Fedora's default and
means the store does not re-check neverallow rules at install; the rules
are checked when the module is built, and `packages/basalt-agent/tests/driver.sh selinux`
counts the allow rules that would break them (expected: zero).

### BSC-MAC-006

```
# systemd-analyze security basalt-ledger basalt-resolver basalt-assistantd
# systemctl show -p NoNewPrivileges,ProtectSystem,CapabilityBoundingSet,PrivateNetwork basalt-ledger
```

Expected: each unit rated OK or better, `NoNewPrivileges=yes`,
`ProtectSystem=strict`, the capability set limited to what the docs name
(`CAP_LINUX_IMMUTABLE` for the ledger, `CAP_NET_ADMIN` for the resolver,
`CAP_DAC_READ_SEARCH` for the assistant), no network for the ledger and
the local model.

## GATE

### BSC-GATE-001

On a desktop with the shell: run `lab/demo/security-test.sh` from
basalt-os/basalt-shell on a lab VM in enforcing mode. Expected: every
attempt by a non-UI process to confirm is refused (T1 to T10) and only the
person's Enter on the sheet applies (T11). On a server: the audit suite's
category 4 passes (the agent cannot reach a confirming socket).

### BSC-GATE-002

```
repo$ make assistant-test
# basalt pending; basalt show ID
```

Expected: `TestCommandsAreExact` and `TestValidationRejects` pass; every
pending proposal lists only actions of the closed set with their exact
commands.

### BSC-GATE-003

```
repo$ go test ./... -run 'TestFingerprintBindsCommands|TestProtocolAndProposeOnly'    # in packages/basalt-assistant
# basalt apply ID --yes --confirm 00000000
```

Expected: tests pass; a wrong code is refused and recorded in the
assistant's audit log (`basalt audit tail`).

### BSC-GATE-004

```
# snapper -c root list | grep basalt=apply
# basalt audit tail
```

Expected: every applied proposal has a pre and post snapshot with
`proposal=ID` and an `apply` record holding the commands and their output.
Lab: `scripts/lab/assistant-test.sh` passes.

### BSC-GATE-005

```
shell$ go test ./internal/shell -run TestPersonActionsRefuseAgents    # in basalt-os/basalt-shell
```

Expected: pass. On a desktop, an agent's request to send mail or insert
text is refused before it reaches a sheet.

### BSC-GATE-006

On a desktop lab VM: start an agent control session, make the agent type
and immediately request a confirmation. Expected: the sheet ignores input
for its arming delay, a confirmation within 1.5 seconds of synthetic input
is refused, and only one sheet shows at a time.

### BSC-GATE-007

```
repo$ make gate-test
# grep -E '^enforce' /etc/basalt-gate/gate.conf
# basalt-ledger --producer basalt-gate --since today
```

Expected: tests pass; `enforce` lists the approval paths the gate decides
on this machine; every request of those paths has a `gate.request` and a
`gate.decision` record naming the rule or the person, and the others have
`observed` records (shadow mode).

### BSC-GATE-010

On a desktop: start an agent control session and press Super+Shift+Escape
or the Stop button. Expected: the session ends at once and the frame
disappears. The full stop of all automation is planned with the gate.

## LEDGER

### BSC-LEDGER-001

```
# basalt-ledger status
# basalt-ledger verify
$ basalt-ledger summary --since today
```

Expected: `ledger chain intact` across all files; the summary lists the
day's sessions, escalations and logins.

### BSC-LEDGER-002

Audit suite category 3 (`api.delete`, `api.update`, `api.truncate`,
`api.rewrite`). Expected: all blocked, each refusal visible as
`ledger.refused` to the user who tried, and the chain still intact.

### BSC-LEDGER-003

Audit suite category 3 (`forge.hash`, `forge.producer`) as an ordinary
user, and the unit test `TestProducerRules`. Expected: forged chain fields
end up in `src` only, built-in producer names are refused from the socket.

### BSC-LEDGER-004

```
$ basalt-ledger --producer polkit
$ basalt-ledger --all
# ls -l /var/log/basalt-ledger
```

Expected: an ordinary user sees only their own records; `--all` asks for
administrator authentication; files are root-only.

### BSC-LEDGER-005

```
# lsattr /var/log/basalt-ledger/*.jsonl /var/log/basalt-assistant/*.jsonl
# findmnt -no SOURCE /var/log
# basalt-ledger rotate && basalt-ledger verify
```

Expected: the current files `a` (append-only), sealed files `i`
(immutable), `/var/log` on its own subvolume (`[/var_log]` in the
source), and verify reports one more seal after the rotation.

### BSC-LEDGER-006

```
# grep -E '^retention' /etc/basalt-ledger/ledger.conf
$ basalt-ledger --event ledger.retention
```

Expected: retention as configured (default 365d); every removed file has a
retention record, and verify reports removed records as covered by the
policy, never as a broken chain.

### BSC-LEDGER-007

```
# basalt-ledger export -n 20 -o /root/check.json
# basalt-ledger verify-export /root/check.json --key /var/log/basalt-ledger/keys/export.pub
```

Expected: the export is signed by a key of kind `tpm` on machines with a
TPM (or `software`, labeled, without one) and verifies; changing one byte
of the export makes verification fail. Keep a copy of `export.pub` from
install time and compare it.

### BSC-LEDGER-010

```
# basalt audit verify
# lsattr /var/log/basalt-assistant/*.jsonl
```

Expected: the assistant's log is intact across its sealed files, current
file append-only, sealed files immutable.

## NET

### BSC-NET-001

```
# systemctl is-active basalt-resolver
# nft list table inet basalt_egress
$ basalt-resolver sessions
```

Expected: the resolver active; during an agent session, a chain and sets
for it. Audit suite category 2: unlisted hosts through the proxy and
directly, IP literals and other DNS servers all blocked; the allowed host
reachable.

### BSC-NET-002

Unit tests `TestRebindingRefused` (resolver) and
`TestPrivateAddressesRefused` (agent); lab: `scripts/lab/ledger-test.sh`.
With a lab name server pointing a name at a private address, the suite's
rebinding cases are blocked and recorded as `dns.rebinding`.

### BSC-NET-003

Audit suite case `egress.dns-exfil`. Expected: blocked, and
`basalt-ledger --event dns.deny` shows the refused name.

### BSC-NET-004

```
$ basalt-agent grant SESSION host example.com      (from inside a session: must fail)
# basalt-ledger --event egress.grant
```

Expected: a session cannot widen itself (audit suite `priv.self-grant`);
a person's grant asks for administrator authentication and is recorded.

### BSC-NET-005

```
# systemctl stop basalt-resolver
$ basalt-agent run PROFILE --mode native --project DIR     (must refuse)
# systemctl start basalt-resolver
```

Expected: native sessions refuse to start while the resolver is down;
running sessions keep their rules and cannot resolve new names.

### BSC-NET-006

```
# getsebool basalt_agent_direct_egress
```

Expected: `off`, unless the administrator turned it on deliberately (write
down why).

### BSC-NET-007

```
repo$ cmp packages/basalt-agent/internal/allowlist/allowlist.go packages/basalt-resolver/internal/allowlist/allowlist.go
```

Expected: identical (lint checks this on every change).

### BSC-NET-008

```
# firewall-cmd --get-default-zone
# firewall-cmd --zone=basalt --list-all
```

Expected (server edition): default zone `basalt`, services `ssh` only, no
open ports beyond what the administrator added and documented.

### BSC-NET-009

```
# sshd -T | grep -E '^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin|authenticationmethods|permitemptypasswords|x11forwarding|allowagentforwarding|permittunnel) '
```

Expected: `passwordauthentication no`, `kbdinteractiveauthentication no`,
`permitrootlogin prohibit-password`, `authenticationmethods publickey`,
`permitemptypasswords no`, `x11forwarding no`, `allowagentforwarding no`,
`permittunnel no`.

## AGENT

### BSC-AGENT-001

Audit suite category 1, `fs.*` cases, from a native session and from a
container session. Expected: reads of decoy keys, keyrings, browser
profiles, other projects and other users blocked; writes to the home
directory and shell rc files blocked; writing inside the project allowed.

### BSC-AGENT-002

Start two sessions; from one, run the suite's `session.*` cases against
the other. Expected: environment, secrets, project and control socket of
the victim session unreadable. `basalt-agent sessions` shows different MCS
levels.

### BSC-AGENT-003

Lab: `scripts/lab/agent-test.sh credentials`. Expected: the agent sees
only the placeholder value; a request to any other host never carries the
key (checked in the mock servers' logs); the ledger records
`credential.use` with name, host and port only.

### BSC-AGENT-004

```
$ ls -Zd ~/.config/basalt-agent/secrets
# sesearch -A -s basalt_agent_t -t basalt_agent_secret_t
```

Expected: the store labeled `basalt_agent_secret_t` (after the first
session); no allow rule from agent domains to it except what the base
policy grants every domain on any file type (such as `map`).

### BSC-AGENT-005

Audit suite cases `priv.*`, `proc.*`, `cont.*`, `ptrace.attach`,
`setuid.find`. Expected: all blocked.

### BSC-AGENT-006

Audit suite case `fs.write-git-hook`. Expected: blocked.

### BSC-AGENT-007

```
$ basalt-agent list
$ basalt-agent egress claude
```

Expected: shipped profiles allow their provider, its login hosts and the
registries list only; no `github.com` unless the profile includes the
GitHub list on purpose.

### BSC-AGENT-008

basalt-shell `lab/demo/security-test.sh` T6 to T9 on a desktop lab VM.
Expected: the agent domain cannot connect to the Wayland, D-Bus, X11 or
PipeWire sockets.

### BSC-AGENT-009

basalt-shell `lab/voice/tests/escape-test.sh` on a desktop lab VM.
Expected: each voice and skill domain may do only what its row in the
shell's SELinux documentation allows.

## AI

### BSC-AI-001

```
$ gpg --show-keys --with-subkey-fingerprints /usr/share/basalt/knowledge/openbasalt-release-key.asc
$ gpg --dearmor </usr/share/basalt/knowledge/openbasalt-release-key.asc >k.gpg
$ gpgv --keyring ./k.gpg /usr/share/basalt/knowledge/44/manifest.json.sig /usr/share/basalt/knowledge/44/manifest.json
```

Expected: the primary and knowledge subkey fingerprints of
docs/assistant.md; `Good signature`; the SHA-256 of `cases.jsonl` and
`index.bin` equal the manifest's. Changing one byte of the index makes the
assistant fall back to rules (`TestVSMKnowledgeSignatureFailsClosed`).

### BSC-AI-002

```
repo$ make models-test llm-test
repo$ packages/basalt-voice/tests/manifest-test.sh
```

Expected: pass; every manifest entry has a pinned HTTPS URL and a SHA-256;
a mismatched file is deleted and never installed.

### BSC-AI-003

```
# grep -E '^downloads' /etc/basalt/models.conf
$ basalt-ledger --producer basalt-models
```

Expected: the policy the administrator chose; every download has a
consent, a request and a result record.

### BSC-AI-004

Expected: no MCP tool of `basalt-mcp` or of the shell changes anything
(read the tool list in docs/assistant.md and the shell's MCP server); the
tests `TestProtocolAndProposeOnly` and
`TestConfidentAnswerWithoutAFixProposesNothing` pass.

### BSC-AI-005

```
shell$ go test ./internal/guard/...      # in basalt-os/basalt-shell
```

Expected: pass. Audit suite category 5 against a live assistant endpoint
(`--endpoint`): every case `no_action`, `no_network_out`, `no_writes` and
`warned`. Without an endpoint only the corpus self-check runs; report the
live cases as not run.

### BSC-AI-006

```
# systemctl show -p PrivateNetwork,IPAddressDeny,DynamicUser basalt-llm
# ps -eo label,comm | grep llama
```

Expected (when the local model is installed): `PrivateNetwork=yes`,
`IPAddressDeny=any`, a dynamic user, domain `basalt_llm_t`.

### BSC-AI-007

```
# systemctl show -p RestrictAddressFamilies,CapabilityBoundingSet basalt-assistantd
```

Expected: address families AF_UNIX and AF_NETLINK only, capability
`cap_dac_read_search` only; the boot test's "assistant daemon confined"
check passes.

### BSC-AI-008

```
repo$ make eval-check eval-rules
```

Expected: every case valid; accuracy and calibration not below the
previous release. Record the numbers in the audit report.

## PRIV

### BSC-PRIV-001

```
$ basalt feedback "audit test" --kind other --preview
```

Expected: the payload shown with the number of redacted values, no host
name, user name or address in it, and nothing sent without `--yes
--confirm CODE`.

### BSC-PRIV-002

```
# grep -A8 '^\[humanize\]' /etc/basalt/assistant.conf
```

Expected: `allow_remote` off unless the administrator turned it on;
`TestRemoteEndpointRefused` and `TestRedact` pass.

### BSC-PRIV-003

```
# systemctl list-timers --all
# grep -H countme /etc/yum.repos.d/*.repo
```

Expected: no Basalt timer or service that sends data off the machine.
Fedora's repository files carry `countme=1` (an anonymous weekly counter
used by Fedora); note it in the report.

### BSC-PRIV-004

```
# grep -c -E 'sk-ant|sk-proj|LAB-FAKE' /var/log/basalt-ledger/*.jsonl
```

Expected: zero after the credential lab run (the decoy key never appears
in the ledger, the agent's session log or the journal).

## UPD

### BSC-UPD-001

```
# dnf -y install <some small package>
# snapper -c root list | tail -2
```

Expected: a pre and post pair with the dnf command line in the
description.

### BSC-UPD-002

```
# basalt-snapshot-boot list
# printf 'n\n' | basalt-rollback N
# btrfs subvolume list / | awk '{print $NF}'
```

Expected: snapshots listed in the boot menu; the rollback shows its
command and changes nothing when declined; separate subvolumes for home,
logs, databases, containers and srv.

### BSC-UPD-003

```
repo$ make nvidia-test
# basalt-ledger --event driver.kernel_hold
```

Expected: tests pass; on a machine with the driver, a kernel without its
signed module is held and the hold recorded.

### BSC-UPD-004

Lab: `scripts/lab/upgrade-rollback-test.sh` on a VM for the next Fedora
release. Expected: upgrade, boot, rollback to the previous release and
forward again succeed.

### BSC-UPD-005

```
repo$ gh run list -R basalt-os/basalt-os -L 20
```

Expected: a scheduled run in the last 8 days, and the latest runs green.

## KEY

### BSC-KEY-001

```
$ curl -fsS https://obpkg.org/keys/openbasalt-release-key.asc | gpg --show-keys --with-subkey-fingerprints
```

Expected: primary `3601 7348 42BD 4E48 2D19 DE4A E4EE D5EC A395 B302` with
usage `[C]` only; signing subkeys `[S]` only, among them the packages
subkey `3024 61D2 6520 E077 D07F FCA9 AA27 C62C 36CC FC4B` and the
knowledge subkey `85D6 1430 B704 3868 0F6E B955 E79E 4020 605A 659A`. The
same file as `packages/basalt-release/RPM-GPG-KEY-basalt`. Record where the
primary is kept (offline token or encrypted storage).

### BSC-KEY-002

```
$ openssl x509 -inform DER -in packages/basalt-security/basalt-module-signing.der -noout -subject -enddate -fingerprint -sha256
```

Expected: issued by the OpenBasalt Kernel Module CA, more than 90 days
before `notAfter` (2028-10-03 for the first certificate), fingerprints as
in docs/secure-boot.md.

### BSC-KEY-003

Expected: the release PK, KEK and db certificates and their signed update
files are recorded in a ceremony log with their fingerprints; no published
package uses them until custom db mode is offered (BSC-BOOT-004).

### BSC-KEY-004

Expected: a ceremony log for each release key, signed by the people who
took part, listing every fingerprint, serial number and checksum. The
audit records whether the log exists and matches the published
fingerprints; the log itself is not published.

### BSC-KEY-005

Expected: the fingerprints in SECURITY.md, docs/key-ceremony.md,
docs/secure-boot.md, docs/assistant.md and on obpkg.org agree with each
other.

## GOV

### BSC-GOV-001

```
repo$ gh api repos/basalt-os/basalt-os/private-vulnerability-reporting --jq .enabled
```

Expected: `true` for every public Basalt OS repository, and a SECURITY.md
in each.

### BSC-GOV-002

```
repo$ scripts/ci/security-controls-check.sh
repo$ git log --oneline -- docs/security/controls.yaml
```

Expected: the check passes; each change of the catalog names its decision
in the commit message or the changelog of [change-policy.md](change-policy.md).

### BSC-GOV-003

Expected: published audit suite results for the release under audit, with
the versions tested, zero failures and a reason for every case not run.

### BSC-GOV-004

```
repo$ gh api repos/basalt-os/basalt-os --jq .security_and_analysis
```

Expected: `secret_scanning`, `secret_scanning_push_protection` and
`dependabot_security_updates` enabled on every public Basalt OS
repository.

## Planned controls

Controls with status planned (see [controls.md](controls.md)) have nothing
to check yet. The audit lists them with their gap so the report shows the
whole picture, and checks that none of them is described anywhere as if it
were in place.
