# Changelog

## Unreleased

### Proof of concept (branch `poc/unified-server-dev-install`, not for release)

- `panko-server`: one binary and one image for the roles `gateway`, `controller`,
  `endpoint`, `mirror`, `runner`, `agent` and `setup`. The controller role embeds
  Metacontroller v4.17.6 and calls the hook over loopback HTTP; there is no separate
  Metacontroller deployment. The per-role commands under `cmd/` are removed.
- Helm chart `charts/panko` with CRDs, explicit RBAC per role, the setup Job for
  generated credentials, optional lab PostgreSQL and S3, and GitOps examples for
  `helm template`, Argo CD and Flux.
- `panko install`, `panko dev up|status|down`, `panko chart export|template` and
  `panko admin` (the former `panko-admin` commands); the CLI embeds the chart.
- `make deploy` installs through `panko install`; `make bootstrap` is removed.
- Evaluation: `docs/poc-unified-server.de.md`.

### Added

- Optional per-pool `leaseSeconds` and `heartbeatSeconds` for remote runner pools;
  the defaults stay 60 and 15 seconds.

### Fixed

- Omit redundant Homebrew version metadata so generated formulae pass strict audit.

## 0.0.1 — 2026-10-24

First public experimental release for local evaluation and personal installations.

### Added

- PostgreSQL database provisioning, updates, and deletion through a team-scoped
  REST API and Go CLI, backed by Kubernetes, Metacontroller, and OpenTofu.
- CRD-derived OpenAPI, schema validation, local bearer tokens, explicit RBAC,
  connection Secrets, and a browser inspector with sanitized resource graphs.
- Structured execution status and error codes, explicit retries, immutable
  execution snapshots, and durable failure reporting after Job expiry.
- Kubernetes state by default, approved S3 state backends, state migration checks,
  per-team state isolation, and checkpoints.
- Backup/export and restore/import, with a rehearsed restoration into a separate lab.
- Provider and module mirrors, public HTTPS access to the example module, and
  optional declared runner egress restrictions.
- Read-only drift checks and controlled per-resource module revision upgrades.
- Configurable teams, installation diagnostics, token rotation, and a complete
  CLI database lifecycle.
- macOS, Linux, and Windows CLI/admin archives for AMD64 and ARM64, SHA-256
  checksums, Homebrew and asdf distribution, and direct mise installation.
- A separate tag-triggered release workflow that requires successful CI on the
  exact commit on `main`, and publishes Linux AMD64/ARM64 container images to GHCR.
- Public quickstart, AI-assisted installation guide, release notes, and Apache-2.0
  licensing for Panko and its example database module.

### Limitations

- Experimental APIs and configuration; compatibility is not promised yet.
- One demonstrated resource type and OpenTofu execution. Terraform CLI execution
  is not configured or tested.
- Supported lab deployment uses repository scripts and local development images;
  installing a CLI package alone does not deploy Kubernetes services.
- Static team tokens, shared PostgreSQL administrator access, no PostgreSQL TLS,
  high availability, OIDC, or production-grade tenant isolation.
- macOS and Windows executables have no paid publisher signature. Checksums are
  provided, and browser downloads can require operating-system approval.
