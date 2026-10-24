#!/usr/bin/env bash
# Acceptance for roadmap item 3 in a dedicated lab: run as
#   PANKO_CONFIG=.local/single-team.json CLUSTER_NAME=... KUBECONFIG=... bash test/single_team.sh
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
: "${PANKO_CONFIG:?PANKO_CONFIG must point to a configuration with the single team home and s3 false}"
export PANKO_CONFIG
make cluster-up bootstrap build deploy
python3 test/single_team_e2e.py create
make build deploy
python3 test/single_team_e2e.py after-upgrade
python3 test/single_team_e2e.py delete
