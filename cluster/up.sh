#!/usr/bin/env bash
set -euo pipefail
mkdir -p .local
cluster_name="${CLUSTER_NAME:-infra-lab}"
kubeconfig="${KUBECONFIG:-$PWD/.local/kubeconfig}"
if ! mise exec -- k3d cluster list --no-headers | awk -v name="$cluster_name" '$1 == name { found=1 } END { exit !found }'; then
  mise exec -- k3d cluster create "$cluster_name" --agents 1 --kubeconfig-update-default=false --kubeconfig-switch-context=false
fi
mise exec -- k3d kubeconfig get "$cluster_name" > "$kubeconfig"
chmod 600 "$kubeconfig"
kubectl --kubeconfig "$kubeconfig" wait --for=condition=Ready nodes --all --timeout=180s
kubectl --kubeconfig "$kubeconfig" get nodes
