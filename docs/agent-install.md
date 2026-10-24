# Install Panko with an AI assistant

Give your coding agent the following request, with access to a terminal on the
machine that will run the lab:

> Help me install Panko v0.0.1 for local evaluation. Read
> https://github.com/hingst-it/panko/blob/v0.0.1/AGENTS.md and
> https://github.com/hingst-it/panko/blob/v0.0.1/docs/quickstart.md first.
> Use a fresh checkout and a dedicated cluster named panko-quickstart. Check
> my existing Docker and Kubernetes setup before changing anything. Install
> the CLI using my existing Homebrew, mise, or asdf setup if possible. Deploy
> the lab, create an example database, and verify Ready status. Tell me how
> to connect and how to clean up. Keep all credentials out of chat output.

## What the agent should establish

1. Identify the host OS and architecture, available package manager, Docker
   availability, and existing k3d clusters. Do not assume installing a CLI also
   deploys the Kubernetes services.
2. Use a new checkout at `v0.0.1`, and follow the quickstart's pinned tool versions.
   If `panko-quickstart` already exists, inspect it and agree on reuse or another
   name rather than deleting it.
3. Read the repository's `panko.json`. Use `.local/kubeconfig` explicitly, and keep
   the host's default Kubernetes contexts unchanged. The public example module
   needs no GitHub token or deploy key.
4. Deploy the services. Start a loopback-only gateway port-forward in a terminal
   or background process whose ownership and stop command are clear to the user.
5. Run `doctor`, create `orders`, wait for `Ready`, and inspect its safe status.
   A command returning successfully is not enough if the resource is still
   provisioning. Report the observed result and any remaining problem.
6. Show the user the local `connect` command. Its output contains a password:
   do not run it into a captured transcript, paste a URI into chat, dump Secrets,
   or print token files. Give port-forward instructions for the in-cluster
   PostgreSQL service when the user wants a local SQL client.
7. Explain resource deletion and whole-lab deletion separately. Preserve the lab
   unless the user requests cleanup; never remove finalizers or state to make an
   installation appear successful.

For a failure report, include the Panko version, host platform, exact failing
command with secrets omitted, resource phase/error code, and relevant sanitized
diagnostics. Use the existing status and `doctor` output before collecting logs.
Do not expose the gateway publicly or change cluster-wide security settings to
work around a failed local install.
