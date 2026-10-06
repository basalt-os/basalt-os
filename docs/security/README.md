# How Basalt OS protects you

Security model 0.1, draft (catalog version 1, 2026-10-06). This model is
in development and is published as it evolves: each release is a tagged
version on GitHub, until a final 1.0 that matches the first stable Basalt OS
release. See [change-policy.md](change-policy.md#releases-of-the-security-model).

Basalt OS is built so that AI can help on your computer without being able
to harm it or you. This page explains, in plain words, the layers of
protection, what each one stops, and what it does not stop. Basalt OS is
pre-alpha: some layers are in place today and some are still being built,
and this page says which.

For the details:

- [threat-model.md](threat-model.md): what we protect, from whom, and what
  we assume;
- [controls.md](controls.md): every protection as a numbered control
  (IDs starting with BSC-), with where it is built and how it is tested;
- [audit-guide.md](audit-guide.md): how to check each control yourself;
- [change-policy.md](change-policy.md): how the controls change, and how
  to report a problem.

## The idea in one paragraph

Safety lives in the system, not in the AI. An AI model, whether ours, a
bigger one you install or one from a cloud provider, can only suggest. The
operating system decides what it may touch, where it may connect, and it
asks you before anything changes. Everything that matters is written to a
record that cannot be quietly edited. A smarter model gets better at
helping, not more powerful.

## The layers

### 1. A verified start: Secure Boot and signed packages

When the computer starts, the firmware checks that the boot loader and the
kernel are signed by Fedora, and the kernel refuses drivers that are not
signed. Basalt's own drivers (for example the NVIDIA driver) are signed
with a Basalt key you approve once at the firmware screen. Every Basalt
package and the list of packages in the repository are signed, and your
system refuses anything with a wrong or missing signature.

Protects against: tampered boot software, unsigned drivers, modified
packages on a download server or mirror.

Does not protect against: a firmware or hardware attack, or a problem in a
package that was correctly signed by Fedora or by us. Signing proves who
published a file, not that it is free of bugs.

### 2. Disk encryption

The installer encrypts the disk by default. The computer's security chip
(TPM) unlocks it automatically only if the start-up was not tampered with;
otherwise it asks for the recovery key you were shown when installing.
Servers can also require a key server on the local network (Tang). Memory
is never swapped to disk.

Protects against: someone who steals the computer or the disk and tries to
read it.

Does not protect against: someone who uses the computer while it is on and
unlocked, or who learns your password or recovery key. If you choose to
install without encryption, this layer is off.

### 3. Confinement with SELinux

SELinux is a feature of the Linux kernel that limits what each program may
do, even when the program is running as you or as root. On Basalt it is
always on (enforcing). Basalt's own services, the assistant and every AI
agent you start through Basalt get their own narrow set of permissions.

Protects against: a program that was tricked or broken doing more than its
job, for example an AI agent reading your SSH keys or another project.

Does not protect against: programs you run yourself outside an agent
session. Like on Fedora, they run with your full rights. Confined user
accounts and confinement for all desktop apps are planned.

We are building Basalt Shield (Escudo in Portuguese), a friendly view of
these protections: it will tell you, in plain words, when something was
blocked ("an app tried to read your Documents and was stopped") and let
you decide, with every change going through the approval step below.

### 4. You approve every change

AI agents and the assistant can only propose a change. You see exactly
what will happen (the commands, the e-mail with its recipient, the files to
move) and you confirm it. The system checks who confirms: only the desktop's
own confirmation sheet or an administrator at a terminal can approve, never
the program that asked. Before a system change, a snapshot is taken so it
can be undone.

Protects against: an AI that was tricked by a web page or an e-mail into
acting for an attacker, and an agent approving its own request.

Does not protect against: approving something without reading it. The
preview is exact, but the decision is yours.

Today each feature has its own approval step. We are building one approval
gate for everything, with an Approvals app where you see what is waiting,
who or which rule decided each past action, and rules such as "tidy my
Downloads folder every Friday and tell me". It will also have a stop button
that halts all automation at once, and a short list of things no rule can
ever allow (turning protection off, deleting the audit record, removing
disk encryption). The gate is planned, not in place yet.

### 5. An audit record you can trust

Basalt keeps one record of security events: what each AI agent did, which
connections were allowed or refused, what SELinux blocked, who became
administrator, when the system was rolled back. Each entry is linked to the
previous one with a cryptographic hash, so editing or deleting an entry
breaks the chain and is detected. You can read your own entries in plain
English, and export a signed copy for an incident report; on computers with
a TPM the signing key never leaves the chip.

Protects against: hiding what happened, and forging entries that look like
they came from another program.

Does not protect against: an administrator (root) deleting the record. Root
can delete or rewrite the files; what root cannot do is make the change
invisible, because the chain will no longer verify. Sending the chain's
latest state to an outside service, so even that is caught, is planned.

### 6. Network control for AI agents

An AI agent session can only connect to the names on its list, for example
its AI provider and the package registries it needs. Everything else is
blocked by the kernel and recorded, including tricks like hiding data in
DNS names or pointing an allowed name at a computer on your local network.
Only a person can add a name to the list.

Protects against: an agent sending your data to a server it chose.

Does not protect against: what an agent sends to a host on its list. If a
list allows a package registry or GitHub, the agent can upload there. Keep
the lists short.

### 7. AI agents in a box

Third-party AI agents such as Claude Code or Codex run through
`basalt-agent`. Each session sees only its project folder; it cannot read
your keys, passwords, browser profiles or other projects, cannot write
elsewhere in your home folder, and cannot become administrator. Your API
key for the AI provider is never given to the agent: a small proxy adds it
only to requests going to that provider.

Protects against: a prompt-injected or misbehaving agent stealing
credentials, planting something that runs later, or leaking its key.

Does not protect against: the agent spending your AI provider credits
through the provider it is meant to use, or damaging the project you gave
it (use version control).

### 8. Signed models and knowledge

The assistant's knowledge of known problems and fixes is signed with the
OpenBasalt release key and checked before use; if the check fails, the
assistant falls back to its built-in rules. Model files are downloaded
only when you choose to, from fixed addresses, and installed only if their
checksum matches. Text the assistant reads from mail, pages and files is
treated as data, never as instructions, and its answers are plain text
without links. The local model runs with no network access at all.

Protects against: poisoned knowledge, swapped model files, and instructions
hidden in content.

Does not protect against: a wrong answer. The assistant can be mistaken;
that is why it proposes and you confirm. Our tests of hidden instructions
against a running assistant are still being completed.

### 9. Privacy when something leaves the computer

Nothing is sent to us unless you send it. Feedback reports show exactly
what will be sent and remove names, addresses and secrets first. Using a
cloud AI model is your choice, off by default, and the administrator can
forbid it. Downloads of models and packages are plain file downloads with
no account, revealing nothing beyond what any download does (your network
address and that the file was fetched).

Protects against: silent collection of your data by Basalt.

Does not protect against: what a cloud AI provider you chose does with
what you send it. Also, Fedora's own repositories count active installs
with an anonymous weekly counter; Basalt has not changed that yet.

## What Basalt OS does not promise

- It does not make a computer safe from someone who already has
  administrator rights on it. It makes their actions visible.
- It does not replace backups. Snapshots let you undo system changes; they
  live on the same disk.
- It does not check what Fedora ships, beyond the signatures. Basalt OS is
  built on Fedora and uses Fedora's packages as they are.
- It is not certified against any standard. The catalog maps to known
  references to help reviewers, and we test it in the open with a public
  adversarial suite, [basalt-os/ai-audit-suite](https://github.com/basalt-os/ai-audit-suite).

## Found a problem?

Please report it privately, as described in
[SECURITY.md](../../SECURITY.md).
