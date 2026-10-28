# GitOps: `helm template`, Argo CD, and Flux

> Panko v0.0.2, experimental. The examples install the published chart
> `oci://ghcr.io/hingst-it/charts/panko` version `0.0.2`, pinned; the release acceptance test runs
> them against Argo CD and Flux ([validation scope](#validation-scope)).

The chart works without `panko install`. It renders deterministically and never
contains a credential, so repeated renders cause no drift and no credential rotation. Missing
credentials are created inside the cluster by the chart's setup Job, which keeps every existing
value (see [Secrets](#secrets)).

Examples:

- [`examples/gitops/values.yaml`](../examples/gitops/values.yaml): commented values for an existing cluster
- [`examples/gitops/argocd/application.yaml`](../examples/gitops/argocd/application.yaml): Argo CD `Application`
- [`examples/gitops/flux/panko.yaml`](../examples/gitops/flux/panko.yaml): Flux `OCIRepository` and `HelmRelease`
- [`examples/gitops/rendered/README.md`](../examples/gitops/rendered/README.md): pre-rendered manifests

## Render and check

```sh
helm pull oci://ghcr.io/hingst-it/charts/panko --version 0.0.2 --untar   # writes ./panko
helm lint --strict ./panko --values values.yaml
helm template panko ./panko \
  --namespace panko-system \
  --include-crds \
  --values values.yaml \
  > rendered/panko.yaml
```

From a checkout, `charts/panko` and `examples/gitops/values.yaml` render the same chart.

Without a checkout or a `helm` binary, the CLI renders its embedded copy of the same chart
identically: `panko chart template --include-crds --values values.yaml`, or
`panko chart export --dir .` to write `panko/` for a GitOps repository.

Checks worth automating on the rendered output (the repository does them in `make lint-chart`
and `internal/chart/chart_test.go`):

- two renders of the same chart and values are byte-identical;
- there is no `kind: Secret`;
- every Deployment and Job names its own ServiceAccount (or has no token), and the gateway's
  Roles contain no `secrets`;
- `kubectl apply --dry-run=server -f rendered/panko.yaml` against the target cluster, after the
  CRDs exist.

## What the cluster needs

- **Namespaces**: the release record lives in `panko-system` (`CreateNamespace=true` /
  `install.createNamespace`). The chart creates `infra-runners`, `infra-gateway`,
  `infra-backends`, and for every team `<team>` and `panko-state-<team>`; all are kept on
  uninstall and pruning.
- **Images** reachable by the cluster: `ghcr.io/hingst-it/panko-server` and
  `panko-module-mirror` with the chart's version as tag, anonymously pullable. For a private
  mirror, see [images from a private registry](installation.md#images-from-a-private-registry):
  pull secrets (`images.pullSecrets`) must exist in `infra-runners`, `infra-gateway`,
  `infra-backends` and every team namespace, because runner Jobs pull `panko-server` through the
  teams' `tofu-runner` ServiceAccounts.
- **The chart** reachable by the controller: `oci://ghcr.io/hingst-it/charts/panko` (public, no
  credentials). Pin an exact version; there is no floating tag.
- **No other Metacontroller** reconciling Panko's CompositeController. The embedded controller
  only manages CompositeControllers labelled `platform.example.com/controller=panko`; a
  Metacontroller without a target label selector would manage that one too.
- **Existing Secrets** only with `secrets.generate=false` (below), and
  `infra-backends/postgres-admin` for an external PostgreSQL (`lab.postgres.enabled=false`).

## Order and readiness

1. CRDs from `crds/` (Panko's and Metacontroller's) must be established before the
   CompositeController and the BackendProfiles in `templates/` are applied. Helm handles this
   (tested), and so does applying `crds/` first and waiting for `Established` with plain
   manifests (tested), Flux with `crds: CreateReplace` (tested) and Argo CD with
   `SkipDryRunOnMissingResource=true` and retries (tested).
2. Everything else is applied in one step. The setup Job `infra-runners/panko-setup-<digest>`
   creates missing credentials within seconds. Pods that mount them (gateway, endpoint,
   provider mirror, lab PostgreSQL and S3) stay in `ContainerCreating` until then, and the
   controller waits for `panko-installation`. No sync waves are needed; waves that wait for the
   workloads before the Job would deadlock.
3. Healthy means: Deployments and StatefulSets ready, the setup Job and `s3-onboard-<digest>`
   complete. The controller's readiness (`/readyz`) includes the embedded Metacontroller's cache
   sync and the loopback hook listener.

## Upgrades, CRDs, pruning and persistent data

- **CRDs**: Helm never upgrades `crds/`. With plain manifests apply `rendered/crds.yaml` on every
  chart update (`kubectl apply --server-side`, tested); `panko install` does the same. Flux uses
  `install.crds`/`upgrade.crds: CreateReplace` (install tested); Argo CD applies `crds/` like
  other resources, with `ServerSideApply=true` for the large Metacontroller CRDs (tested).
  CRD changes are additive; existing resources are not converted.
- **Jobs**: the setup and S3 onboarding Jobs are named by a digest of their inputs (installation
  config, image, arguments; team list, S3 image and template). Job templates are immutable, so
  every template input is part of the name. Unchanged values keep the completed Job, so there is no
  perpetual drift; changed values create a new Job, and pruning removes the old one.
- **Pruning**: namespaces, the provider cache PVC, the lab StatefulSets and the CompositeController carry
  `helm.sh/resource-policy: keep`, `argocd.argoproj.io/sync-options: Prune=false,Delete=false`
  and `kustomize.toolkit.fluxcd.io/prune: disabled`. Team resources, state and credentials are
  never part of the render and are never pruned. All CRDs in `crds/` carry the same Argo CD and
  Flux annotations, because deleting a CRD deletes every resource of its kind; Helm never deletes
  `crds/`. A team's runner identity and execution RBAC are retained as well when the team is
  removed (see [operations](operations.de.md#teams-und-konfiguration)).
- **Persistent data**: PostgreSQL, S3 and provider cache volumes are retained when their
  StatefulSet or the release is deleted (`persistentVolumeClaimRetentionPolicy: Retain`).
- **Manual changes**: `panko admin pause` scales the controller and gateway to zero; Argo CD
  self-heal reverts that (tested: back to one replica within seconds). The Flux example does not
  enable `driftDetection`, so Flux leaves such manual changes until the next upgrade. Suspend the application for maintenance.
  The same applies to `kubectl set env` on the controller.
- **Module upgrades**: change `module.revision` in Git. With `rollout: manual`, resources stay
  on their revision until `panko admin upgrade-module`.
- **Runner pools**: removing a pool that resources are still bound to does not block them. Every
  starting controller replica merges the configured pool definitions into
  `infra-runners/panko-runner-pools-retained` and executes with the configured definitions plus all
  recorded ones; the record only grows, so neither a rollout nor a replica that is not yet leader
  can drop a definition an older controller still uses. Each definition carries its writer: the
  controller Deployment (UID and creation time) and the rollout revision of its ReplicaSet. Within
  one Deployment only the same or a newer revision may replace a definition, so a restarting older
  replica cannot bring back an outdated module (a rollback is a new revision); a Deployment created
  later replaces definitions of an earlier one, so a reinstall that restarts the revisions works.
  Owners are verified by UID, so an old replica cannot claim a recreated Deployment's identity. Bound pools that are no longer configured
  are logged as `runner_pool_retained`; their resources keep executing, including destroy. New
  resources still bind only to configured pools. A bound pool without any recorded definition (for
  example removed in the same change that introduced the record) stays blocked rather than
  guessing its module. Delete a definition from the record by hand once nothing is bound to it.
  `panko install` additionally refuses such a change up front; `panko admin --config
  new-panko.json --kubeconfig ... check-pools` does the same check for GitOps changes.

## Secrets

With `secrets.generate: true` (default) the setup Job creates each missing credential once:
random tokens and passwords, the provider mirror and runner endpoint certificates from their own
CAs, the empty agent registry, and S3 keys. It never replaces an existing value, so a re-render,
resync or new chart version keeps every credential. Token rotation stays explicit (`panko admin
rotate-token`). Server certificates are renewed 30 days before expiry from stable CAs by the
daily CronJob `panko-setup-scheduled`, also when no value changes, and the mirror and endpoint
reload them without a restart. Replacing a CA itself is a planned manual step.

With `secrets.generate: false`, provide them yourself, for example with SOPS, External Secrets
or cert-manager. The setup Job then only verifies them and fails listing every missing name
(never a value). Names and keys are fixed:

| Secret | Keys |
|---|---|
| `infra-backends/postgres-admin` | `password` |
| `infra-gateway/gateway-auth` | `tokens.json`: `{"<team>": "<token>", ...}` for every team |
| `infra-runners/provider-mirror-tls-ca`, `infra-runners/runner-endpoint-tls-ca` (only with `generate: true`) | `ca.crt`, `ca.key`: the stable CAs; readable only by the setup role |
| `infra-runners/provider-mirror-tls` | `tls.crt`, `tls.key`, `ca.crt` (names `provider-mirror.infra-runners.svc[.cluster.local]`) |
| `infra-runners/runner-endpoint-tls` | `tls.crt`, `tls.key`, `ca.crt` (names `runner-endpoint.infra-runners.svc[.cluster.local]` and `runnerEndpoint.names`) |
| `infra-runners/panko-runner-agents` | none initially; `panko admin add-agent` adds hashed entries |
| `infra-backends/s3-admin` (with `s3`) | `access_key_id`, `secret_access_key` |
| `infra-backends/s3-teams` (with `s3`) | `<team>`: the team's S3 secret key |
| `<team>/panko-state-s3` (with `s3`) | `access_key_id` (`panko-<team>`), `secret_access_key` |
| `infra-runners/<module.sshSecret>` (SSH modules) | `ssh`, `known_hosts` |

The setup Job also maintains objects derived from the live cluster, which a render cannot know:
`panko-mirror-ca` ConfigMaps in team namespaces (from `provider-mirror-tls`), the egress
NetworkPolicies `panko-runner-egress` (resolved Service selectors and API server addresses),
and `infra-runners/panko-installation`. They are not part of the render, so GitOps tools do not
report them as drift.

Team tokens for people: `panko admin --config cluster --kubeconfig ... --tokens-file
./team-tokens.json ensure-gateway-auth` copies them from the cluster into a private local file
(mode 0600). Never commit it.

## Argo CD

[`application.yaml`](../examples/gitops/argocd/application.yaml) uses two sources: the OCI chart
(`repoURL: ghcr.io/hingst-it/charts`, `chart: panko`, `targetRevision: 0.0.2`) and your values
from a GitOps repository (`$values`). Relevant options:
`CreateNamespace=true`, `ServerSideApply=true`, `SkipDryRunOnMissingResource=true`, retries, and
automated sync with pruning. Argo CD renders with `helm template` and applies `crds/`.
Helm hooks are not used. Argo CD tracks only rendered objects, so the setup Job's Secrets,
ConfigMaps and NetworkPolicies do not show up as out of sync. The release acceptance test runs it with Argo CD v3.5.6 from the OCI chart (pending for
v0.0.2: Synced and Healthy, a Database Ready, retention after deleting the Application). Tested
during the PoC with the chart from Git: no out-of-sync resources after a hard refresh, the setup
Job not re-run, and self-heal. The `ignoreDifferences` entry for `volumeClaimTemplates` status is
needed: without it the lab PostgreSQL StatefulSet stays `OutOfSync`.

## Flux

[`panko.yaml`](../examples/gitops/flux/panko.yaml) defines an `OCIRepository` for the published
chart (`ref.tag: 0.0.2`) and a `HelmRelease` (`chartRef`) with `crds: CreateReplace` for install and upgrade, retries
without rollback (Panko's state lives in its own objects), and values from a ConfigMap
(`valuesFrom`). The release record is stored in `panko-system`. The release acceptance test runs it with Flux v2.10.0 from the OCI chart (pending for v0.0.2:
Ready, CRDs created, a Database Ready, retention after deleting the HelmRelease). Tested during
the PoC with the chart from Git: a changed value rolled out through `valuesFrom` with
the tokens unchanged. Flux's helm-controller uses its own Helm SDK version and wait logic, which may
differ from `panko install`.

## Pre-rendered manifests

See [`examples/gitops/rendered`](../examples/gitops/rendered/README.md): render `helm show crds`
and `helm template` once per version, commit both, and apply the CRDs first. Without a Helm
release record, `panko install` later treats the cluster as an existing installation and needs
`--take-ownership`.

## Moving an installation

Moving a live installation from `panko install` or Helm to Argo CD or Flux is not validated in
v0.0.2. Use the same release name (`panko`), release namespace (`panko-system`) and values, suspend
the previous tool first so that two tools never reconcile the release at the same time, and
rehearse on a disposable copy. Flux's helm-controller and Argo CD take over fields under their own
field managers. Plain manifests have no release record; a later `panko install` needs
`--take-ownership` once. A v0.0.1 installation first follows the
[migration](upgrading.md#migrating-a-v001-installation).

## Validation scope

| Path | Validated for v0.0.2 |
|---|---|
| `helm lint --strict`, `helm template` with defaults and `examples/gitops/values.yaml` | yes (`make lint-chart`, CI unit job) |
| Reproducible render, no Secrets, RBAC equal to the `panko admin` generators, image references equal to the published images | yes (`internal/chart` tests, `tools/release.py check`) |
| Install and upgrade through `panko install` and `panko dev up` on k3d | yes, in the e2e suites; release acceptance test pending |
| `helm install` / `helm uninstall` of the packaged chart, private registry with pull secrets | pending: release acceptance test |
| Argo CD `Application` (v3.5.6) from the published OCI chart | pending: release acceptance test (Synced/Healthy, Database Ready, retention after deleting the Application) |
| Flux `OCIRepository`/`HelmRelease` (v2.10.0) from the published OCI chart | pending: release acceptance test (Ready, Database Ready, retention after deleting the HelmRelease) |
| Argo CD self-heal, hard refresh without drift; Flux `valuesFrom` upgrade | during the PoC only (chart from Git over SSH) |
| Pre-rendered manifests applied with `kubectl apply --server-side`, CRDs first; switching to Helm with `--take-ownership` | during the PoC |
| Moving a live installation between `panko install`, Argo CD and Flux | no |
