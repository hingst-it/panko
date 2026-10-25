#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
python3 test/rbac_e2e.py
python3 test/e2e.py
bash test/gateway.sh
python3 test/observability_e2e.py
python3 test/runner-smoke.py
python3 test/admin_e2e.py
s3=$(.local/bin/admin config-s3)
if [ "$s3" = true ]; then
  python3 test/state_backend_e2e.py
fi
python3 test/failures_e2e.py
python3 test/mirror_e2e.py
python3 test/cli_e2e.py
python3 test/drift_e2e.py
python3 test/ui_e2e.py
python3 test/egress_e2e.py
python3 test/remote_e2e.py
