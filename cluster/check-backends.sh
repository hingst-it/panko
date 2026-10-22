#!/usr/bin/env bash
set -euo pipefail
# Short-lived administrator-run debug pod. Credentials never go to argv or logs.
kubectl -n infra-backends delete pod postgres-debug --ignore-not-found --wait=true >/dev/null
cat <<'YAML' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: postgres-debug
  namespace: infra-backends
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  containers:
    - name: psql
      image: postgres:17.11-alpine
      env:
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef: {name: postgres-admin, key: password}
      command: [psql, -h, postgres, -U, postgres, -d, postgres, -v, ON_ERROR_STOP=1, -c, 'SELECT version();']
YAML
kubectl -n infra-backends wait --for=jsonpath='{.status.phase}'=Succeeded pod/postgres-debug --timeout=120s
kubectl -n infra-backends logs postgres-debug
kubectl -n infra-backends delete pod postgres-debug --wait=false >/dev/null
kubectl -n metacontroller get pods
