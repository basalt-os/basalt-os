# Versions and release stages

Basalt OS versions start with the major version of the Fedora release they
are built on, followed by the Basalt release number:

- `44.0` is the first official Basalt OS release on Fedora 44.
- `44.1` is a later fix release on the same base.
- `45.0` is the first official release on Fedora 45.

## Stages before an official release

| Stage | Example | Who it is for |
|---|---|---|
| dev | `44.0-dev.20261005` | the project team; never published as a release |
| alpha | `44.0-alpha.1` | early testers; features may still be missing |
| beta | `44.0-beta.1` | anyone who wants to test; feature freeze, fixes only |
| release candidate | `44.0-rc.1` | becomes the official release if nothing blocks it |
| official | `44.0` | everyone |

## Image names

Images are named `basalt-os-<version>-<edition>-<arch>.iso`, for example
`basalt-os-44.0-alpha.1-desktop-x86_64.iso` or
`basalt-os-44.0-alpha.1-server-x86_64.iso`. Each set comes with a
`SHA256SUMS` file signed by the OpenBasalt release key
(https://obpkg.org/keys/openbasalt-release-key.asc).

## Build inputs

The image and package builds read the version from the `VERSION` file
(`44.0`) and the stage from `BASALT_STAGE`: `dev` (the default), `alpha.N`,
`beta.N`, `rc.N` or `final`. A dev build adds `BASALT_BUILD`, the build
date (default today, UTC): `BASALT_STAGE=alpha.1 make live-iso` names the
image `basalt-os-44.0-alpha.1-server-x86_64.iso`, a plain `make live-iso`
`basalt-os-44.0-dev.20261005-server-x86_64.iso`.

`basalt-release` writes them into `/etc/os-release`:

```
VERSION="44.0 (dev)"
VERSION_ID=44.0
PLATFORM_ID="platform:f44"
BASALT_VERSION=44.0
BASALT_STAGE=dev
BASALT_BUILD=20261005
PRETTY_NAME="Basalt OS 44.0 (dev)"
```

`VERSION_ID`'s major number is the Fedora release; `PLATFORM_ID` and
dnf's `$releasever` carry it as well. An official release has
`VERSION="44.0"` and `BASALT_STAGE=final`.

## Where to find them

- Test stages (alpha, beta, release candidates): https://obpkg.org/iso/testing/
- Official releases: https://obpkg.org/iso/<version>/, for example
  https://obpkg.org/iso/44.0/

## Package channels

- `basalt`: stable packages and the packages of official releases.
- `basalt-testing`: packages for test stages; off by default.
- `basalt-tools`: official OpenBasalt tools.

No image has been published yet. The first published image will be an
alpha.
