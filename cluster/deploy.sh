#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
kubectl apply -f api/database.yaml
python3 cluster/runner-rbac.py | kubectl apply -f -
kubectl apply -f controller/deployment.yaml
kubectl -n infra-runners rollout restart deploy/database-hook
kubectl -n infra-runners rollout status deploy/database-hook --timeout=120s
kubectl apply -f controller/compositecontroller.yaml
if ! kubectl -n infra-runners get secret gateway-auth >/dev/null 2>&1; then
  python3 - <<'PY'
import json,os,secrets
path = '.local/team-tokens.json'
if not os.path.exists(path):
    with open(path, 'w') as stream:
        json.dump({team: secrets.token_urlsafe(32) for team in ['team-a', 'team-b']}, stream)
    os.chmod(path, 0o600)
PY
  kubectl -n infra-runners create secret generic gateway-auth --from-file=tokens.json=.local/team-tokens.json
fi
python3 gateway/rbac.py | kubectl apply -f -
kubectl apply -f gateway/deployment.yaml
kubectl -n infra-runners rollout restart deploy/platform-gateway
kubectl -n infra-runners rollout status deploy/platform-gateway --timeout=120s
