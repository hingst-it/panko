#!/usr/bin/env bash
set -euo pipefail
cluster_name="${CLUSTER_NAME:-infra-lab}"
mkdir -p .local
# One panko-server image serves every role; runner Jobs copy the engine and fetch the module at start.
docker build -t panko-server:dev -f Dockerfile.server .
# The module mirror needs git-daemon and SSH, but no Go binary.
docker build -t panko-module-mirror:dev -f cluster/Dockerfile.module-mirror .
# A tag derived from the image content changes the rendered pods exactly when an image changed, so
# make deploy rolls them out; an unchanged :dev tag would leave the old binaries running.
tags=()
: > .local/image-tags.env
for image in panko-server panko-module-mirror; do
  tag="dev-$(docker image inspect --format '{{.Id}}' "$image:dev" | cut -d: -f2 | cut -c1-12)"
  docker tag "$image:dev" "$image:$tag"
  tags+=("$image:dev" "$image:$tag")
  printf '%s=%s\n' "$(printf '%s' "$image" | tr 'a-z-' 'A-Z_')_TAG" "$tag" >> .local/image-tags.env
done
if mise exec -- k3d cluster list --no-headers 2>/dev/null | awk -v name="$cluster_name" '$1 == name { found=1 } END { exit !found }'; then
  mise exec -- k3d image import "${tags[@]}" -c "$cluster_name"
fi
