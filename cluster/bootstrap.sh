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
# Lab S3 gateway for team-defined state backends, if panko.json enables it.
if [ "$(.local/bin/admin config-s3)" = true ]; then
  .local/bin/admin --kubeconfig "$KUBECONFIG" ensure-s3-auth
  kubectl apply -f cluster/s3.yaml
  kubectl -n infra-backends rollout status statefulset/s3 --timeout=180s
fi
bash cluster/check-backends.sh
