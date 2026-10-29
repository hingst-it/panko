# Operating Panko v0.0.2

Certificates, troubleshooting, uninstalling and cleaning up an installation made with the Helm
chart (`panko install`, `panko dev up`, Helm, Argo CD, Flux or rendered manifests). Upgrades and
backups are in [upgrading](upgrading.md); day-to-day details (teams, egress, drift, metrics, the
CLI) are in the German [operations guide](operations.de.md).

## Supported configuration

- One controller replica (`controller.replicas: 1`; the chart refuses other values). Leader
  election is wired but failover between replicas is not validated; Panko v0.0.2 is not highly
  available. The gateway can run several replicas; the module mirror and runner endpoint run one.
- Kubernetes 1.29 or newer, with a default StorageClass for the lab PostgreSQL, S3 and the
  provider cache. Validated on k3s v1.35 (k3d 5.9) on Linux AMD64 (CI) and macOS ARM64.
- Installing needs cluster-administrator rights (CRDs, ClusterRoles, namespaces); `panko install`
  checks them up front and lists what is missing.
- Static team tokens, a shared PostgreSQL administrator and no OIDC: suitable for a personal or
  lab installation, not for production tenant isolation.

## Certificates

The provider mirror and the runner endpoint serve TLS from stable CAs
(`infra-runners/provider-mirror-tls-ca`, `infra-runners/runner-endpoint-tls-ca`; ten years, keys
readable only by the setup role). The setup Job renews a server certificate 30 days before it
expires, or when `installation.runnerEndpoint.names` change, from the same CA. It runs on every
`panko install`, whenever its inputs change under GitOps, and daily as the CronJob
`infra-runners/panko-setup-scheduled` (`secrets.renewSchedule`). The mirror and endpoint reload a
renewed certificate within about a minute without a restart. Clients keep trusting the same CA:
runner Jobs read `panko-mirror-ca` in their namespace, agents keep the file from
`panko admin endpoint-ca`.

With `secrets.generate: false` you provide and renew the Secrets yourself (for example with
cert-manager); the setup Job only verifies them.

### Legacy CA migration

An installation from before stable CAs (v0.0.1, or the PoC before review) had certificates signed
by throwaway CAs. On the first v0.0.2 setup run, a still-valid certificate keeps being served,
and its `ca.crt` (and `panko-mirror-ca`, and `panko admin endpoint-ca`) becomes a bundle of the old
and the new stable CA. The server switches to a certificate from the stable CA at its next
renewal. In-cluster clients get the bundle automatically. Remote agents must receive the bundle
(`panko admin endpoint-ca > ca.crt`) before that renewal; v0.0.1 had no agents, so this only
concerns installations from the PoC. To finish the transition at a time you choose instead of at
renewal, delete the server certificate (never its CA) and rerun setup:

```sh
kubectl -n infra-runners delete secret provider-mirror-tls     # or runner-endpoint-tls
panko install ...                                              # or wait for the daily CronJob
```

The release acceptance test migrates a v0.0.1 installation, checks the bundle, completes the
transition this way and applies afterwards.

### Replacing a CA (manual trust rollover)

The setup Job warns a year before a CA expires (`warning: CA ... expires on ...` in its log).
Replacing a CA is a planned two-step rollover:

1. `kubectl -n infra-runners delete secret runner-endpoint-tls-ca` (or `provider-mirror-tls-ca`),
   then rerun setup. A new CA is created; the current certificate stays and `ca.crt` becomes an
   old+new bundle. Give agents the bundle from `panko admin endpoint-ca`; runner Jobs get theirs
   from `panko-mirror-ca`.
2. Once every client has the bundle, delete the server certificate (`runner-endpoint-tls` or
   `provider-mirror-tls`) and rerun setup. The new certificate is signed by the new CA, and
   `ca.crt` contains only the new CA again; refresh agents' copies once more.

The acceptance test rehearses this for both CAs. Deleting a CA and its certificate at the same
time breaks every client that has not received the new CA yet.

## Troubleshooting

`panko install` and `panko dev up` print what they check and stop with a reason. Rerunning the
same command is always safe: it resumes and never replaces credentials.

