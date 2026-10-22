SHELL := /bin/bash
GATEWAY_PORT ?= 8081
export KUBECONFIG := $(CURDIR)/.local/kubeconfig
export PATH := $(CURDIR)/.local/bin:$(PATH)
.PHONY: cluster-up cluster-down bootstrap build deploy test clean
cluster-up:
	mise exec -- bash cluster/up.sh
cluster-down:
	mise exec -- k3d cluster delete infra-lab
bootstrap:
	mise exec -- bash cluster/bootstrap.sh
build:
	mise exec -- bash runner/build.sh
deploy:
	mise exec -- bash cluster/deploy.sh
test:
	mise exec -- bash test/e2e.sh
clean:
	mise exec -- bash cluster/clean.sh
.PHONY: gateway-rbac gateway-forward test-gateway
gateway-rbac:
	mise exec -- bash -c 'python3 gateway/rbac.py | kubectl apply -f -'
gateway-forward:
	mise exec -- kubectl -n infra-runners port-forward --address 127.0.0.1 service/platform-gateway $(GATEWAY_PORT):8080
test-gateway:
	mise exec -- bash test/gateway.sh
