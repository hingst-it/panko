# Install Panko with an AI assistant

> This page describes the proof of concept on branch `poc/unified-server-dev-install`. For the
> released 0.0.1, use the [0.0.1 version](https://github.com/hingst-it/panko/blob/v0.0.1/docs/agent-install.md).

Give your coding agent the following request, with access to a terminal on the machine that
will run the lab:

> Help me try the Panko proof of concept for local evaluation. Read
> https://github.com/hingst-it/panko/blob/poc/unified-server-dev-install/AGENTS.md and
> https://github.com/hingst-it/panko/blob/poc/unified-server-dev-install/docs/quickstart.md
> first. Check my existing Docker and Kubernetes setup before changing anything. Build or use
> the `panko` CLI and the two images from that branch, run `panko dev up` for a dedicated lab,
> create an example database, and verify Ready status. Tell me how to connect and how to clean
> up. Keep all credentials out of chat output.

## What the agent should establish

1. Identify the host OS and architecture, Docker availability, any installed k3d, and existing
   k3d clusters. Installing a CLI does not deploy the Kubernetes services.
2. Get `panko` and the images `panko-server:dev` and `panko-module-mirror:dev`: build them from
   the branch (quickstart) or load a provided image archive with `docker load`. The released
   0.0.1 CLI from Homebrew, mise or asdf has no `dev` command.
3. Run `panko dev up --server-image panko-server:dev --module-mirror-image panko-module-mirror:dev`.
   If k3d is missing, the CLI asks before installing its pinned version privately; let the user
   decide. If a cluster named `panko-dev` exists and does not belong to the lab, the CLI refuses;
   agree on another `--name` instead of deleting anything. The user's `~/.kube/config` and
   default context stay unchanged.
4. Use the printed `PANKO_URL`, `PANKO_TOKENS_FILE` and `PANKO_KUBECONFIG`. The API is mapped to
   localhost by the lab cluster; no port-forward or other background process is needed.
5. Run `panko --team team-a doctor`, create `orders`, wait for `Ready`, and inspect its safe
   status. A command returning successfully is not enough if the resource is still
   provisioning. Report the observed result and any remaining problem. `panko dev status` is
   safe to show; it prints no secrets.
6. Show the user the local `connect` command. Its output contains a password: do not run it
   into a captured transcript, paste a URI into chat, dump Secrets, or print token files or the
   lab kubeconfig. Give port-forward instructions for the in-cluster PostgreSQL service when
   the user wants a local SQL client.
7. Explain resource deletion and whole-lab deletion separately. Preserve the lab unless the
   user requests cleanup; `panko dev down` asks the user to type the lab name. Never remove
   finalizers or state to make an installation appear successful.

For a failure report, include the commit of the branch, host platform, exact failing command
with secrets omitted, resource phase/error code, `panko dev status`, and relevant sanitized
diagnostics. Use the existing status and `doctor` output before collecting logs. Do not expose
the gateway publicly or change cluster-wide security settings to work around a failed local
install.
