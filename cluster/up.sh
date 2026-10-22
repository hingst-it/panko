#!/usr/bin/env bash
set -euo pipefail
mkdir -p .local
if ! mise exec -- k3d cluster list -o json | python3 -c 'import json,sys; sys.exit(not any(c["name"] == "infra-lab" for c in json.load(sys.stdin)))'; then
  mise exec -- k3d cluster create infra-lab --agents 1 --kubeconfig-update-default=false --kubeconfig-switch-context=false
fi
mise exec -- k3d kubeconfig get infra-lab > .local/kubeconfig
chmod 600 .local/kubeconfig
kubectl --kubeconfig .local/kubeconfig wait --for=condition=Ready nodes --all --timeout=180s
kubectl --kubeconfig .local/kubeconfig get nodes
