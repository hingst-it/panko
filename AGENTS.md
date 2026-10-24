# Panko: guidance for coding and installation agents

Panko is an experimental Go service that exposes OpenTofu modules through a
Kubernetes-backed API. The first release supports PostgreSQL databases in a
personal/local lab. The project and its example module use Apache-2.0.

For installation assistance, read `docs/quickstart.md` and `docs/agent-install.md`.
The public module is fetched over HTTPS without credentials. Installing the CLI
through Homebrew, mise, or asdf is separate from deploying the cluster services.
Use a fresh checkout and a dedicated cluster; preserve existing labs and data.
Keep bearer tokens, kubeconfigs, Secret contents, state, backups, and connection
URIs out of chat and committed files.

For code changes:

- Read the relevant Go package, deployment script, and tests before editing.
- Tool versions are in `mise.toml`; `make test-unit lint lint-go` runs local checks.
- `node --test .github/scripts/release-gate.test.cjs` checks release authorization.
- `make test` requires a deployed, disposable k3d lab and exercises real resources.
- Keep `.local/` untracked. It holds credentials, build caches, and lab state.
- Preserve explicit retries, finalizers, team state separation, and sanitized status.
- Read `docs/releases.md` before releasing. A release tag must belong to `main`
  and its exact commit must already have successful `unit` and `e2e` CI jobs.

Do not add agent attribution or co-author trailers to commits.
