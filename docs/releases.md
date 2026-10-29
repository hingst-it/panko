# Publishing a release

`.github/workflows/release.yaml` runs separately from Checks, on pushed tags such as `v0.0.2` or
`v0.0.2-rc.1`. Creating a tag does not itself prove that CI passed: the release workflow verifies
that before building or publishing anything.

The tag must point to a commit in `main`'s history. The newest Checks run from a push to `main`
for that exact commit must have completed successfully, including `unit` and every e2e shard
(`e2e (a)`, `e2e (b)`, `e2e (c)`); skipped jobs do not qualify. A green pull-request run for a
different commit does not qualify. Annotated and lightweight tags are supported.

## Versions

| Artifact | Version form | Example |
| --- | --- | --- |
| Git tag, GitHub release, binaries (`--version`) | with `v` | `v0.0.2`, `panko v0.0.2` |
| Archives, Helm chart `version` and `appVersion`, image tags | without `v` | `panko_0.0.2_linux_amd64.tar.gz`, chart `0.0.2`, `panko-server:0.0.2` |

`charts/panko/Chart.yaml` carries the next release's core version (`0.0.2`) as both `version`
and `appVersion`. The workflow stamps the tag's full version into the chart before the CLI embeds
it (`tools/release.py stamp`), so a release candidate `v0.0.2-rc.1` produces chart `0.0.2-rc.1`,
images `:0.0.2-rc.1` and a CLI whose defaults reference exactly those. A tag whose core version
differs from the committed chart fails verification. `CHANGELOG.md` must have a section for the
core version. There are no floating tags such as `latest`.

## Steps

1. Update `CHANGELOG.md`, `docs/release-notes/vVERSION.md` and the committed chart version, and
   merge to `main`.
2. Wait for that commit's Checks workflow to pass on `main`.
3. Optionally tag a release candidate on the tested commit, let it publish (GitHub prerelease), and
   run the [acceptance test](#acceptance-test) against its published artifacts.
4. Tag the tested commit and push the tag:

   ```sh
   git switch main
   git pull --ff-only
   git tag -a v0.0.2 -m 'Panko 0.0.2'
   git push origin v0.0.2
   ```

5. Watch the Release workflow, then [publish the Homebrew formula](#package-managers) and smoke
   test the published release from an empty directory.

If a tag is pushed before Checks finishes, verification fails without publishing. Wait for Checks
to pass, then re-run the failed Release workflow. Never move or overwrite a published tag; fix
forward with a new version.

## Published artifacts

- `panko_VERSION_OS_ARCH.tar.gz` for Linux and macOS (`darwin`), and `.zip` for Windows, each for
  `amd64` and `arm64`. Each archive contains `panko`, `panko-admin` (compatibility wrapper for
  `panko admin`; `.exe` on Windows), `LICENSE`, `README.md` and `CHANGELOG.md`.
- `panko-server_VERSION_linux_ARCH.tar.gz` for `amd64` and `arm64`: `panko-server` for remote
  agents, with the systemd unit and example configuration from `deploy/agent`.
- `panko-VERSION.tgz`: the Helm chart, also pushed to `oci://ghcr.io/hingst-it/charts/panko`.
- `SHA256SUMS` over all nine archives. These are checksums, not publisher signatures.
- `panko.rb`, generated from the archive checksums for the Homebrew tap.
- Three images with Linux AMD64 and ARM64 manifests: `ghcr.io/hingst-it/panko-server:VERSION`
  (every in-cluster role), `panko-module-mirror:VERSION` (git daemon for the pinned module) and
  `panko-agent:VERSION` (remote agent: `panko-server`, OpenTofu, git). Release 0.0.1 published five
  per-component images with `v`-prefixed tags instead.

All builds check out the verified commit SHA. `tools/release-build.sh` builds the archives and
the chart; it is the same script that local candidate builds use. Before anything becomes public
in the GitHub release, the workflow checks that:

- the packaged chart's default rendering references exactly
  `ghcr.io/hingst-it/panko-server:VERSION` and `panko-module-mirror:VERSION`, every other image is
  pinned, and the chart's OpenTofu engine equals the agent image's (`tools/release.py check`; the
  same check runs in `make lint-chart` on every commit);
- every image exists for both platforms;
- an existing chart version in the registry has identical content (a rerun pushes nothing new);
- images and chart are pullable anonymously.

Images are pushed by their build jobs and the chart by the publish job, so a failed release can
leave some versioned images or the chart published; rerun the failed jobs to finish. Tags with a
SemVer prerelease suffix create GitHub prereleases.

Publishing uses the repository's `GITHUB_TOKEN`: `actions: read` for verification,
`packages: write` for images and the chart, and `contents: write` for the GitHub release. No
personal access token or deploy key is needed by Release or by the default public module
installation.

New GHCR packages start private. When a release creates a package for the first time
(`panko-server`, `panko-agent` and `charts/panko` in 0.0.2), the anonymous-access check fails and
names it: set the package's visibility to **Public** in its package settings, then re-run the
failed job. Organization package policies must allow the workflow to create packages.

## Acceptance test

`test/release_acceptance.py` installs release artifacts the way a user does, in disposable k3d
clusters, without using the checkout for what it tests. Against a published release or candidate:

```sh
mkdir -p .local/dist && gh release download v0.0.2-rc.1 --dir .local/dist
.venv/bin/python test/release_acceptance.py --dist .local/dist --version 0.0.2-rc.1 \
  server mirror dev install gitops migrate
```

Against a local candidate built from a clean checkout (images tagged as the release would tag
them, imported instead of pulled; the gitops phase needs a published OCI chart):

```sh
git worktree add .local/candidate HEAD && cd .local/candidate
for os in linux darwin; do for arch in amd64 arm64; do
  GOOS=$os GOARCH=$arch tools/release-build.sh binaries v0.0.2 ../dist; done; done
tools/release-build.sh chart v0.0.2 ../dist && tools/release-build.sh checksums ../dist
docker build --build-arg VERSION=v0.0.2 -t ghcr.io/hingst-it/panko-server:0.0.2 -f Dockerfile.server .
docker build -t ghcr.io/hingst-it/panko-module-mirror:0.0.2 -f cluster/Dockerfile.module-mirror .
docker build --build-arg VERSION=v0.0.2 -t ghcr.io/hingst-it/panko-agent:0.0.2 -f Dockerfile.agent .
cd ../.. && .venv/bin/python test/release_acceptance.py --dist .local/dist --version 0.0.2 --images docker
```

It needs Docker, internet access, and `k3d`, `helm` and `kubectl` (from `mise.toml`) for the
parts a user brings: an existing cluster and Helm. The `panko` CLI under test is extracted from
its archive into an empty directory and runs with a `PATH` containing only itself and Docker.

## Package managers

[homebrew-tap](https://github.com/hingst-it/homebrew-tap) contains the formula; copy the published
`panko.rb` to `Formula/panko.rb` and commit it for each stable release (not for release
candidates). This cross-repository update intentionally uses no extra CI credential.
[asdf-panko](https://github.com/hingst-it/asdf-panko) discovers version tags and downloads
`panko_VERSION_OS_ARCH.tar.gz` with checksum verification. Mise's GitHub backend consumes the
release archives directly, without a plugin.

Curated release notes belong in `docs/release-notes/vVERSION.md`; release candidates use the
notes of the release they prepare. The workflow generates notes for other tags.
