#!/bin/sh
set -eu
# CSI filesystem volumes enter Kata through virtiofs, which cannot be used as
# an OverlayFS upper directory. This loop device belongs to the Kata guest,
# not to a hosting node. The backing file dies with the per-job ephemeral PVC.
mkdir -p /docker-storage /var/lib/docker
if [ ! -s /docker-storage/docker.ext4 ]; then
  truncate -s "${DOCKER_DISK_SIZE:-45G}" /docker-storage/docker.ext4
  mkfs.ext4 -q -F /docker-storage/docker.ext4
fi
[ -e /dev/loop-control ] || mknod /dev/loop-control c 10 237
[ -e /dev/loop0 ] || mknod /dev/loop0 b 7 0
mount -t ext4 -o loop,noatime /docker-storage/docker.ext4 /var/lib/docker
exec /usr/local/bin/dockerd-entrypoint.sh "$@"
