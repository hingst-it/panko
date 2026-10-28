# Install Panko with an AI assistant

Give your coding agent the following request, with access to a terminal on the machine that will
run the lab:

> Help me try Panko v0.0.2 for local evaluation. Read
> https://github.com/hingst-it/panko/blob/v0.0.2/AGENTS.md and
> https://github.com/hingst-it/panko/blob/v0.0.2/docs/quickstart.md first. Check my existing
> Docker and Kubernetes setup before changing anything. Install the released `panko` CLI, run
> `panko dev up` for a dedicated lab, create an example database, and verify Ready status. Tell me
> how to connect and how to clean up. Keep all credentials out of chat output.

## What the agent should establish

1. Identify the host OS and architecture, Docker availability, any installed k3d, and existing
   k3d clusters. Installing a CLI does not deploy the Kubernetes services.
2. Install the released CLI (Homebrew `hingst-it/tap/panko`, mise `github:hingst-it/panko@0.0.2`,
   asdf, or the release archive verified against `SHA256SUMS`) and check that
   `panko --version` prints `panko v0.0.2`. No checkout, Go, `make` or image build is needed; the
   lab pulls the published images.
3. Run `panko dev up`. If k3d is missing, the CLI asks before installing its pinned version
   privately; let the user decide. If a cluster named `panko-dev` exists and does not belong to the
   lab, the CLI refuses; agree on another `--name` instead of deleting anything. The user's
   `~/.kube/config` and default context stay unchanged.
4. Use the printed `PANKO_URL`, `PANKO_TOKENS_FILE` and `PANKO_KUBECONFIG`. The API is mapped to
   localhost by the lab cluster; no port-forward or other background process is needed.
5. Run `panko --team team-a doctor`, create `orders`, wait for `Ready`, and inspect its safe
   status. A command returning successfully is not enough if the resource is still provisioning.
   Report the observed result and any remaining problem. `panko dev status` is safe to show; it
   prints no secrets.
6. Show the user the local `connect` command. Its output contains a password: do not run it into a
   captured transcript, paste a URI into chat, dump Secrets, or print token files or the lab
   kubeconfig. Give port-forward instructions for the in-cluster PostgreSQL service when the user
   wants a local SQL client.
7. Explain resource deletion and whole-lab deletion separately. Preserve the lab unless the user
   requests cleanup; `panko dev down` asks the user to type the lab name. Never remove finalizers
   or state to make an installation appear successful.

For an existing cluster, `panko install --kubeconfig ... --context ...` replaces steps 3–4; confirm
the target cluster with the user first and prefer a dedicated cluster. To upgrade a v0.0.1
installation, follow [upgrading](upgrading.md) step by step and take its backup first.

For a failure report, include `panko --version`, the host platform, the exact failing command with
secrets omitted, the resource phase and error code, `panko dev status`, and relevant sanitized
diagnostics ([troubleshooting](operations.md#troubleshooting)). Do not expose the gateway publicly
or change cluster-wide security settings to work around a failed local install.
