#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
helm upgrade --install metacontroller oci://ghcr.io/metacontroller/metacontroller-helm \
  --version 4.17.6 --namespace metacontroller --create-namespace \
  --set fullnameOverride=metacontroller --wait --timeout 180s
kubectl create namespace infra-backends --dry-run=client -o yaml | kubectl apply -f -
.local/bin/admin --kubeconfig "$KUBECONFIG" ensure-postgres-auth
kubectl apply -f cluster/postgres.yaml
kubectl -n infra-backends rollout status statefulset/postgres --timeout=180s
bash cluster/check-backends.sh
