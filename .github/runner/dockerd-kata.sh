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
# kubelet in a nested k3d node requires the guest's kernel log device.
[ -e /dev/kmsg ] || mknod /dev/kmsg c 1 11
mount -t ext4 -o loop,noatime /docker-storage/docker.ext4 /var/lib/docker
# Keep the guest's binfmt superblock alive after the QEMU installer exits.
# Otherwise its registrations disappear with that short-lived container.
mountpoint -q /proc/sys/fs/binfmt_misc || mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc
exec /usr/local/bin/dockerd-entrypoint.sh "$@"
