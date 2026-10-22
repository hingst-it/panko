SHELL := /bin/bash
export KUBECONFIG := $(CURDIR)/.local/kubeconfig
export PATH := $(CURDIR)/.local/bin:$(PATH)
.PHONY: cluster-up cluster-down bootstrap build deploy test clean
cluster-up:
	bash cluster/up.sh
cluster-down:
	mise exec -- k3d cluster delete infra-lab
bootstrap:
	bash cluster/bootstrap.sh
build:
	bash runner/build.sh
deploy:
	bash cluster/deploy.sh
test:
	bash test/e2e.sh
clean:
	bash cluster/clean.sh
