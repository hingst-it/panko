#!/usr/bin/env bash
# Acceptance for roadmap item 3 in a dedicated lab: run as
#   PANKO_CONFIG=.local/single-team.json CLUSTER_NAME=... KUBECONFIG=... bash test/single_team.sh
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
: "${PANKO_CONFIG:?PANKO_CONFIG must point to a configuration with the single team home and s3 false}"
export PANKO_CONFIG
# The Makefile sets KUBECONFIG itself, so it only follows a make argument; never touch the default lab.
if [ "$KUBECONFIG" = "$PWD/.local/kubeconfig" ]; then
  echo "use a dedicated KUBECONFIG for this acceptance test" >&2
  exit 1
fi
lab=(CLUSTER_NAME="${CLUSTER_NAME:?CLUSTER_NAME must name a dedicated lab}" KUBECONFIG="$KUBECONFIG")
make "${lab[@]}" cluster-up bootstrap build deploy
python3 test/single_team_e2e.py create
make "${lab[@]}" build deploy
python3 test/single_team_e2e.py after-upgrade
python3 test/single_team_e2e.py delete
