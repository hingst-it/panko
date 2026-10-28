#!/usr/bin/env bash
# Quiesced backup: resources, state, credentials, database contents and lab S3 data.
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
# The administrator command: the checkout's build, or e.g. PANKO_ADMIN="panko admin --config cluster".
read -r -a admin <<< "${PANKO_ADMIN:-.local/bin/admin}"
dir="${BACKUP_DIR:?BACKUP_DIR must name a new, private backup directory}"
if [ -e "$dir" ]; then
  echo "backup directory $dir already exists" >&2
  exit 1
fi
mkdir -p "$dir"
chmod 700 "$dir"
umask 077
# Resume even if pausing or a later step fails; the backup is then incomplete and reported as failed.
trap '"${admin[@]}" --kubeconfig "$KUBECONFIG" resume' EXIT
"${admin[@]}" --kubeconfig "$KUBECONFIG" pause
"${admin[@]}" --kubeconfig "$KUBECONFIG" export --dir "$dir"
kubectl -n infra-backends exec postgres-0 -- pg_dumpall -U postgres --clean --if-exists > "$dir/postgres.sql"
s3=$("${admin[@]}" --kubeconfig "$KUBECONFIG" config-s3)
if [ "$s3" = true ]; then
  kubectl -n infra-backends exec s3-0 -- tar -C /data -cf - . > "$dir/s3.tar"
fi
echo "Backup written to $dir"
