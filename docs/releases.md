# Publishing a release

`.github/workflows/release.yaml` runs separately from Checks, on pushed tags such
as `v0.0.1` or `v0.0.2-rc.1`. Creating a tag does not itself prove that CI passed:
the release workflow verifies that before building or publishing anything.

The tag must point to a commit in `main`'s history. The newest Checks run from a
push to `main` for that exact commit must have completed successfully, including
both `unit` and `e2e`; skipped jobs do not qualify. A green pull-request run for a
different commit does not qualify. Annotated and lightweight tags are supported.

1. Push or merge the changes to `main`.
2. Wait for that commit's Checks workflow to pass.
3. Tag the tested commit and push the tag:

   ```sh
   git switch main
   git pull --ff-only
   git tag -a v0.0.1 -m 'Panko 0.0.1'
   git push origin v0.0.1
   ```

If a tag is pushed before Checks finishes, verification fails without publishing.
Wait for Checks to pass, then re-run the failed Release workflow in GitHub Actions.
Ordinary CI runs on branch pushes and pull requests, rather than repeating the
tests on tag pushes.

## Published artifacts

- `panko_VERSION_OS_ARCH.tar.gz` for Linux and macOS (`darwin`), and `.zip` for
  Windows, each for `amd64` and `arm64`. Each archive contains `panko`,
  `panko-admin` (with `.exe` on Windows), `LICENSE`, `README.md`, and `CHANGELOG.md`.
- `SHA256SUMS`, covering the six archives. These are checksums, not publisher
  signatures. No paid signing service is used.
- `panko.rb`, generated from the archive checksums for the Homebrew tap.
- Five images, each with Linux AMD64 and ARM64 manifests:
  `ghcr.io/hingst-it/panko-runner:TAG`, `panko-hook:TAG`, `panko-gateway:TAG`,
  `panko-mirror:TAG`, and `panko-module-mirror:TAG` under the same namespace.

All builds check out the verified commit SHA. The GitHub release is created only
after every binary and image build succeeds. Images are pushed by their individual
build jobs, so a failed release can leave some versioned images published; rerun
the failed jobs to finish. There is no floating `latest` image tag. Tags with a
SemVer prerelease suffix create GitHub prereleases.

Publishing uses the repository's `GITHUB_TOKEN`: `actions: read` for verification,
`packages: write` for images, and `contents: write` for the GitHub release. No
personal access token or deploy key is needed by Release or the default public
module installation. Private SSH modules can still use `MODULE_SSH_KEY_FILE`.

When first publishing GHCR packages, set each package's visibility to **Public**
in its package settings for anonymous pulls; a public source repository alone
does not guarantee public package visibility. Organization package policies must
allow the workflow to create packages. Subsequent releases can use the same
packages and token permissions.

These artifacts do not install the Kubernetes services by themselves. The local
lab still uses the repository's manifests and `make` commands with local `:dev`
images. [homebrew-tap](https://github.com/hingst-it/homebrew-tap) contains the formula;
copy the published `panko.rb` to `Formula/panko.rb` and commit it for each new
release. This cross-repository update intentionally uses no extra CI credential.
[asdf-panko](https://github.com/hingst-it/asdf-panko) discovers version tags and
downloads archives with checksum verification. Mise's GitHub backend consumes
the release archives directly, without a plugin.

Curated release notes belong in `docs/release-notes/vVERSION.md`; the workflow
uses that file when present and generates notes for other tags. Update
`CHANGELOG.md` before tagging. The initial release is `v0.0.1`.
