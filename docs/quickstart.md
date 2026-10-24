# Quickstart: Panko 0.0.1

Panko turns an OpenTofu module into a small, team-scoped infrastructure API.
This first experimental release provisions PostgreSQL databases in a local
Kubernetes lab. Expect API and configuration changes before a stable release.

There are two parts: the `panko` CLI on your machine and the services in a
Kubernetes cluster. Installing the CLI alone does not deploy those services.

## Install the CLI

Choose one method. All three install `panko` and `panko-admin` from the same
release. No Apple developer certificate or paid signing service is needed for
the documented Homebrew and mise installation paths.

### Homebrew (macOS or Linux)

```sh
brew install hingst-it/tap/panko
panko --version
```

### mise (macOS, Linux, or Windows)

```sh
mise use -g github:hingst-it/panko@0.0.1
mise exec github:hingst-it/panko@0.0.1 -- panko --version
```

If mise is activated in your shell, `panko` is also directly on `PATH`. Use
`mise use github:hingst-it/panko@0.0.1` without `-g` to pin a project instead.

### asdf (macOS or Linux)

```sh
asdf plugin add panko https://github.com/hingst-it/asdf-panko.git
asdf install panko 0.0.1
asdf set -u panko 0.0.1
panko --version
```

The plugin verifies the release archive against its published SHA-256 checksum.
It needs `git`, `curl`, `tar`, and either `sha256sum` or `shasum`.

### Manual download

Download the archive for your OS and architecture and `SHA256SUMS` from
[v0.0.1](https://github.com/hingst-it/panko/releases/tag/v0.0.1). Verify the archive
with `sha256sum --ignore-missing --check SHA256SUMS` on Linux, or compare
`shasum -a 256 ARCHIVE` with its exact entry on macOS. Extract the archive and put
`panko` and `panko-admin` on `PATH`. On Windows the executables end in `.exe`.
The executables are not signed with Apple Developer ID or Windows Authenticode;
browser downloads may require operating-system approval. The checksums detect
changed bytes but are not a publisher signature.

## Deploy a local lab

The supported cluster setup uses macOS or Linux, a running Docker engine,
`git`, `make`, and [mise](https://mise.jdx.dev/). On Windows use a Linux environment
with Docker access for the lab; the Windows CLI can talk to an existing gateway.
The repository supplies pinned tools and builds the service images locally.
Internet access is needed for tools, images, modules, and provider downloads.

Use a fresh directory so that an existing installation's configuration and token
files are kept separate:

```sh
git clone --branch v0.0.1 --depth 1 https://github.com/hingst-it/panko.git panko-0.0.1
cd panko-0.0.1
mise trust
mise install
export CLUSTER_NAME=panko-quickstart
make cluster-up bootstrap build deploy
```

The example module is public and fetched over HTTPS: **no GitHub account, deploy
key, cloud account, or cloud credentials are required**. The default configuration
creates `team-a`, `team-b`, PostgreSQL, and a local S3-compatible service.
`panko.json` controls teams, optional S3, and optional egress restrictions.

The kubeconfig is written to `.local/kubeconfig`; existing default Kubernetes
contexts are not modified. Team bearer tokens are created in
`.local/team-tokens.json` with private permissions. Run the following commands
from this checkout, where the CLI can find those files.

In a second terminal, from the same checkout, leave the gateway forward running:

```sh
make gateway-forward
```

Back in the first terminal, use the installed CLI or the source-built one:

```sh
.local/bin/panko --team team-a doctor
.local/bin/panko --team team-a create orders --extensions pgcrypto --connection-limit 20
.local/bin/panko --team team-a wait orders --timeout 5m
.local/bin/panko --team team-a get orders
```

Open <http://127.0.0.1:8081/ui> for the inspector, or
<http://127.0.0.1:8081/docs> for API documentation. To sign in to the inspector,
read the `team-a` token locally from `.local/team-tokens.json`; do not paste it
into issue reports or an AI chat.

An explicit command reveals the connection URI, including the database password:

```sh
.local/bin/panko --team team-a connect orders
```

The database host in that URI is internal to the cluster. For a local SQL client,
run `mise exec -- kubectl --kubeconfig .local/kubeconfig -n infra-backends
port-forward service/postgres 15432:5432` in another terminal (as one command),
then use `127.0.0.1:15432` with the database, username, and password from the URI.

Delete the example database through its normal destroy lifecycle:

```sh
.local/bin/panko --team team-a delete orders --wait --timeout 5m
```

To remove the entire quickstart lab and all its data, explicitly name it:

```sh
make CLUSTER_NAME=panko-quickstart cluster-down
```

## Troubleshooting and next steps

- Check `docker info` if cluster creation cannot reach Docker.
- Keep `make gateway-forward` running when using the CLI. Its default URL is
  `http://127.0.0.1:8081`; override with `--url` or `PANKO_URL`.
- Run `.local/bin/panko --team team-a doctor` for configuration and service checks.
- If a resource fails, inspect its status and error code with `get`; fix the
  cause, then use `retry NAME --wait`. Do not remove its finalizer to force deletion.
- Full verification is optional for a quickstart: create `.venv`, install
  `requirements-dev.txt`, then run `make test`. It creates additional temporary
  resources and can take about 20 minutes.

See [operations](operations.de.md), [state backends](state-backends.de.md), and
[AI-assisted installation](agent-install.md). This release has one resource type,
static team tokens, and shared PostgreSQL administrator credentials. It is for
local evaluation and personal installations, not production tenant isolation.
