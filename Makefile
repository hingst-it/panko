SHELL := /bin/bash
GATEWAY_PORT ?= 8081
export CLUSTER_NAME ?= infra-lab
PYTHON ?= .venv/bin/python
export KUBECONFIG := $(CURDIR)/.local/kubeconfig
export PATH := $(CURDIR)/.local/bin:$(PATH)
export GOCACHE := $(CURDIR)/.local/go-cache
export GOMODCACHE := $(CURDIR)/.local/go-mod
.PHONY: cluster-up cluster-down build deploy test test-shard clean help
# Default goal: plain `make` lists the available targets.
.DEFAULT_GOAL := help

## Create the local k3d lab cluster (idempotent).
cluster-up:
	mise exec -- bash cluster/up.sh

## Delete the local k3d lab cluster.
cluster-down:
	mise exec -- k3d cluster delete $(CLUSTER_NAME)

## Build the panko-server and module mirror images and import them into the lab cluster.
build:
	mise exec -- bash runner/build.sh

## Install or upgrade Panko in the lab cluster with panko install and the chart (cluster/lab-values.yaml).
deploy: build-go
	mise exec -- bash cluster/deploy.sh

## Run the end-to-end tests against a deployed lab (requires a running cluster).
test:
	mise exec -- bash test/e2e.sh

## Run one e2e shard (a, b, or c) against a deployed lab (requires a running cluster).
test-shard:
	mise exec -- bash test/e2e-shard.sh "$(SHARD)"

.PHONY: test-unit lint migrate-state
.PHONY: test-go lint-go build-go lint-chart

## Run the Go unit tests with the race detector.
test-go:
	mise exec -- go test -race ./...

## Check Go formatting and run go vet.
lint-go:
	test -z "$$(mise exec -- gofmt -l cmd internal)"
	mise exec -- go vet ./...

## Build all binaries under cmd/ (panko, panko-server and the legacy admin wrapper) into .local/bin.
build-go:
	mkdir -p .local/bin
	mise exec -- go build -o .local/bin/ ./cmd/...

## Lint and render the Helm chart; the rendering must be reproducible and free of Secrets.
lint-chart:
	mise exec -- helm lint --strict charts/panko
	mise exec -- helm lint --strict charts/panko --values examples/gitops/values.yaml
	mise exec -- helm template panko charts/panko --namespace panko-system --include-crds --values examples/gitops/values.yaml > .local/rendered-1.yaml
	mise exec -- helm template panko charts/panko --namespace panko-system --include-crds --values examples/gitops/values.yaml > .local/rendered-2.yaml
	cmp .local/rendered-1.yaml .local/rendered-2.yaml
	! grep -q '^kind: Secret$$' .local/rendered-1.yaml

## Run all local unit checks (Go tests with the race detector, chart lint and rendering).
test-unit: test-go lint-chart

## Lint the Python tooling with ruff.
lint:
	$(PYTHON) -m ruff check test tools

## Move Tofu state secrets from infra-runners into the team state namespaces; needs a maintenance window (docs/operations.de.md).
migrate-state: build-go
	.local/bin/admin --kubeconfig "$(KUBECONFIG)" migrate-state

## Delete every team's Database resources; the platform and Postgres keep running (cluster/clean.sh).
clean: build-go
	mise exec -- bash cluster/clean.sh

.PHONY: backup restore

## Quiesced backup of resources, state, credentials, database contents and lab S3 data to BACKUP_DIR=<dir>.
backup: build-go
	mise exec -- bash cluster/backup.sh

## Restore a backup from BACKUP_DIR=<dir> into a freshly deployed installation (cluster/restore.sh).
restore: build-go
	mise exec -- bash cluster/restore.sh

.PHONY: gateway-rbac gateway-forward test-gateway

## Apply gateway RBAC for every exposed CRD in the cluster, including CRDs added after installation.
gateway-rbac: build-go
	.local/bin/admin --kubeconfig "$(KUBECONFIG)" gateway-rbac | kubectl apply -f -

## Forward the platform-gateway service to localhost for manual testing.
gateway-forward:
	mise exec -- kubectl -n infra-gateway port-forward --address 127.0.0.1 service/platform-gateway $(GATEWAY_PORT):8080

## Run the gateway integration tests against a deployed lab.
test-gateway:
	mise exec -- bash test/gateway.sh

## List every public target with a one-line description.
help:
	@echo "Panko make targets:"
	@awk '/^## / {desc = substr($$0, 4); next} /^[^\t#]+:/ {name = $$0; sub(/:.*/, "", name); if (desc != "") {printf "  %-16s %s\n", name, desc; desc = ""}}' $(MAKEFILE_LIST)
