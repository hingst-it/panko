#!/usr/bin/env bash
# Rehearsal for #16: back up a lab, delete it, restore into a fresh lab and verify.
set -euo pipefail
export CLUSTER_NAME="${CLUSTER_NAME:-infra-lab-restore}"
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/restore-kubeconfig}"
# The Makefile sets KUBECONFIG itself, so it only follows a make argument; never touch the default lab.
if [ "$KUBECONFIG" = "$PWD/.local/kubeconfig" ]; then
  echo "use a dedicated KUBECONFIG for the rehearsal" >&2
  exit 1
fi
lab=(CLUSTER_NAME="$CLUSTER_NAME" KUBECONFIG="$KUBECONFIG")
backup="${BACKUP_DIR:-.local/backup-rehearsal-$(date +%s)}"
make "${lab[@]}" cluster-up build deploy
python3 test/restore_e2e.py prepare
make "${lab[@]}" backup BACKUP_DIR="$backup"
mise exec -- k3d cluster delete "$CLUSTER_NAME"
make "${lab[@]}" cluster-up build deploy
make "${lab[@]}" restore BACKUP_DIR="$backup"
python3 test/restore_e2e.py verify
mise exec -- k3d cluster delete "$CLUSTER_NAME"
