#!/bin/sh
set -eu
: "${MODULE_REPO:?MODULE_REPO is required}"
export HOME=/tmp GIT_TERMINAL_PROMPT=0
case "$MODULE_REPO" in
  git@*|ssh://*)
    export GIT_SSH_COMMAND='ssh -F /dev/null -i /etc/git-secret/ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/etc/git-secret/known_hosts -o BatchMode=yes'
    ;;
esac

# An emptyDir starts clean with each Pod. Never serve a partially cloned repository.
git clone --mirror "$MODULE_REPO" /repositories/module.git
# git-sync fetches full SHAs, including older commits reachable from mirrored refs.
git -C /repositories/module.git config uploadpack.allowReachableSHA1InWant true
# Serve only once the pinned revision is present: during a rolling update the previous pod keeps
# serving until this one is ready, so Jobs for a just-pinned commit do not hit a stale clone.
if [ -n "${MODULE_REVISION:-}" ]; then
  until git -C /repositories/module.git cat-file -e "$MODULE_REVISION^{commit}" 2>/dev/null; do
    echo "pinned revision $MODULE_REVISION is not in $MODULE_REPO yet; fetching again in 10 s" >&2
    sleep 10
    git -C /repositories/module.git remote update --prune || true
  done
fi
(
  while sleep 60; do
    if ! git -C /repositories/module.git remote update --prune; then
      echo 'module mirror update failed; serving the last fetched refs' >&2
    fi
  done
) &
# No detailed errors or writable protocol services, even if repo config changes.
exec git daemon --reuseaddr --listen=0.0.0.0 --port=9418 \
  --export-all --base-path=/repositories --strict-paths --no-informative-errors \
  --disable=receive-pack --forbid-override=receive-pack \
  --disable=upload-archive --forbid-override=upload-archive /repositories/module.git
