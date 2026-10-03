# Image signing

Two signing modes, one per place an image is published.

## Public images: keyless (GitHub Actions)

`.github/workflows/image.yml` signs `ghcr.io/basalt-os/basalt-os` with cosign
in keyless mode. The workflow's GitHub OIDC token is exchanged for a short
lived certificate from Sigstore's Fulcio; the signature is recorded in the
Rekor transparency log. There is no private key to store, rotate or leak. The
identity to verify is the workflow file in this repository:

```sh
cosign verify ghcr.io/basalt-os/basalt-os:stable \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/basalt-os/basalt-os/\.github/workflows/image\.yml@'
```

Known gap: the containers signature policy (`/etc/containers/policy.json`,
type `sigstoreSigned`) can match a Fulcio certificate by e-mail subject only,
and a GitHub workflow identity is a URI. So an installed system cannot yet
enforce "signed by this workflow" on `bootc upgrade` for keyless images. Until
the policy format supports it, the options are a long-lived key for release
signing (below) alongside keyless, or verifying with cosign before switching
images. Public images built today carry no signature policy.

## Lab and private registries: a key pair

`scripts/publish.sh` signs with a cosign key pair:

- `COSIGN_KEY`: the encrypted private key; `COSIGN_PASSWORD_FILE`: its
  passphrase. The passphrase is exported into the signing process only, never
  passed on a command line.
- Signatures are pushed to the registry as sigstore attachments
  (`<digest>.sig` tags). The transparency log is not used
  (`--tlog-upload=false`) because the registry is private.
- cosign 2.x is used on purpose: it writes the attachment format that
  containers-image (podman, skopeo, bootc) verifies.

When `SIGNING_PUBKEY` is set at build time, `scripts/site-overlay.sh` adds to
the image:

- `/usr/share/basalt/keys/image-signing.pub`
- `/etc/containers/policy.json`: `sigstoreSigned` with that key for
  `REGISTRY/IMAGE_REPO`; default `reject`; the registries listed in
  `POLICY_ALLOW_REGISTRIES` (docker.io, quay.io, ghcr.io, the Fedora and Red
  Hat registries by default) and local transports accepted without
  signatures. bootc refuses to enforce signatures under a policy whose default
  is `insecureAcceptAnything`, which is the base image's policy;
- `/etc/containers/registries.d/50-basalt.yaml`: `use-sigstore-attachments`
  for that repository.

An installed system that was installed with `--enforce-container-sigpolicy`
follows `ostree-image-signed:docker://REGISTRY/IMAGE_REPO:stable`, and
`bootc upgrade` refuses an image without a valid signature from that key.

## Key handling rules

- Development keys (`make lab-keys`) are created on the build host under
  `$LAB_DIR/keys`, mode 0700 for the directory and 0600 for the private key and
  passphrase. They are for the lab only.
- Release signing keys are never generated or stored on a build host or in
  this repository. They are generated offline and kept in the project's
  secrets vault; the CI uses keyless signing so it needs no key at all.
- Rotation: publish the new public key in a new image first (the policy can
  list two keys), sign with both during the transition, then drop the old one.
