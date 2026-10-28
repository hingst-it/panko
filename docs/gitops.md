# GitOps: `helm template`, Argo CD, and Flux

> Proof of concept on branch `poc/unified-server-dev-install`. The chart and the examples below
> are validated by rendering (`helm lint`, `helm template`, the chart's Go tests), by
> `panko install` / `panko dev up` against k3d, and by applying pre-rendered manifests with `kubectl`. They were **not** applied by a running Argo CD or
> Flux; see [Validation scope](#validation-scope).

The chart `charts/panko` works without `panko install`. It renders deterministically and never
contains a credential, so repeated renders cause no drift and no credential rotation. Missing
credentials are created inside the cluster by the chart's setup Job, which keeps every existing
value (see [Secrets](#secrets)).

Examples:

- [`examples/gitops/values.yaml`](../examples/gitops/values.yaml): commented values for an existing cluster
- [`examples/gitops/argocd/application.yaml`](../examples/gitops/argocd/application.yaml): Argo CD `Application`
- [`examples/gitops/flux/panko.yaml`](../examples/gitops/flux/panko.yaml): Flux `GitRepository` and `HelmRelease`
- [`examples/gitops/rendered/README.md`](../examples/gitops/rendered/README.md): pre-rendered manifests

## Render and check

```sh
helm lint --strict charts/panko --values examples/gitops/values.yaml
helm template panko charts/panko \
  --namespace panko-system \
  --include-crds \
  --values examples/gitops/values.yaml \
  > rendered/panko.yaml
```

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
- **Images** reachable by the cluster: the proof of concept publishes none. Push
  `panko-server` and `panko-module-mirror` built from the branch to your registry and pin their
  tags in the values ([Images](installation.md#images-for-the-proof-of-concept)). Pull secrets
  (`images.pullSecrets`) must exist in `infra-runners`, `infra-gateway`, and `infra-backends`.
- **The chart** reachable by the controller: Argo CD and Flux read `charts/panko` from the Git
  repository (pin a commit); a packaged chart in an OCI registry is future work.
- **No other Metacontroller** reconciling Panko's CompositeController. The embedded controller
  only manages CompositeControllers labelled `platform.example.com/controller=panko`; a
  Metacontroller without a target label selector would manage that one too.
- **Existing Secrets** only with `secrets.generate=false` (below), and
  `infra-backends/postgres-admin` for an external PostgreSQL (`lab.postgres.enabled=false`).

## Order and readiness

1. CRDs from `crds/` (Panko's and Metacontroller's) must be established before the
   CompositeController and the BackendProfiles in `templates/` are applied. Helm handles this
   (tested), and so does applying `crds/` first and waiting for `Established` with plain
   manifests (tested). Flux (`crds: CreateReplace`) and Argo CD (`SkipDryRunOnMissingResource=true`
   with retries) are expected to handle it according to their documentation; not tested here.
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
  chart update (`kubectl apply --server-side`, tested); `panko install` does the same. For Flux,
  `install.crds`/`upgrade.crds: CreateReplace`, and for Argo CD, which applies `crds/` like other
  resources, `ServerSideApply=true` for the large Metacontroller CRDs are the documented options
  of those tools (not tested here).
  CRD changes are additive; existing resources are not converted.
- **Jobs**: the setup and S3 onboarding Jobs are named by a digest of their inputs (installation
  config, image, arguments; team list). Unchanged values keep the completed Job, so there is no
  perpetual drift; changed values create a new Job, and pruning removes the old one.
- **Pruning**: namespaces, the provider cache PVC and the lab StatefulSets carry
  `helm.sh/resource-policy: keep`, `argocd.argoproj.io/sync-options: Prune=false,Delete=false`
  and `kustomize.toolkit.fluxcd.io/prune: disabled`. Team resources, state and credentials are
  never part of the render and are never pruned. CRDs are not deleted by Helm; protect them in
  Argo CD with the same annotation if your policy prunes CRDs.
- **Persistent data**: PostgreSQL, S3 and provider cache volumes are retained when their
  StatefulSet or the release is deleted (`persistentVolumeClaimRetentionPolicy: Retain`).
- **Manual changes**: `panko admin pause` scales the controller and gateway to zero; Argo CD
  self-heal or a Flux drift correction can be expected to revert that (not tested here). Suspend the application for maintenance.
  The same applies to `kubectl set env` on the controller.
- **Module upgrades**: change `module.revision` in Git. With `rollout: manual`, resources stay
  on their revision until `panko admin upgrade-module`.
- **Runner pools**: before removing a pool from the values, check the new configuration with
  `panko admin --config new-panko.json --kubeconfig ... check-pools` (the `installation` values
  as JSON); the chart cannot refuse it, while `panko install` does.

## Secrets

With `secrets.generate: true` (default) the setup Job creates each missing credential once:
random tokens and passwords, the provider mirror and runner endpoint certificates from their own
CAs, the empty agent registry, and S3 keys. It never replaces an existing value, so a re-render,
resync or new chart version keeps every credential. Rotation stays explicit (`panko admin
rotate-token`, certificate renewal 30 days before expiry, which also needs a restart of the
mirror and endpoint pods).

With `secrets.generate: false`, provide them yourself, for example with SOPS, External Secrets
or cert-manager. The setup Job then only verifies them and fails listing every missing name
(never a value). Names and keys are fixed:

| Secret | Keys |
|---|---|
| `infra-backends/postgres-admin` | `password` |
| `infra-gateway/gateway-auth` | `tokens.json`: `{"<team>": "<token>", ...}` for every team |
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

[`application.yaml`](../examples/gitops/argocd/application.yaml) uses two sources: the chart from
the Panko repository and your values from a GitOps repository (`$values`). Relevant options:
`CreateNamespace=true`, `ServerSideApply=true`, `SkipDryRunOnMissingResource=true`, retries, and
automated sync with pruning. Argo CD renders with `helm template` and applies `crds/`.
Helm hooks are not used. Argo CD tracks only rendered objects, so the setup Job's Secrets,
ConfigMaps and NetworkPolicies should not show up as out of sync. These options follow the Argo CD
documentation; a sync against a running Argo CD was not tested.

## Flux

[`panko.yaml`](../examples/gitops/flux/panko.yaml) defines a `GitRepository` limited to
`charts/panko` and a `HelmRelease` with `crds: CreateReplace` for install and upgrade, retries
without rollback (Panko's state lives in its own objects), and values from a ConfigMap
(`valuesFrom`). The release record is stored in `panko-system`. These options follow the Flux
documentation; a reconciliation by a running Flux was not tested. Flux's helm-controller uses its
own Helm SDK version and wait logic, which may differ from `panko install`.

## Pre-rendered manifests

See [`examples/gitops/rendered`](../examples/gitops/rendered/README.md): render `helm show crds`
and `helm template` once per version, commit both, and apply the CRDs first. Without a Helm
release record, `panko install` later treats the cluster as an existing installation and needs
`--take-ownership`.

## Validation scope

| Path | Validated in this proof of concept |
|---|---|
| `helm lint --strict`, `helm template` with defaults and `examples/gitops/values.yaml` | yes (`make lint-chart`, CI unit job) |
| Reproducible render, no Secrets, RBAC equal to the `panko admin` generators | yes (`internal/chart` tests) |
| Install and upgrade through the Helm SDK (`panko install`, `panko dev up`) on k3d | yes, including the e2e suites |
| Argo CD `Application` | YAML parsed and reviewed; not applied to an Argo CD instance |
| Flux `GitRepository`/`HelmRelease` | YAML parsed and reviewed; not applied to a Flux instance |
| Pre-rendered manifests: `helm show crds` and `helm template` applied with `kubectl apply --server-side`, CRDs first | yes: no Secrets rendered, setup Job and all workloads ready, controller `/readyz`, re-render byte-identical |
| Switching that installation to a Helm release (`panko install --take-ownership`) | yes, tokens unchanged |
| `helm uninstall` keeps namespaces, CRDs, StatefulSets, PVCs and Secrets | yes |
