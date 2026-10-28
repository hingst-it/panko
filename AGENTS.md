# Panko: guidance for coding and installation agents

Panko is an experimental Go service that exposes OpenTofu modules through a
Kubernetes-backed API. The first release supports PostgreSQL databases in a
personal/local lab. The project and its example module use Apache-2.0.

For installation assistance, read `docs/quickstart.md` and `docs/agent-install.md`.
The public module is fetched over HTTPS without credentials. Installing the CLI
through Homebrew, mise, or asdf is separate from deploying the cluster services.
On the proof-of-concept branch, `panko dev up` (dedicated k3d lab) and `panko
install` (existing cluster) deploy them from the chart embedded in the CLI; see
`docs/installation.md`. Use a dedicated cluster; preserve existing labs and data.
Keep bearer tokens, kubeconfigs, Secret contents, state, backups, and connection
URIs out of chat and committed files.

For code changes:

- Read the relevant Go package, deployment script, and tests before editing.
- Tool versions are in `mise.toml`; `make test-unit lint lint-go` runs local checks,
  including `helm lint` and reproducible rendering of `charts/panko`.
- `charts/panko` is the only installation path (CRDs in `crds/`); keep its RBAC equal
  to the `internal/admin` generators (`internal/chart` tests check this).
- `node --test .github/scripts/release-gate.test.cjs` checks release authorization.
- `make test` requires a disposable k3d lab from `make cluster-up build deploy` and
  exercises real resources.
- Keep `.local/` untracked. It holds credentials, build caches, and lab state.
- Preserve explicit retries, finalizers, team state separation, and sanitized status.
- Read `docs/releases.md` before releasing. A release tag must belong to `main`
  and its exact commit must already have successful `unit` and `e2e` CI jobs.

Do not add agent attribution or co-author trailers to commits.
