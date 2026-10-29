# Resource naming and versioning

Status: conventions for Panko-managed CRDs as of v0.0.2. Sections marked **deferred** are design
direction, not implemented behavior. The discussion that led here is in
[Brainstorm.md](../Brainstorm.md) (German).

## What v0.0.2 implements

Panko manages four CRDs, all in the API group `platform.example.com`, version `v1`:

| CRD | Kind | Purpose |
| --- | --- | --- |
| `databases.platform.example.com` | `Database` | the example service, provisioned by the [example module](https://github.com/hingst-it/panko-database) |
| `statebackends.platform.example.com` | `StateBackend` | a team's choice of OpenTofu state destination |
| `backendprofiles.platform.example.com` | `BackendProfile` | an operator-approved state destination |
| `runs.platform.example.com` | `Run` | one execution handed to a remote runner agent |

Conventions:

- **Category `panko`.** Every Panko-managed CRD lists `categories: [panko]`, so
  `kubectl get panko -n team-a` lists Panko's objects in a namespace. Categories are a discovery
  aid, not a security boundary: RBAC still grants access per resource.
- **No short names.** Panko-managed CRDs have no `shortNames`. Short names are global per cluster
  and collide silently across API groups; a future second provider team's `db` would shadow or be
  shadowed by Panko's. v0.0.1 defined `db` for `databases`; it is removed (see
  [compatibility](#compatibility-change-in-v002)).
- **Fully qualified names in automation.** Scripts, tests, generated instructions and the
  documentation use `databases.platform.example.com` (resource.group) rather than `databases`.
  Absence of short names does not prevent ambiguity: two API groups may define the same plural,
  and `kubectl get databases` then picks one by discovery order.
- **No `panko-<provider-team>` category yet.** A provider category names the team that owns a
  resource *type* (publishes its contract and module). The consuming teams are namespaces and
  never appear in names. None of the current CRDs has an explicitly defined provider team: the
  Database type belongs to the Panko example, and the other three are Panko's own machinery. The
  category is therefore not added; it must not be derived from the list of consuming teams.
- Metacontroller's CRDs (`compositecontrollers`, `controllerrevisions`, `decoratorcontrollers` in
  `metacontroller.k8s.io`) are vendored unchanged and keep their upstream short names.

### Compatibility change in v0.0.2

`kubectl get db` no longer works; use `kubectl get databases.platform.example.com` (or
`kubectl get panko`). `panko install` removes the old short name from an existing CRD, which
server-side apply alone would leave in place when `kubectl apply` of v0.0.1 still co-owns it.
The REST API, the `panko` CLI and stored objects are unaffected.

## Intended conventions (deferred)

These describe the direction for a separate feature workstream. v0.0.2 does not implement them,
and they need their own design and migration work.

- **Provider API groups.** A resource type published by a provider team would live in
  `<provider-team>.panko.dev`, for example `databases.data-platform.panko.dev`, with categories
  `panko` and `panko-<provider-team>`. The group names the owner of the type, not its consumers.
- **Moving existing resources to a new group is a migration, not a rename.** A Kubernetes object's
  API group is part of its identity: a `Database` in `platform.example.com` and one in
  `data-platform.panko.dev` are different resources with different CRDs, RBAC rules, finalizers
  and watches. Moving them needs a planned procedure that keeps infrastructure identity, state
  bindings, connection Secrets and finalizers, and must not trigger a destroy of the old object's
  infrastructure. No such procedure exists yet.
- **Group-aware routing.** The gateway and CLI route by resource plural today. Supporting the same
  plural in several groups needs group-aware discovery, routes and resource identities; version
  plus plural alone is not enough.
- **Kubernetes API versions are representations, not execution selectors.** `v1alpha1`, `v1beta1`
  and `v1` of one CRD are different representations of the *same* stored objects. Reading an
  object through another version, or converting it, must never change which infrastructure code
  runs. API versions therefore cannot select a module revision.
- **Contract enforcement and conversion.** Immutable contract checks, conversion webhooks and
  resource-version upgrades are deferred.

### Open decision: contract version and implementation revision

Panko pins the module revision per installation (`module.revision`) and records the revision
each resource last applied (`status.observedRevision`). How a published *contract version* (the
CRD's schema and semantics) should relate to the *implementation revision* (the module commit) is
not decided:

1. every contract version maps immutably to exactly one module revision, so any module fix needs a
   new contract version; or
2. contract version and implementation release are separate, so internal fixes reach existing
   resources without a customer-visible upgrade, within rules still to be defined.

Neither option is adopted, and v0.0.2 does not imply one. Likewise undecided: whether every module
revision needs a new API version, how a server-side upgrade command (`panko upgrade ... --to-version`
in the brainstorm) would coordinate conversion, state migration and partially applied changes, and
how CLI, UI, API and GitOps share those rules.

Also deferred to separate workstreams: secret brokering and ephemeral credential delivery to
remote agents, plan approvals, additional execution engines, and a general workflow framework.
