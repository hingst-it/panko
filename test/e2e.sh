#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
python3 -m unittest discover -s test -p 'test_*.py'
tofu -chdir=terraform/database fmt -check
tofu -chdir=terraform/database validate
python3 test/e2e.py
