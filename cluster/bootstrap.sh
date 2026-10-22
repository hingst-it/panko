#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
helm upgrade --install metacontroller oci://ghcr.io/metacontroller/metacontroller-helm \
  --version 4.17.6 --namespace metacontroller --create-namespace \
  --set fullnameOverride=metacontroller --wait --timeout 180s
kubectl create namespace infra-backends --dry-run=client -o yaml | kubectl apply -f -
if ! kubectl -n infra-backends get secret postgres-admin >/dev/null 2>&1; then
  python3 - <<'PY' | kubectl create -f -
import json,secrets
print(json.dumps({'apiVersion':'v1','kind':'Secret','metadata':{'name':'postgres-admin','namespace':'infra-backends'},'stringData':{'password':secrets.token_urlsafe(32)}}))
PY
fi
kubectl apply -f cluster/postgres.yaml
kubectl -n infra-backends rollout status statefulset/postgres --timeout=180s
bash cluster/check-backends.sh
