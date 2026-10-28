# Quickstart: Panko proof of concept (unified server, Helm chart, `panko dev`)

Panko turns an OpenTofu module into a small, team-scoped infrastructure API. It provisions
PostgreSQL databases in a Kubernetes lab. This branch (`poc/unified-server-dev-install`) is a
proof of concept: two programs (`panko` and `panko-server`), one Helm chart, and a local lab
command. Nothing on this branch is released. For the released 0.0.1, follow the
[0.0.1 quickstart](https://github.com/hingst-it/panko/blob/v0.0.1/docs/quickstart.md).

There are two parts: the `panko` CLI on your machine and the services in a Kubernetes cluster.
`panko dev up` creates a dedicated local cluster and installs the services; `panko install`
installs them into an existing cluster. Both use the Helm chart embedded in the CLI and need no
repository checkout, `make`, or image build on the user's machine.

## Get the CLI and the images

The released CLI (Homebrew `hingst-it/tap/panko`, mise `github:hingst-it/panko@0.0.1`, asdf)
is 0.0.1 and has no `install` or `dev` command. For the proof of concept, build once from the
branch on a machine with Go and Docker:

```sh
git clone --branch poc/unified-server-dev-install https://github.com/hingst-it/panko.git
cd panko
go build -o panko ./cmd/panko
docker build -t panko-server:dev -f Dockerfile.server .
docker build -t panko-module-mirror:dev -f cluster/Dockerfile.module-mirror .
```

To use them elsewhere without a checkout, copy the `panko` binary (built for that OS and
architecture with `GOOS`/`GOARCH`) and the images (`docker save panko-server:dev
panko-module-mirror:dev -o panko-poc-images.tar`, then `docker load -i panko-poc-images.tar`).
The images are not published to a registry; see [installation](installation.md#images-for-the-proof-of-concept).

## Start a local lab

Requirements: macOS or Linux, a running Docker engine, and internet access for images, the
module and providers. k3d 5.6 or newer is used if installed; otherwise `panko dev up` offers to
install k3d v5.9.0 privately and verifies its checksum (`--install-k3d` agrees in advance).

```sh
panko dev up --server-image panko-server:dev --module-mirror-image panko-module-mirror:dev
```

This creates the k3d cluster `k3d-panko-dev`, installs Panko with PostgreSQL and a local
S3-compatible service for `team-a` and `team-b`, and maps the API to
<http://127.0.0.1:8081>. No port-forward or other background process is needed. The example
module is public and fetched over HTTPS: **no GitHub account, deploy key, cloud account, or
cloud credentials are required**. Your `~/.kube/config` and its current context stay unchanged;
the lab's kubeconfig and team tokens are written with private permissions to
`~/.local/state/panko/dev/panko-dev/`.

`dev up` prints the environment to use the lab from any directory:

```sh
export PANKO_URL=http://127.0.0.1:8081
export PANKO_TOKENS_FILE=~/.local/state/panko/dev/panko-dev/team-tokens.json
export PANKO_KUBECONFIG=~/.local/state/panko/dev/panko-dev/kubeconfig
panko --team team-a doctor
panko --team team-a create orders --extensions pgcrypto --connection-limit 20
panko --team team-a wait orders --timeout 5m
panko --team team-a get orders
```

Open <http://127.0.0.1:8081/ui> for the inspector, or <http://127.0.0.1:8081/docs> for API
documentation. To sign in to the inspector, read the `team-a` token locally from the token
file; do not paste it into issue reports or an AI chat.

An explicit command reveals the connection URI, including the database password:

```sh
panko --team team-a connect orders
```

The database host in that URI is internal to the cluster. For a local SQL client, run
`kubectl --kubeconfig "$PANKO_KUBECONFIG" -n infra-backends port-forward service/postgres 15432:5432`
in another terminal, then use `127.0.0.1:15432` with the database, username, and password from
the URI.

Delete the example database through its normal destroy lifecycle:

```sh
panko --team team-a delete orders --wait --timeout 5m
```

`panko dev status` shows the lab without printing secrets. To remove the lab and all its data,
run `panko dev down` and type the lab name. Only the cluster created for this lab is deleted.

## Install into an existing cluster

Use a dedicated test cluster. The images must be pullable there (push them to your registry
and set `images.server.repository`/`tag` and `images.moduleMirror.repository`/`tag`):

```sh
panko install --kubeconfig ./config --context mein-cluster --values values.yaml \
  --tokens-file ./team-tokens.json
```

The command names the target cluster and context, checks permissions and conflicts, asks for
confirmation, installs or upgrades the Helm release, and verifies readiness. See
[installation](installation.md) for details and [GitOps](gitops.md) for `helm template`,
Argo CD and Flux.

## From a checkout (contributors)

The repository's pinned tools are in `mise.toml`. The make targets use the same chart and
`panko install` with locally built images:

```sh
mise trust && mise install
export CLUSTER_NAME=panko-quickstart
make cluster-up build deploy
make gateway-forward    # second terminal; the lab cluster from make has no port mapping
.local/bin/panko --team team-a doctor
```

`make cluster-up` writes `.local/kubeconfig`, `make deploy` writes `.local/team-tokens.json`,
and `make CLUSTER_NAME=panko-quickstart cluster-down` removes that cluster.

## Troubleshooting and next steps

- Check `docker info` if cluster creation cannot reach Docker.
- `panko dev status` shows which component is not ready; `panko --team team-a doctor` checks
  configuration and services.
- If a resource fails, inspect its status and error code with `get`; fix the cause, then use
  `retry NAME --wait`. Do not remove its finalizer to force deletion.
- Full verification from a checkout: create `.venv`, install `requirements-dev.txt`, then run
  `make test` against a lab from `make cluster-up build deploy`. It creates additional
  temporary resources and can take a long time.

See [operations](operations.de.md), [state backends](state-backends.de.md),
[AI-assisted installation](agent-install.md), and the
[proof-of-concept evaluation](poc-unified-server.de.md). This proof of concept has one resource
type, static team tokens, and shared PostgreSQL administrator credentials. It is for local
evaluation and personal installations, not production tenant isolation.
