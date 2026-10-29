# Panko Helm chart

Installs Panko (experimental): CRDs (Panko's and Metacontroller v4.17.6's), explicit RBAC per role,
the `panko-server` roles (controller with the embedded Metacontroller, gateway, runner endpoint,
provider mirror), the module mirror, a setup Job and a daily CronJob for credentials and
certificates, and optionally a lab PostgreSQL and S3 service.

```sh
helm install panko oci://ghcr.io/hingst-it/charts/panko --version 0.0.2 \
  --namespace panko-system --create-namespace --values my-values.yaml
helm template panko oci://ghcr.io/hingst-it/charts/panko --version 0.0.2 \
  --namespace panko-system --include-crds --values my-values.yaml
```

- The chart version equals Panko's release version, and `appVersion` is the default tag of the
  images `ghcr.io/hingst-it/panko-server` and `ghcr.io/hingst-it/panko-module-mirror`.
- `installation` has the same shape as `panko.json` (teams, `s3`, `egress`, `egressTargets`,
  `runnerPools`, `runnerEndpoint`).
- The chart never renders a Secret. With `secrets.generate: true` the setup Job creates missing
  credentials once and keeps existing ones; with `false` it only verifies them.
- Workloads use fixed namespaces (`infra-runners`, `infra-gateway`, `infra-backends`, the team
  namespaces); the release namespace only holds the Helm record.
- `controller.replicas` must be 1; several replicas are not supported in this release.
- Helm never upgrades `crds/`; apply them before an upgrade (`panko install` does).
- Uninstalling keeps namespaces, CRDs, data, credentials and every resource with its finalizer.

See [installation](../../docs/installation.md), [GitOps](../../docs/gitops.md),
[operations](../../docs/operations.md) and the commented [values.yaml](values.yaml).
