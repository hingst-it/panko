#!/usr/bin/env bash
# Rehearsal for roadmap item 2: back up a lab, delete it, restore into a fresh lab and verify.
set -euo pipefail
export CLUSTER_NAME="${CLUSTER_NAME:-infra-lab-restore}"
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/restore-kubeconfig}"
backup="${BACKUP_DIR:-.local/backup-rehearsal-$(date +%s)}"
make cluster-up bootstrap build deploy
python3 test/restore_e2e.py prepare
make backup BACKUP_DIR="$backup"
mise exec -- k3d cluster delete "$CLUSTER_NAME"
make cluster-up bootstrap build deploy
make restore BACKUP_DIR="$backup"
python3 test/restore_e2e.py verify
mise exec -- k3d cluster delete "$CLUSTER_NAME"
