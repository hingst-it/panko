# Panko Helm chart (proof of concept)

Installs Panko: CRDs (Panko's and Metacontroller v4.17.6's), explicit RBAC per role, the
`panko-server` roles (controller with the embedded Metacontroller, gateway, runner endpoint,
provider mirror), the module mirror, a setup Job for generated credentials, and optionally a lab
PostgreSQL and S3 service.

```sh
helm template panko charts/panko --namespace panko-system --include-crds --values my-values.yaml
```

- `installation` has the same shape as `panko.json` (teams, `s3`, `egress`, `egressTargets`,
  `runnerPools`, `runnerEndpoint`).
- The chart never renders a Secret. With `secrets.generate: true` the setup Job creates missing
  credentials once and keeps existing ones; with `false` it only verifies them.
- Workloads use fixed namespaces (`infra-runners`, `infra-gateway`, `infra-backends`, the team
  namespaces); the release namespace only holds the Helm record.
- Helm never upgrades `crds/`; apply them before an upgrade (`panko install` does).

See [docs/installation.md](../../docs/installation.md) and [docs/gitops.md](../../docs/gitops.md),
and the commented [values.yaml](values.yaml).
