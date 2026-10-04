# Security policy

Basalt OS promises secure defaults, a confined assistant and AI agents that
change nothing without a person's confirmation. A way around SELinux
confinement, the egress policy, the confirmation of a proposal, the audit
trail, the boot chain or package signatures is a security problem. Please
report it privately and give us time to fix it before it is disclosed.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository:
<https://github.com/basalt-os/basalt-os/security/advisories/new>
(the "Report a vulnerability" button under the Security tab). This is the
preferred channel. If you cannot use GitHub, write to
security@basalt-os.org.

Do not open a public issue, pull request or discussion for a security
problem, and do not send it through `basalt feedback` or the feedback form:
those are for bugs and ideas. If the problem is in the desktop shell or in
VSM, you may also report it in
[basalt-os/basalt-shell](https://github.com/basalt-os/basalt-shell) or
[basalt-os/vsm](https://github.com/basalt-os/vsm); we move reports between
repositories.

Please include what you can of:

- the Basalt OS version (`/etc/os-release`) and the versions of the
  `basalt-*` packages involved (`rpm -qa 'basalt-*'`);
- whether SELinux was enforcing;
- steps to reproduce, and what an attacker gains (who the attacker is: an
  AI agent in a session, a local user, a remote party, a package or
  update source);
- whether you want to be credited, and how.

We aim to acknowledge a report within 7 days and to agree on a disclosure
date with you, normally within 90 days of the report or when a fix is
released, whichever comes first. Fixes are published as new packages with
a GitHub security advisory (and a CVE when it applies).

## Supported versions

Basalt OS is pre-alpha. Until a first release, only the latest packages in
the Basalt repository receive security fixes.

## Scope

In scope: the packages built from this repository (basalt-release,
basalt-snapshots, basalt-security, basalt-assistant, basalt-agent,
basalt-resolver, basalt-ledger, basalt-llm, basalt-installer and the
others), their SELinux modules, the kickstart and the installer media.

Out of scope, report upstream: Fedora packages that Basalt OS installs
unchanged, and the AI agents and models a person chooses to run. Problems
in how Basalt OS configures or confines them are in scope.

## Verifying releases

Packages in the Basalt OS repository (<https://obpkg.org/basalt>) are signed
with the OpenBasalt release key, published at
<https://obpkg.org/keys/openbasalt-release-key.asc>. Check its fingerprint
before trusting it:

- primary key `3601 7348 42BD 4E48 2D19  DE4A E4EE D5EC A395 B302`
- packages signing subkey `3024 61D2 6520 E077 D07F  FCA9 AA27 C62C 36CC FC4B`
