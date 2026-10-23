#!/usr/bin/env bash
set -euo pipefail
cluster_name="${CLUSTER_NAME:-infra-lab}"
# The runner image carries no engine or module; Jobs assemble both at start.
for component in runner hook gateway; do
  docker build -t "panko-$component:dev" --build-arg COMPONENT="$component" -f Dockerfile.runtime .
  mise exec -- k3d image import "panko-$component:dev" -c "$cluster_name"
done
