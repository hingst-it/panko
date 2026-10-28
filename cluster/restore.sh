#!/usr/bin/env bash
# Restore a backup into a freshly deployed installation (make cluster-up build deploy).
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
dir="${BACKUP_DIR:?BACKUP_DIR must name an existing backup directory}"
test -f "$dir/cluster.json" -a -f "$dir/postgres.sql"
# A failed restore stays paused: resuming would run Jobs against partially restored state.
# Rerunning make restore with the same BACKUP_DIR continues; import skips Databases it already restored.
trap 'echo "Restore failed; the installation stays paused. Fix the cause and rerun make restore BACKUP_DIR=$dir" >&2' ERR
umask 077
.local/bin/admin --kubeconfig "$KUBECONFIG" pause
.local/bin/admin --kubeconfig "$KUBECONFIG" import --dir "$dir"
# Roles and databases come back with their original passwords; postgres-admin was restored to match.
# The connecting superuser cannot be dropped or created again; every other error stops the restore.
# psql output can quote data, so it stays in the private backup directory.
grep -vxE 'DROP ROLE IF EXISTS postgres;|CREATE ROLE postgres;' "$dir/postgres.sql" |
  kubectl -n infra-backends exec -i postgres-0 -- psql -U postgres -d postgres -q -v ON_ERROR_STOP=1 > "$dir/restore-psql.log" 2>&1
if [ -f "$dir/s3.tar" ]; then
  kubectl -n infra-backends exec -i s3-0 -- tar -C /data -xf - < "$dir/s3.tar"
  # The gateway reads its root key and users at start.
  kubectl -n infra-backends rollout restart statefulset/s3
  kubectl -n infra-backends rollout status statefulset/s3 --timeout=180s
fi
# The restored gateway-auth Secret is authoritative; align the local token file with it.
.local/bin/admin --kubeconfig "$KUBECONFIG" ensure-gateway-auth
trap - ERR
.local/bin/admin --kubeconfig "$KUBECONFIG" resume
kubectl -n infra-runners rollout status deploy/database-hook --timeout=120s
kubectl -n infra-gateway rollout restart deploy/platform-gateway
kubectl -n infra-gateway rollout status deploy/platform-gateway --timeout=120s
echo "Restore complete; every Database applies once against its restored state"
