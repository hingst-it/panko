SHELL := /bin/bash
GATEWAY_PORT ?= 8081
export CLUSTER_NAME ?= infra-lab
PYTHON ?= .venv/bin/python
export KUBECONFIG := $(CURDIR)/.local/kubeconfig
export PATH := $(CURDIR)/.local/bin:$(PATH)
export GOCACHE := $(CURDIR)/.local/go-cache
export GOMODCACHE := $(CURDIR)/.local/go-mod
.PHONY: cluster-up cluster-down bootstrap build deploy test clean
cluster-up:
	mise exec -- bash cluster/up.sh
cluster-down:
	mise exec -- k3d cluster delete $(CLUSTER_NAME)
bootstrap: build-go
	mise exec -- bash cluster/bootstrap.sh
build:
	mise exec -- bash runner/build.sh
deploy: build-go
	mise exec -- bash cluster/deploy.sh
test:
	mise exec -- bash test/e2e.sh
.PHONY: test-unit lint migrate-state
.PHONY: test-go lint-go build-go
test-go:
	mise exec -- go test -race ./...
lint-go:
	test -z "$$(mise exec -- gofmt -l cmd internal)"
	mise exec -- go vet ./...
build-go:
	mkdir -p .local/bin
	mise exec -- go build -o .local/bin/ ./cmd/...
test-unit: test-go
lint:
	$(PYTHON) -m ruff check test tools
migrate-state: build-go
	.local/bin/admin --kubeconfig "$(KUBECONFIG)" migrate-state
clean: build-go
	mise exec -- bash cluster/clean.sh
.PHONY: gateway-rbac gateway-forward test-gateway
gateway-rbac: build-go
	.local/bin/admin --kubeconfig "$(KUBECONFIG)" gateway-rbac | kubectl apply -f -
gateway-forward:
	mise exec -- kubectl -n infra-gateway port-forward --address 127.0.0.1 service/platform-gateway $(GATEWAY_PORT):8080
test-gateway:
	mise exec -- bash test/gateway.sh
