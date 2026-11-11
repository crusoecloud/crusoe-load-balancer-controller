# Tool versions and workflow logic live in mise.toml.
IMG ?= pphelan001/controller:latest
CONTAINER_TOOL ?= docker
PLATFORMS ?= linux/arm64,linux/amd64
LOCALBIN ?= $(CURDIR)/bin
KUBECTL ?= kubectl
ignore-not-found ?= false
export IMG CONTAINER_TOOL PLATFORMS LOCALBIN KUBECTL
export IGNORE_NOT_FOUND = $(ignore-not-found)
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec
.DEFAULT_GOAL := all

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: all
all: ## Run build.
	mise run build

.PHONY: manifests
manifests: ## Run manifests.
	mise run manifests

.PHONY: dev
dev: ## Run dev.
	mise run dev

.PHONY: generate
generate: ## Run generate.
	mise run generate

.PHONY: test
test: ## Run test:integration.
	mise run test:integration

.PHONY: test-e2e
test-e2e: ## Run test:e2e.
	mise run test:e2e

.PHONY: lint
lint: ## Run go:lint:check.
	mise run go:lint:check

.PHONY: ci
ci: ## Run go:lint:check.
	mise run go:lint:check

.PHONY: lint-fix
lint-fix: ## Run go:lint.
	mise run go:lint

.PHONY: test-ci
test-ci: ## Run go:mod-tidy:check.
	mise run go:mod-tidy:check

.PHONY: build
build: ## Run build.
	mise run build

.PHONY: run
run: ## Run run.
	mise run run

.PHONY: docker-build
docker-build: ## Run image:build.
	mise run image:build

.PHONY: docker-push
docker-push: ## Run image:push.
	mise run image:push

.PHONY: docker-buildx
docker-buildx: ## Run image:buildx.
	mise run image:buildx

.PHONY: build-installer
build-installer: ## Run installer:build.
	mise run installer:build

.PHONY: install
install: ## Run crds:install.
	mise run crds:install

.PHONY: uninstall
uninstall: ## Run crds:uninstall.
	mise run crds:uninstall

.PHONY: deploy
deploy: ## Run deploy.
	mise run deploy

.PHONY: undeploy
undeploy: ## Run undeploy.
	mise run undeploy

.PHONY: build-deps
build-deps: ## Install configured tooling.
	mise install

.PHONY: kustomize
kustomize: ## Install configured tooling.
	mise install go:sigs.k8s.io/kustomize/kustomize/v5

.PHONY: controller-gen
controller-gen: ## Install configured tooling.
	mise install go:sigs.k8s.io/controller-tools/cmd/controller-gen

.PHONY: envtest
envtest: ## Install configured tooling.
	mise install go:sigs.k8s.io/controller-runtime/tools/setup-envtest

.PHONY: golangci-lint
golangci-lint: ## Install configured tooling.
	mise install golangci-lint
