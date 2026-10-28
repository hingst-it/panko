# Installation: `panko install`, `panko dev`, Helm and GitOps

Panko v0.0.2 is an experimental release for local evaluation and personal installations. It
ships as two programs, three images and one Helm chart:

- `panko` manages resources (`create`, `get`, `retry`, ...), administers an installation
  (`panko admin ...`), installs it (`panko install`) and runs a local lab (`panko dev`). It embeds
  the Helm chart of its own release, so none of these commands needs a checkout, Go, `make` or an
  image build. `panko-admin` remains in the archives as a compatibility wrapper for
  `panko admin`.
- `panko-server` is the runtime binary. Each pod or Job selects one role: `gateway`,
  `controller` (with the embedded Metacontroller), `endpoint`, `mirror`, `runner`, `agent` and
  `setup`.
- Images, for Linux AMD64 and ARM64, anonymously pullable:
  `ghcr.io/hingst-it/panko-server:0.0.2` (every in-cluster role), `ghcr.io/hingst-it/panko-module-mirror:0.0.2`
  (git daemon serving the pinned module) and `ghcr.io/hingst-it/panko-agent:0.0.2` (remote agent:
  `panko-server` with OpenTofu 1.11.7 and git).
- The chart `oci://ghcr.io/hingst-it/charts/panko`, version `0.0.2`. Its `appVersion` is the
  default image tag, so pinning the chart version pins the images. Every installation path uses
  this chart: `panko install`, `panko dev up`, Helm, Argo CD, Flux and rendered manifests.

## Supported platforms and prerequisites

| Component | Supported |
| --- | --- |
| `panko` CLI | macOS and Linux on AMD64 and ARM64; Windows AMD64 and ARM64 for resource and admin commands (`panko dev` needs macOS or Linux) |
| `panko-server` binary (remote agents, systemd) | Linux AMD64 and ARM64 |
| Images | Linux AMD64 and ARM64 |
| Cluster | Kubernetes 1.29 or newer with a default StorageClass; validated on k3s v1.35 (k3d 5.9) |
| `panko dev` | macOS or Linux with a running Docker engine; k3d 5.6 or newer, or the pinned k3d v5.9.0 installed privately |

