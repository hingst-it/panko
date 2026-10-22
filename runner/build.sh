#!/usr/bin/env bash
set -euo pipefail
docker build -t panko-runner:dev -f runner/Dockerfile .
mise exec -- k3d image import panko-runner:dev -c infra-lab
