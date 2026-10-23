#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
make test-unit lint lint-go
python3 test/rbac_e2e.py
python3 test/e2e.py
bash test/gateway.sh
python3 test/observability_e2e.py
python3 test/runner-smoke.py
python3 test/admin_e2e.py