| Symptom | Cause and recovery |
| --- | --- |
| `the context "..." lacks permissions: ...` before any change | The kubeconfig's identity lacks the listed rights. Use a cluster-administrator context; nothing was changed. |
| `invalid installation values` or `the chart does not render with these values` | A values file, `--set` or `panko.json` is invalid (unknown team in a pool, bad module revision, `controller.replicas` other than 1). Fix the values; nothing was changed. |
| `problem infra-.../...: ImagePullBackOff` / `ErrImagePull` after the Helm wait | The cluster cannot pull the named image. Check network access to `ghcr.io`, the `images.*` values, or for a private registry the pull Secret named in `images.pullSecrets` in `infra-runners`, `infra-gateway`, `infra-backends` and every team namespace. Fix it and rerun; pods retry the pull on their own. |
| `problem ...: CreateContainerConfigError` | A required Secret is missing, usually with `secrets.generate: false`. The setup Job's log names it (`kubectl -n infra-runners logs job/<panko-setup-...>`). |
| `another panko install (...) is running against this cluster` | Another run holds the Lease `panko-system/panko-install` (the second run waits up to 45 seconds first). Wait for it; the Lease of a killed run expires after 30 seconds, and the next run resumes its work. |
| `release revision N is pending-install/pending-upgrade` | A Helm operation of another tool (Helm, Flux) did not finish. Make sure it is no longer running, then rerun with `--recover-pending`. An interrupted `panko install` is resumed without that flag. |
| `team ... still has N databases; delete them ...` | Removing a team with resources is refused. Delete them through the API first ([teams](operations.de.md#teams-und-konfiguration)). |
| `Panko is already deployed without Helm` or `a separate Metacontroller runs in namespace metacontroller` | A v0.0.1 installation: follow the [migration](upgrading.md#migrating-a-v001-installation). |
| A resource stays `Failed` with code `module_fetch` | The pinned `module.revision` is not reachable in `module.repository`. Fix the revision; then `panko retry NAME`. |
| A resource stays `Failed` with another code | See [failed applies and deletes](operations.de.md#fehlgeschlagene-applies-und-deletes); fix the cause and `panko retry NAME`. Never remove the finalizer. |

For a bug report include `panko --version`, the chart version (`helm -n panko-system list`), the
failing command without secrets, `panko dev status` or `kubectl get pods -A -l app.kubernetes.io/part-of=panko`,
and the resource's `status.lastExecution`. Never include tokens, kubeconfigs, Secret contents,
state or connection URIs.

## Uninstalling and cleaning up retained objects

`helm uninstall panko -n panko-system` (or deleting the Argo CD Application or Flux HelmRelease)
removes only the stateless workloads. Kept on purpose: namespaces, CRDs, the CompositeController
`database-controller`, the lab PostgreSQL and S3 StatefulSets, data and cache PVCs, Secrets
(credentials, state, connection details), team runner RBAC and BackendProfiles, and every
Database with its destroy finalizer. Reinstalling the same chart resumes management.

To remove Panko and its data completely, destroy the infrastructure **first**, while Panko still
runs:

```sh
# 1. Destroy every resource through Panko (finalizers run OpenTofu destroy and clean up state).
kubectl delete databases.platform.example.com --all -A --wait --timeout=15m
kubectl get panko -A                      # nothing left in any team namespace
# 2. Still while Panko runs: its embedded Metacontroller finalizes the CompositeController.
kubectl delete compositecontroller database-controller --timeout=5m
# 3. Remove the release, then the retained objects.
helm uninstall panko -n panko-system --wait
kubectl delete clusterrole,clusterrolebinding -l app.kubernetes.io/part-of=panko
kubectl delete namespace infra-runners infra-gateway infra-backends panko-system \
  team-a panko-state-team-a team-b panko-state-team-b   # your teams
kubectl delete crd databases.platform.example.com runs.platform.example.com \
  statebackends.platform.example.com backendprofiles.platform.example.com
# Metacontroller's CRDs, if no other installation uses them:
kubectl delete crd compositecontrollers.metacontroller.k8s.io \
  controllerrevisions.metacontroller.k8s.io decoratorcontrollers.metacontroller.k8s.io
```

The order matters. A CompositeController deleted after the release is gone stays `Terminating`
with the finalizer `metacontroller.io/parent-finalizer-cleanup`, because no controller runs to
finalize it; reinstall the release (it resumes with the retained objects) and delete it then.
Deleting the CompositeController or the CRDs while Databases exist removes their finalizers or the
resources themselves without destroying the infrastructure, which then has no state anywhere.
Deleted namespaces take their state and connection Secrets with them. The acceptance test runs
this procedure on an empty installation and checks that no Panko object remains. A lab from
`panko dev up` is removed completely with `panko dev down`.

Removing a single team: destroy its resources, remove it from `installation.teams` and upgrade
(`panko install` refuses while resources remain). The team's runner identity and execution RBAC
are retained so that remaining resources could still be destroyed. Afterwards delete them by
hand: the namespaces `<team>` and `panko-state-<team>` (with the ServiceAccount, Roles,
RoleBindings and BackendProfile inside), the ClusterRoleBinding `tofu-schema-reader-<team>`, and
the RoleBinding `postgres-admin-reader-<team>` in `infra-backends`.
