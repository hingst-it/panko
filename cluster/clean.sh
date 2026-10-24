#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
teams=$(.local/bin/admin teams)
for team in $teams; do
  kubectl -n "$team" delete databases --all --wait=true --timeout=300s
done
# Keep the platform and Postgres running; cluster-down removes the entire lab.
