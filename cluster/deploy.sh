#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
# The same chart and command as without a checkout; only the lab values and local images differ.
args=(--kubeconfig "$KUBECONFIG" --config "${PANKO_CONFIG:-panko.json}" --values cluster/lab-values.yaml --tokens-file .local/team-tokens.json --yes)
# Content-derived tags from make build roll out exactly the changed images.
if [ -f .local/image-tags.env ]; then
  # shellcheck disable=SC1091
  . .local/image-tags.env
  args+=(--set "images.server.tag=$PANKO_SERVER_TAG" --set "images.moduleMirror.tag=$PANKO_MODULE_MIRROR_TAG")
fi
# A private SSH module keeps its deploy key only in infra-runners, as module.sshSecret.
if [ -n "${MODULE_SSH_KEY_FILE:-}" ]; then
  kubectl create namespace infra-runners --dry-run=client -o yaml | kubectl apply -f -
  .local/bin/admin --ssh-key-file "$MODULE_SSH_KEY_FILE" module-mirror-secret | kubectl apply -f -
  args+=(--set module.sshSecret=panko-module-git --take-ownership)
fi
.local/bin/panko install "${args[@]}"
bash cluster/check-backends.sh
