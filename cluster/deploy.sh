#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
kubectl apply -f api/database.yaml
python3 cluster/runner-rbac.py | kubectl apply -f -
kubectl apply -f controller/deployment.yaml
kubectl -n infra-runners rollout restart deploy/database-hook
kubectl -n infra-runners rollout status deploy/database-hook --timeout=120s
kubectl apply -f controller/compositecontroller.yaml
