# Self-hosted CI on hosting

Panko's trusted CI and release jobs can use the Flux-managed ARC scale set
`panko-hosting`. The deployment lives in `designfactory/defac-flux`,
`apps/github-actions/`, selected only by the hosting cluster.

The repository variable `PANKO_CI_RUNNER=panko-hosting` selects those workers.
Removing the variable returns jobs to `ubuntu-latest`, which is also the fallback
while provisioning or recovering the cluster. Pull requests from forks always
use GitHub-hosted runners. The two worker-image bootstrap/recovery workflows
also stay GitHub-hosted so the worker infrastructure can be rebuilt independently.

Each job receives a fresh Kata VM-backed pod, its own Docker daemon and ephemeral
60 GiB PVC, and no hosting-cluster service-account token. The controller and
GitHub App credentials are separate from job execution. Docker's OverlayFS data
uses a 45 GiB guest-local ext4 loop filesystem backed by that ephemeral PVC:
OverlayFS cannot use the CSI filesystem volume's Kata virtiofs path directly.
The loop device is inside the guest, not on a hosting node.

There is one idle worker and a maximum of three workers. GitHub queues additional
matrix jobs. Workspaces, Docker images and tool caches are not shared across
jobs; ARC deletes the worker and its PVC after use.

To verify a changed runner image or template, dispatch `Hosting runner smoke`
and require success before enabling routing. It checks tool availability,
absence of a hosting API token, a nested Docker container, and a disposable k3d
cluster with Ready nodes. The normal Checks workflow then tests the complete
Panko lifecycle on the self-hosted workers.

Controller/listener and runner metrics/events are collected by the hosting
OneUptime Kubernetes agent. Runner pod logs are explicitly excluded, and resource
spec collection remains disabled. A dedicated OneUptime monitor is deferred until
project API credentials are available; no existing admin password was changed.
