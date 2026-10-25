SHELL := /bin/bash
GATEWAY_PORT ?= 8081
export CLUSTER_NAME ?= infra-lab
PYTHON ?= .venv/bin/python
export KUBECONFIG := $(CURDIR)/.local/kubeconfig
export PATH := $(CURDIR)/.local/bin:$(PATH)
export GOCACHE := $(CURDIR)/.local/go-cache
export GOMODCACHE := $(CURDIR)/.local/go-mod
.PHONY: cluster-up cluster-down bootstrap build deploy test clean help
# Default goal: plain `make` lists the available targets.
.DEFAULT_GOAL := help

## Create the local k3d lab cluster (idempotent).
cluster-up:
	mise exec -- bash cluster/up.sh

## Delete the local k3d lab cluster.
cluster-down:
	mise exec -- k3d cluster delete $(CLUSTER_NAME)

## Build the Go binaries and provision the lab cluster from scratch.
bootstrap: build-go
	mise exec -- bash cluster/bootstrap.sh

## Build the runner container image.
build:
	mise exec -- bash runner/build.sh

## Build the Go binaries and deploy the platform into the lab cluster.
deploy: build-go
	mise exec -- bash cluster/deploy.sh

## Run the end-to-end tests against a deployed lab (requires a running cluster).
test:
	mise exec -- bash test/e2e.sh

.PHONY: test-unit lint migrate-state
.PHONY: test-go lint-go build-go

## Run the Go unit tests with the race detector.
test-go:
	mise exec -- go test -race ./...

## Check Go formatting and run go vet.
lint-go:
	test -z "$$(mise exec -- gofmt -l cmd internal)"
	mise exec -- go vet ./...

## Build all binaries under cmd/ (admin, agent, endpoint, gateway, hook, mirror, panko, runner) into .local/bin.
build-go:
	mkdir -p .local/bin
	mise exec -- go build -o .local/bin/ ./cmd/...

## Run all local unit checks (Go tests with the race detector).
test-unit: test-go

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

## Apply the gateway RBAC manifests to the cluster.
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
