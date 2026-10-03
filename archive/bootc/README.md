# Archived: image-based (bootc) prototype

An earlier milestone 0 built Basalt OS as a bootable container image on the
Fedora bootc base: an OCI image pipeline, cosign signing, a lab registry, a
`basalt-install` wrapper around `bootc install`, `bootc upgrade` and
`bootc rollback`. Basalt OS then moved to a traditional, package-based model
(see `docs/design.md` at the top of this repository), so this code is no
longer built or maintained. It is kept for reference: its findings on SSH
hardening, the firewall, branding, LUKS2 with TPM2 and the lab are reused, and
are summarized in `docs/milestone-0-bootc-report.md`.

Paths inside these files refer to the old layout of the repository.
