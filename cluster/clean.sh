#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
for team in $(.local/bin/admin teams); do
  kubectl -n "$team" delete databases --all --wait=true --timeout=300s
done
# Keep the platform and Postgres running; cluster-down removes the entire lab.
