#!/usr/bin/env bash
set -euo pipefail
export KUBECONFIG="${KUBECONFIG:-$PWD/.local/kubeconfig}"
# Refuse an unsafe backend switch when legacy states still exist.
.local/bin/admin --kubeconfig "$KUBECONFIG" preflight
kubectl create namespace infra-gateway --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f api/database.yaml -f api/state-backends.yaml
.local/bin/admin runner-rbac | kubectl apply -f -
# Read the hook's literal setting locally, before applying the new Deployment.
module_mirror=$(kubectl create --dry-run=client --validate=false -f controller/deployment.yaml -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="MODULE_MIRROR")].value}')
if [ -n "$module_mirror" ]; then
  .local/bin/admin --ssh-key-file "${MODULE_SSH_KEY_FILE:-.local/module-deploy-key}" module-mirror-secret | kubectl apply -f -
  kubectl apply -f cluster/module-mirror.yaml
  kubectl -n infra-runners rollout restart deploy/module-mirror
  kubectl -n infra-runners rollout status deploy/module-mirror --timeout=180s
else
  .local/bin/admin --ssh-key-file "${MODULE_SSH_KEY_FILE:-.local/module-deploy-key}" module-secret | kubectl apply -f -
fi
# Team S3 users, buckets and approved profiles; existing keys are kept.
s3=$(.local/bin/admin config-s3)
if [ "$s3" = true ]; then
  .local/bin/admin --kubeconfig "$KUBECONFIG" ensure-s3-teams
  kubectl -n infra-backends delete job s3-onboard --ignore-not-found --wait=true
  kubectl apply -f cluster/s3-onboard.yaml
  kubectl -n infra-backends wait --for=condition=complete job/s3-onboard --timeout=180s
  .local/bin/admin s3-profiles | kubectl apply -f -
fi
.local/bin/admin --kubeconfig "$KUBECONFIG" ensure-installation-id
.local/bin/admin --kubeconfig "$KUBECONFIG" ensure-mirror-tls
.local/bin/admin --kubeconfig "$KUBECONFIG" mirror-ca | kubectl apply -f -
kubectl apply -f cluster/provider-mirror.yaml
kubectl -n infra-runners rollout restart deploy/provider-mirror
kubectl -n infra-runners rollout status deploy/provider-mirror --timeout=120s
kubectl apply -f controller/deployment.yaml
kubectl -n infra-runners rollout restart deploy/database-hook
kubectl -n infra-runners rollout status deploy/database-hook --timeout=120s
if [ -n "$module_mirror" ]; then
  # The replacement hook no longer creates Jobs that mount team key copies.
  teams=$(.local/bin/admin teams)
  for team in $teams; do
    kubectl -n "$team" delete secret panko-module-git --ignore-not-found
  done
fi
kubectl apply -f controller/compositecontroller.yaml
.local/bin/admin --kubeconfig "$KUBECONFIG" ensure-gateway-auth
.local/bin/admin --kubeconfig "$KUBECONFIG" gateway-rbac | kubectl apply -f -
kubectl apply -f gateway/deployment.yaml
kubectl -n infra-gateway rollout restart deploy/platform-gateway
kubectl -n infra-gateway rollout status deploy/platform-gateway --timeout=120s
# Remove pre-isolation privileges and gateway only after the replacement is ready.
kubectl -n infra-runners delete rolebinding tofu-state --ignore-not-found
kubectl -n infra-runners delete role tofu-state --ignore-not-found
kubectl -n infra-runners delete deployment,service,serviceaccount platform-gateway --ignore-not-found
kubectl -n infra-runners delete secret gateway-auth --ignore-not-found
