#!/usr/bin/env bash
set -euo pipefail
cluster_name="${CLUSTER_NAME:-infra-lab}"
docker build -t panko-runner:dev -f runner/Dockerfile.runtime .
mise exec -- k3d image import panko-runner:dev -c "$cluster_name"
docker build -t panko-hook:dev --build-arg COMPONENT=hook -f Dockerfile.runtime .
mise exec -- k3d image import panko-hook:dev -c "$cluster_name"
docker build -t panko-gateway:dev --build-arg COMPONENT=gateway -f Dockerfile.runtime .
mise exec -- k3d image import panko-gateway:dev -c "$cluster_name"
