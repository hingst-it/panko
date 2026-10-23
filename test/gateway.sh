#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
kubectl -n infra-gateway port-forward --address 127.0.0.1 service/platform-gateway 18080:8080 > .local/gateway-forward.log 2>&1 &
forward_pid=$!
trap 'kill "$forward_pid" 2>/dev/null || true' EXIT
python3 test/gateway_e2e.py
# Real parser and client generation, in a container; no global Java/npm installation.
# Run as the calling user so the generated files stay writable on Linux hosts.
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/.local:/local" openapitools/openapi-generator-cli:v7.26.0 \
  validate -i /local/openapi.json > .local/openapi-validate.log 2>&1
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/.local:/local" openapitools/openapi-generator-cli:v7.26.0 \
  generate -i /local/openapi.json -g python -o /local/generated-client > .local/openapi-generate.log 2>&1
python3 -m compileall -q .local/generated-client/openapi_client
printf '%s\n' 'OpenAPI Generator validated the OpenAPI 3.1 document and generated a Python client'
