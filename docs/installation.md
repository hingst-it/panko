# Installation: `panko install`, `panko dev`, and the Helm chart

> Proof of concept on branch `poc/unified-server-dev-install`. Nothing here is released; the
> images are built locally (see [Images](#images-for-the-proof-of-concept)).

Panko ships as two programs and one Helm chart:

- `panko` manages resources (`create`, `get`, `retry`, ...), administers an installation
  (`panko admin ...`), installs it (`panko install`) and runs a local lab (`panko dev`).
  It embeds the Helm chart, so none of these commands needs a repository checkout or `make`.
- `panko-server` is the single runtime binary. Each pod or Job selects one role:
  `gateway`, `controller` (with the embedded Metacontroller), `endpoint`, `mirror`, `runner`,
  `agent` and `setup`. All in-cluster roles use the image `panko-server`; the module mirror
  (`panko-module-mirror`, git daemon) and the remote agent (`Dockerfile.agent`, with OpenTofu
  and git) are separate images.
- `charts/panko` is the chart that `panko install`, `panko dev up` and GitOps all use.
  See [GitOps](gitops.md) for `helm template`, Argo CD, and Flux.

## Local lab: `panko dev`

Requirements: macOS or Linux, a running Docker engine, and internet access for images, the
module and providers. k3d 5.6 or newer is used if it is on `PATH`; otherwise `panko dev up`
offers to install k3d v5.9.0 privately (into the state directory, never on `PATH`) and verifies
its SHA-256 against the checksum pinned in the CLI. Podman, bundled runtimes and VM management
are not supported.

```sh
panko dev up                 # create or resume the lab, install the chart, wait for readiness
panko dev status             # cluster, release, workloads and API reachability; no secrets
panko dev down               # delete this lab only, after typing its name
```

`dev up`:

1. checks that Docker runs and finds a supported k3d (or installs it after you agree, or with
   `--install-k3d`);
2. creates a dedicated k3d cluster `k3d-<name>` (default name `panko-dev`) with one agent node,
   labels its server container with the lab's ID, and writes its kubeconfig to the lab's state
   directory. Your `~/.kube/config` and its current context are not changed; k3d itself runs
   with a lab-private `KUBECONFIG`;
3. imports images that exist in the local Docker daemon (`--server-image`,
   `--module-mirror-image`), otherwise the cluster pulls them;
4. runs the same steps as `panko install` with lab values: local S3, PostgreSQL, the gateway
   on NodePort 30081 and the runner endpoint on NodePort 30443;
5. maps the gateway to `127.0.0.1:<port>` (default 8081) through k3d's Docker port mapping.
   **No background process is started**, so there is nothing to stop besides the cluster;
6. waits for the release, the setup Job, the controller's `/readyz` and the gateway, writes a
   private copy of the team tokens, and prints the next steps.

State lives in `$PANKO_HOME`, else `$XDG_STATE_HOME/panko`, else `~/.local/state/panko`:
`dev/<name>/lab.json` (no secrets), `kubeconfig` and `team-tokens.json` (both mode 0600),
and `bin/k3d-5.9.0` if it was installed. Use the lab from any directory:

```sh
export PANKO_URL=http://127.0.0.1:8081
export PANKO_TOKENS_FILE=~/.local/state/panko/dev/panko-dev/team-tokens.json
export PANKO_KUBECONFIG=~/.local/state/panko/dev/panko-dev/kubeconfig
panko --team team-a create orders --extensions pgcrypto
panko --team team-a wait orders --timeout 5m
```

Repeating `dev up` keeps a compatible lab: it starts a stopped cluster, upgrades the release in
place, and keeps resources, state and tokens. An interrupted `dev up` resumes from where it
stopped. A k3d cluster with the same name that this lab did not create (no state, or a different
owner label) is refused and left unchanged; choose another `--name`. A port in use is refused
before anything is created; pass `--port`. `dev down` deletes only the cluster carrying this
lab's owner label and its state directory, after you type the lab name (or `--yes`).

## Existing cluster: `panko install`

```sh
panko install --kubeconfig ./config --context mein-cluster [--values values.yaml] [--set key=value] \
  [--config panko.json] [--tokens-file ./team-tokens.json] [--yes]
```

The command never edits the kubeconfig and always prints the cluster it targets (API server,
context, kubeconfig, release) before asking for confirmation. It then:

1. renders the chart with your values, so a configuration error stops before any change;
2. checks the API server (Kubernetes 1.29 or newer) and, with `SelfSubjectAccessReview`, the
   permissions it needs: CRDs, namespaces, cluster roles and bindings, roles, workloads, Jobs,
   Secrets and CompositeControllers;
3. refuses an installation made without Helm (`make deploy` before this proof of concept, or
   pre-rendered manifests) unless `--take-ownership` is given, and refuses a separately running
   Metacontroller in namespace `metacontroller` unless `--allow-foreign-metacontroller` is given,
   because both would reconcile the same CompositeController. `--take-ownership` adopts the
   existing objects into the release (forcing conflicts with `kubectl apply`'s field manager)
   and replaces the stateless Deployments that `kubectl apply` created; Secrets, StatefulSets
   and volumes are kept. See the [migration](poc-unified-server.de.md#migration);
4. for an existing installation, refuses unsafe changes: legacy state secrets that still need
   `panko admin migrate-state`, and runner pools that resources are bound to but the new values
   no longer declare;
5. applies the CRDs (see below), installs or upgrades the Helm release `panko-system/panko`,
   and waits for Deployments, StatefulSets and Jobs;
6. verifies the controller's `/readyz` (embedded Metacontroller synced) and the gateway through
   its Service, and with `--tokens-file` writes a private copy of the team tokens.

The release is applied with the field manager `helm`, like the helm CLI, so an installation can
alternate between `panko install` and `helm upgrade`; GitOps controllers use their own managers
(Flux: `helm-controller`) and may need to take over fields once when an installation moves to them.
A failed upgrade is not rolled back. Panko's state lives in its own objects, so rerunning the
same command resumes. Runs are serialized by the Lease `panko-system/panko-install`: a second
`panko install` waits up to 45 seconds and then refuses while the first is active. A run that can
no longer be sure it holds the Lease cancels all its changes, including the Helm operation: when
another holder appears, and in any case 20 seconds after the last successful renewal, even while a
renewal request still hangs; the Lease lasts 30 seconds. Directly after
acquiring the Lease and before any change, a pending Helm revision is checked: it is recovered
(marked failed, then installed again) only when the previous `panko install` holding the Lease
stopped without releasing it. A pending revision from Helm, Flux or another tool may still be in
progress, so `panko install` refuses it and changes nothing unless `--recover-pending` is given. Every run also starts the setup Job again, so a deleted credential is recreated, and
it refuses to remove a team that still has resources.

`panko.json` (`--config`) is the same document as the chart's `installation` values: teams,
`s3`, `egress`, `egressTargets`, `runnerPools` and `runnerEndpoint`. Value layers, lowest
first: chart defaults, `panko dev` lab settings, `--config`, `--values` files, `--set`.

### Responsibilities of Helm and the CLI

| Concern | Helm chart | Setup Job (`panko-server setup`) | `panko install` |
|---|---|---|---|
| CRDs | `crds/`, created on first install only | — | applied before every upgrade |
| Namespaces, RBAC, workloads, Services, CompositeController, config | rendered, deterministic | — | — |
| Credentials (Postgres admin, gateway tokens, mirror and endpoint TLS, agent registry, S3 keys) | never rendered | created if missing, never replaced (`secrets.generate=true`), or only verified (`false`) | — |
| Installation ID | — | created once | — |
| Mirror CA copies (`panko-mirror-ca`) and egress NetworkPolicies | — | derived from the live cluster | — |
| Pre-checks (permissions, legacy installs, pools, legacy state) | — | — | yes |
| Local copy of team tokens | — | — | `--tokens-file` |

Nothing that `panko install` does is required for a working installation: the checks prevent
mistakes, the CRD step corresponds to `kubectl apply --server-side` of `crds/` (or Flux's
`crds: CreateReplace`), and the token file is a convenience. GitOps installations get everything else from the chart and
its setup Job.

### CRDs and existing resources

Helm installs `crds/` on the first install and never upgrades or deletes them. `panko install`
therefore server-side applies Panko's CRDs (field manager `panko`) before each upgrade.
Metacontroller's CRDs are created when missing and upgraded only if Panko applied them before;
CRDs of another Metacontroller installation keep their version. CRD changes must stay backward
compatible (new optional fields, new versions), because existing Database, StateBackend and Run
objects are never converted or recreated.

Uninstalling the release (`helm uninstall` or deleting the GitOps application) keeps the CRDs,
every team resource, state, credentials, namespaces and data volumes: namespaces, the lab
StatefulSets, the provider cache and the CompositeController carry `helm.sh/resource-policy: keep`
and the matching Argo CD and Flux annotations. Removing them is a deliberate manual step. The
CompositeController must stay: Metacontroller finalizes a deleted CompositeController by removing
its finalizer from every Database, so deleting it would drop the guarantee that a Database is
only removed after its destroy.

## Images for the proof of concept

The images are not published. Build them from the branch and make them available to the cluster:

```sh
docker build -t panko-server:dev -f Dockerfile.server .
docker build -t panko-module-mirror:dev -f cluster/Dockerfile.module-mirror .
```

- `panko dev up --server-image panko-server:dev --module-mirror-image panko-module-mirror:dev`
  imports local images into the lab.
- Another machine needs no checkout: `docker save panko-server:dev panko-module-mirror:dev -o panko-poc-images.tar`,
  then `docker load -i panko-poc-images.tar` there before `panko dev up ...` with the same flags.
- An existing cluster pulls from a registry you control: tag and push both images, then set
  `images.server.repository`/`tag` and `images.moduleMirror.repository`/`tag` (and
  `images.pullSecrets` if needed). k3d and kind clusters can also import them
  (`k3d image import`, `kind load docker-image`).
- In a checkout, `make cluster-up build deploy` builds, imports and installs them through
  `panko install` with [`cluster/lab-values.yaml`](../cluster/lab-values.yaml).

The CLI itself comes from `go build ./cmd/panko` on the branch; release archives with the
embedded chart are future work.