Not supported: Podman or other container runtimes for `panko dev`, more than one controller
replica (see [operations](operations.md#supported-configuration)), OIDC, and Terraform CLI
execution (OpenTofu only). Installing into a cluster needs cluster-administrator rights.

## Get the CLI

```sh
brew install hingst-it/tap/panko          # Homebrew (macOS, Linux)
mise use -g github:hingst-it/panko@0.0.2  # mise, GitHub backend, no plugin
asdf plugin add panko https://github.com/hingst-it/asdf-panko.git && asdf install panko 0.0.2
```

Or download `panko_0.0.2_<os>_<arch>.tar.gz` (`.zip` for Windows) and `SHA256SUMS` from the
[release](https://github.com/hingst-it/panko/releases/tag/v0.0.2), check it with
`sha256sum --check --ignore-missing SHA256SUMS` (macOS: `shasum -a 256 -c --ignore-missing SHA256SUMS`)
and put `panko` on your `PATH`. `panko --version` prints `panko v0.0.2`. The checksums are not
publisher signatures. Installing the CLI does not deploy anything into a cluster.

## Local lab: `panko dev`

```sh
panko dev up                 # create or resume the lab, install the chart, wait for readiness
panko dev status             # cluster, release, workloads and API reachability; no secrets
panko dev down               # delete this lab only, after typing its name
```

`dev up`:

1. checks that Docker runs and finds a supported k3d on `PATH`, or offers to install k3d v5.9.0
   privately into the state directory with its SHA-256 verified against the checksum pinned in the
   CLI (`--install-k3d` agrees in advance; `PATH` is not changed);
2. creates a dedicated k3d cluster `k3d-<name>` (default name `panko-dev`) with its own
   kubeconfig. Your `~/.kube/config` and its current context are not changed;
3. installs the chart embedded in the CLI with lab values (PostgreSQL, a local S3 service, the
   gateway on NodePort 30081, the runner endpoint on NodePort 30443). The cluster pulls the
   release images; `--server-image` and `--module-mirror-image` select other images, which are
   imported from the local Docker daemon when present there;
4. maps the API to `127.0.0.1:<port>` (default 8081) through k3d's Docker port mapping. **No
   background process is started**;
5. waits for the release, the setup Job, the controller and the gateway, writes a private copy of
   the team tokens, and prints the next steps.

State lives in `$PANKO_HOME`, else `$XDG_STATE_HOME/panko`, else `~/.local/state/panko`:
`dev/<name>/lab.json` (no secrets), `kubeconfig` and `team-tokens.json` (both mode 0600), and
`bin/k3d-5.9.0` if it was installed.

Repeating `dev up` keeps a compatible lab: it starts a stopped cluster, upgrades the release in
place (also to the chart of a newer CLI) and keeps resources, state and tokens. An interrupted
`dev up` resumes. A k3d cluster with the same name that this lab did not create is refused and left
unchanged; choose another `--name`. A port in use is refused before anything is created; pass
`--port`. `dev down` deletes only the cluster carrying this lab's owner label and its state
directory, after you type the lab name (or `--yes`).

## Existing cluster: `panko install`

```sh
panko install --kubeconfig ./config --context my-cluster [--values values.yaml] [--set key=value] \
  [--config panko.json] [--tokens-file ./team-tokens.json] [--yes]
```

Use a dedicated cluster. The command never edits the kubeconfig and prints the cluster it targets
(API server, context, kubeconfig, release and chart version) before asking for confirmation. It
then:

1. renders the chart with your values, so a configuration error stops before any cluster access;
2. checks the API server (Kubernetes 1.29 or newer) and, with `SelfSubjectAccessReview`, the
   permissions it needs, listing every missing one;
3. refuses an installation made without Helm (v0.0.1, or rendered manifests) unless
   `--take-ownership` is given, and a separately running Metacontroller in namespace
   `metacontroller` ([migration](upgrading.md#migrating-a-v001-installation));
4. for an existing installation, refuses unsafe changes: removing a team that still has
   resources, legacy state that needs `panko admin migrate-state`, and removing runner pools that
   resources are bound to;
5. applies the CRDs, reruns the setup Job, installs or upgrades the Helm release
   `panko-system/panko`, and waits for Deployments, StatefulSets and Jobs. If that fails, it lists
   pods that cannot start, with their image and a hint ([troubleshooting](operations.md#troubleshooting));
6. verifies the controller's `/readyz` (embedded Metacontroller synced) and the gateway, and with
   `--tokens-file` writes a private copy of the team tokens.

The release is applied with the field manager `helm`, so an installation can alternate between
`panko install` and `helm upgrade`. A failed upgrade is not rolled back; rerunning the same
command resumes. Runs are serialized by the Lease `panko-system/panko-install`; a run interrupted
while it held the Lease is resumed by the next one, while a pending Helm revision of another tool
is refused unless `--recover-pending` is given.

`panko.json` (`--config`) is the same document as the chart's `installation` values: teams,
`s3`, `egress`, `egressTargets`, `runnerPools` and `runnerEndpoint`. Value layers, lowest
first: chart defaults, `panko dev` lab settings, `--config`, `--values` files, `--set`.

## Helm directly

```sh
helm install panko oci://ghcr.io/hingst-it/charts/panko --version 0.0.2 \
  --namespace panko-system --create-namespace --values values.yaml --wait --timeout 15m
```

Helm installs `crds/` on the first install only. Before every `helm upgrade`, apply the new
chart's CRDs: `helm show crds oci://ghcr.io/hingst-it/charts/panko --version NEW | kubectl apply --server-side -f -`.
Without `panko install` there are no pre-checks; read the list above before removing teams or
pools. Retrieve the team tokens with
`panko admin --kubeconfig ./config --config cluster --tokens-file ./team-tokens.json ensure-gateway-auth`.
The chart's values are documented in [values.yaml](../charts/panko/values.yaml) and summarized in
the [chart README](../charts/panko/README.md). For Argo CD, Flux and rendered manifests see
[GitOps](gitops.md).

### Images from a private registry

To pull from your own registry, copy the three images with their tags, set
`images.server.repository`, `images.moduleMirror.repository` (and `tag` if it differs from the
chart version) and name a pull Secret in `images.pullSecrets`. The Secret must exist in
`infra-runners`, `infra-gateway`, `infra-backends` and every team namespace: runner Jobs pull
`panko-server` through the teams' `tofu-runner` ServiceAccounts. The chart creates these
namespaces; to create the Secrets before the first install, create the namespaces yourself and let
Helm adopt them (`helm install --take-ownership ...`), or create the Secrets right after the
install starts — pods retry the pull. The release acceptance test covers this with a
password-protected registry that refuses anonymous pulls.

## Responsibilities of Helm and the CLI

| Concern | Helm chart | Setup Job (`panko-server setup`) | `panko install` |
|---|---|---|---|
| CRDs | `crds/`, created on first install only | — | applied before every upgrade; obsolete short names removed |
| Namespaces, RBAC, workloads, Services, CompositeController, config | rendered, deterministic | — | — |
| Credentials (Postgres admin, gateway tokens, mirror and endpoint TLS, agent registry, S3 keys) | never rendered | created if missing, never replaced (`secrets.generate=true`), or only verified (`false`) | — |
| Certificate renewal | daily CronJob | renews 30 days before expiry from stable CAs | reruns setup |
| Mirror CA copies (`panko-mirror-ca`) and egress NetworkPolicies | — | derived from the live cluster | — |
| Pre-checks (permissions, legacy installs, teams, pools, legacy state) | — | — | yes |
| Local copy of team tokens | — | — | `--tokens-file` |

Nothing that `panko install` does is required for a working installation: the checks prevent
mistakes, the CRD step corresponds to `kubectl apply --server-side` of `crds/` (or Flux's
`crds: CreateReplace`), and the token file is a convenience.

## CRDs and existing resources

Helm never upgrades or deletes `crds/`. `panko install` server-side applies Panko's CRDs before
each upgrade. Metacontroller's CRDs are created when missing and upgraded only if Panko applied
them before; CRDs of another Metacontroller installation keep their version. CRD changes stay
backward compatible: existing Database, StateBackend, BackendProfile and Run objects are never
converted or recreated. v0.0.2 removes the short name `db` and adds the category `panko`
([naming](naming.md)).

Uninstalling the release keeps the CRDs, every team resource, state, credentials, namespaces and
data volumes; see [uninstalling and cleanup](operations.md#uninstalling-and-cleaning-up-retained-objects).

## Building from source (contributors)

The user installation above needs no checkout. Contributors use the pinned tools in
[mise.toml](../mise.toml): `make cluster-up build deploy` builds `panko-server` and the module
mirror image, imports them into a k3d lab and runs `panko install` with
[cluster/lab-values.yaml](../cluster/lab-values.yaml). `tools/release-build.sh` builds the release
archives and chart exactly as the Release workflow does ([releases](releases.md)).
