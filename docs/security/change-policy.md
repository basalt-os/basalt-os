# Changing the security model

The security model is a promise. This page says how the promise changes,
how exceptions are recorded, and where to report a problem with it.

## What counts as a change

- adding, removing or rewording a control's requirement in
  [controls.yaml](controls.yaml);
- changing a control's status (planned to partial, partial to implemented,
  or back);
- changing where a control is enforced or how it is verified, when the
  enforcement or the verification gets weaker;
- changing a default that a control depends on (for example a SELinux
  boolean, an allowlist, a systemd hardening option, a key, a repository
  setting).

Fixing a typo, a path that moved, or a test that was renamed is
maintenance, not a change of the model, but it still goes through the
catalog check.

## How a control changes

1. A design decision first. Every change of a requirement, of a default a
   control depends on, or of a status other than "implemented as planned"
   is decided in a design decision record (ADR). The decision states which
   control IDs it touches. New controls get the next free ID of their area;
   IDs are never reused.
2. The catalog in the same change. The code change that implements,
   weakens or removes a control updates `docs/security/controls.yaml` in
   the same commit or pull request, then regenerates `controls.md`
   (`scripts/ci/security-controls-check.sh --render`). A pull request that
   touches a security path without touching the catalog must say in its
   description why the catalog does not change.
3. Review. Changes to security paths (SELinux modules, systemd units of
   Basalt services, polkit actions, the ledger, the resolver, basalt-agent,
   the assistant's actions and validators, release and signing scripts,
   repository files and keys, and `docs/security/`) are reviewed by the
   project owner, with an automated review of the pull request as a second
   reader. The owner approves every change of a control.
4. Version. The catalog's `version` increases by one for every change of a
   requirement or status, and `updated` carries the date. A note goes into
   the changelog below.
5. CI. `scripts/ci/security-controls-check.sh` runs in `scripts/ci/lint.sh`
   for code changes and in its own workflow for documentation changes. A
   catalog that names a missing path, a missing test or a missing audit
   step fails the build.

Weakening a control is allowed only through this process. "We will fix the
catalog later" is not.

## Deviations and exceptions

Sometimes a control cannot be met for a while (a dependency is missing, a
decision is pending) or a specific installation needs an exception.

- A project-wide deviation is recorded in the control itself: the status
  is set honestly (partial or planned) and `gap` says what is missing and
  why. Example: BSC-PKG-007 records that one signing subkey signs every
  repository today, although the decision calls for one per family.
- An exception for one machine or site (for example turning on
  `basalt_agent_direct_egress`, or installing without disk encryption) is
  an administrator choice. Where Basalt offers the switch, the choice is a
  confirmed action and is recorded in the audit ledger; administrators
  should also write down why. The periodic audit lists every deviation it
  finds on the machines it audits.
- An exception never covers the locked list of the approval gate (turning
  SELinux off, replacing Basalt policy, Secure Boot keys, disabling or
  rewriting the ledger) without the unlock procedure that list requires.

## Audits

The catalog is checked on every change by CI, before every release by the
release checklist in [audit-guide.md](audit-guide.md), and periodically by
a full run of the audit guide on a lab installation. Gaps found by an audit
become work items, and their effect on a control's status is recorded here
through the process above.

## Reporting a vulnerability

A way around any control in this catalog is a security problem. Report it
privately as described in [SECURITY.md](../../SECURITY.md); do not open a
public issue.

## Releases of the security model

The security model (these documents and the catalog) is released on its
own, with versions that say how mature it is:

- 0.x drafts while Basalt OS is pre-alpha and alpha: requirements may still
  change, and planned controls are expected.
- release candidates (1.0-rc.N) once every control needed for the first
  stable release is implemented or has a recorded exception.
- 1.0, published together with the first stable Basalt OS release, then
  1.x and 2.0 following the same rules.

Each release is a Git tag `security-model-vX.Y[-stage]` with a GitHub
release whose notes summarize the changelog below and the audit report it
relies on. The catalog's integer `version` keeps counting every change in
between releases.

## Changelog of the security model

- Version 1 (2026-10-06, released as security model 0.1, draft): first catalog, 97 controls in 13 areas, with the
  threat model, the audit guide and this policy.
- Version 2 (2026-10-06): approval gate phase 2, first wave. BSC-GATE-007 is
  partial (the gate is built; paths move to it one by one, enforced per path
  in gate.conf, shadow mode elsewhere); BSC-GATE-003 is also enforced by the
  gate for the system assistant's proposals (the short code is the
  fingerprint, root's confirmation is recorded as the person's decision).
- Version 3 (2026-10-06): where the approval gate decides the desktop shell's
  proposals, BSC-GATE-001 holds at the gate too (only the shell UI's domain
  decides there; the shell daemon refuses to decide those itself) and
  BSC-GATE-005 is also enforced by the gate's registry.
