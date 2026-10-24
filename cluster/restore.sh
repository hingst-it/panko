#!/usr/bin/env bash
# Restore a backup into a freshly deployed installation (make cluster-up bootstrap build deploy).
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
dir="${BACKUP_DIR:?BACKUP_DIR must name an existing backup directory}"
test -f "$dir/cluster.json" -a -f "$dir/postgres.sql"
.local/bin/admin --kubeconfig "$KUBECONFIG" pause
trap '.local/bin/admin --kubeconfig "$KUBECONFIG" resume' EXIT
.local/bin/admin --kubeconfig "$KUBECONFIG" import --dir "$dir"
# Roles and databases come back with their original passwords; postgres-admin was restored to match.
kubectl -n infra-backends exec -i postgres-0 -- psql -U postgres -d postgres -q -v ON_ERROR_STOP=0 < "$dir/postgres.sql" > /dev/null
if [ -f "$dir/s3.tar" ]; then
  kubectl -n infra-backends exec -i s3-0 -- tar -C /data -xf - < "$dir/s3.tar"
  # The gateway reads its root key and users at start.
  kubectl -n infra-backends rollout restart statefulset/s3
  kubectl -n infra-backends rollout status statefulset/s3 --timeout=180s
fi
# The restored gateway-auth Secret is authoritative; align the local token file with it.
.local/bin/admin --kubeconfig "$KUBECONFIG" ensure-gateway-auth
trap - EXIT
.local/bin/admin --kubeconfig "$KUBECONFIG" resume
kubectl -n infra-runners rollout status deploy/database-hook --timeout=120s
kubectl -n infra-gateway rollout restart deploy/platform-gateway
kubectl -n infra-gateway rollout status deploy/platform-gateway --timeout=120s
echo "Restore complete; every Database applies once against its restored state"
