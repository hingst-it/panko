# Quickstart

Panko turns an OpenTofu module into a small, team-scoped infrastructure API. v0.0.2 provisions
PostgreSQL databases in a Kubernetes cluster. It is an experimental release for local evaluation
and personal installations: APIs and configuration may change, and production readiness has not
been established.

There are two parts: the `panko` CLI on your machine and the services in a Kubernetes cluster.
`panko dev up` creates a dedicated local cluster and installs the services; `panko install`
installs them into an existing cluster. Both use the Helm chart embedded in the CLI and the
published images. No repository checkout, Go, `make` or image build is needed.

## 1. Get the CLI

```sh
brew install hingst-it/tap/panko          # or: mise use -g github:hingst-it/panko@0.0.2
panko --version                           # panko v0.0.2
```

Archives with checksums for macOS, Linux and Windows are on the
[release page](https://github.com/hingst-it/panko/releases/tag/v0.0.2); see
[installation](installation.md#get-the-cli) for asdf and manual downloads.

## 2. Start a local lab

Requirements: macOS or Linux, a running Docker engine, and internet access for images, the
module and providers. k3d 5.6 or newer is used if installed; otherwise `panko dev up` offers to
install k3d v5.9.0 privately and verifies its checksum (`--install-k3d` agrees in advance).

```sh
panko dev up
```

This creates the k3d cluster `k3d-panko-dev`, installs Panko with PostgreSQL and a local
S3-compatible service for `team-a` and `team-b`, and maps the API to <http://127.0.0.1:8081>. No
port-forward or other background process is needed. The example module is public and fetched
over HTTPS: **no GitHub account, deploy key, cloud account or cloud credentials are required**.
Your `~/.kube/config` and its current context stay unchanged; the lab's kubeconfig and team tokens
are written with private permissions to `~/.local/state/panko/dev/panko-dev/`.

## 3. Request a database

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
documentation. To sign in to the inspector, read the `team-a` token locally from the token file;
do not paste it into issue reports or an AI chat.

An explicit command reveals the connection URI, including the database password:

```sh
panko --team team-a connect orders
```

The database host in that URI is internal to the cluster. For a local SQL client, run
`kubectl --kubeconfig "$PANKO_KUBECONFIG" -n infra-backends port-forward service/postgres 15432:5432`
in another terminal, then use `127.0.0.1:15432` with the database, username and password from the
URI.

If an apply fails, `get` shows the phase `Failed` and an error code in `status.lastExecution`.
Fix the cause, then request another attempt explicitly: `panko --team team-a retry orders --wait`.

Delete the database through its destroy lifecycle:

```sh
panko --team team-a delete orders --wait --timeout 5m
```

## 4. Clean up

`panko dev status` shows the lab without printing secrets. `panko dev down` removes the lab and
all its data after you type the lab name; only the cluster created for this lab is deleted.

## Install into an existing cluster

Use a dedicated cluster where you have cluster-administrator rights:

```sh
panko install --kubeconfig ./config --context my-cluster --tokens-file ./team-tokens.json
```

The command names the target cluster and context, checks permissions and conflicts, asks for
confirmation, installs or upgrades the Helm release, and verifies readiness. The gateway is a
ClusterIP Service; reach it with
`kubectl --kubeconfig ./config --context my-cluster -n infra-gateway port-forward service/platform-gateway 8081:8080`. See
[installation](installation.md) for values, Helm and private registries, [GitOps](gitops.md) for
Argo CD, Flux and rendered manifests, and [upgrading](upgrading.md) to move a v0.0.1 installation.

## Troubleshooting and next steps

- Check `docker info` if cluster creation cannot reach Docker.
- `panko dev status` shows which component is not ready; `panko --team team-a doctor` checks
  configuration and services. Failed installations name their cause and are resumed by rerunning
  the same command ([troubleshooting](operations.md#troubleshooting)).
- If a resource fails, fix the cause and use `retry NAME --wait`. Do not remove its finalizer to
  force deletion.

See [operations](operations.md), [state backends](state-backends.de.md),
[remote runners](remote-runners.de.md) and [AI-assisted installation](agent-install.md). Panko has
one resource type, static team tokens and shared PostgreSQL administrator credentials; it is not
production tenant isolation. Contributors: see [building from source](installation.md#building-from-source-contributors).
