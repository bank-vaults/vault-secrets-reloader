# A Self-Documenting Makefile: http://marmelab.com/blog/2016/02/29/auto-documented-makefile.html

export PATH := $(abspath bin/):${PATH}

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Default values for environment variables used in the Makefile
KUBECONFIG ?= $(HOME)/.kube/config
TEST_KIND_CLUSTER ?= vault-secrets-reloader-namespaced
# Target image name
CONTAINER_IMAGE_REF = ghcr.io/bank-vaults/vault-secrets-reloader-namespaced:dev

# Operator and Webhook image name
OPERATOR_VERSION ?= latest
WEBHOOK_VERSION ?= latest

##@ General

# Targets commented with ## will be visible in "make help" info.
# Comments marked with ##@ will be used as categories for a group of targets.

.PHONY: help
default: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "	\033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: up-kind
up-kind: create-kind up ## Start kind development environment

.PHONY: create-kind
create-kind: ## Create kind cluster
	$(KIND_BIN) create cluster --name $(TEST_KIND_CLUSTER)

.PHONY: up
up: ## Start development environment
	$(HELM_BIN) upgrade --install vault-operator oci://ghcr.io/bank-vaults/helm-charts/vault-operator \
		--set image.tag=latest \
		--set image.bankVaultsTag=latest \
		--wait
	kubectl create namespace bank-vaults-infra --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f $(shell pwd)/e2e/deploy/vault/
	sleep 60
	$(HELM_BIN) upgrade --install secrets-webhook oci://ghcr.io/bank-vaults/helm-charts/secrets-webhook \
		--set replicaCount=1 \
		--set image.tag=latest \
		--set image.pullPolicy=IfNotPresent \
		--set podsFailurePolicy=Fail \
		--set secretInit.tag=latest \
		--namespace bank-vaults-infra

.PHONY: down
down: ## Destroy kind development environment
	$(KIND_BIN) delete cluster --name $(TEST_KIND_CLUSTER)

.PHONY: run
run: ## Run manager from your host
	go run main.go -log-level=debug -collector-sync-period=30s -reloader-run-period=1m

##@ Build

.PHONY: build
build: ## Build manager binary
	@mkdir -p build
	go build -race -o build/vault-secrets-reloader-namespaced .

.PHONY: artifacts
artifacts: container-image helm-chart ## Build artifacts

.PHONY: container-image
container-image: ## Build docker image
	docker build -t ${CONTAINER_IMAGE_REF} .

.PHONY: helm-chart
helm-chart: ## Build Helm chart
	@mkdir -p build
	helm package -d build/ deploy/charts/vault-secrets-reloader-namespaced

##@ Checks

.PHONY: check
check: test lint ## Run lint checks and tests

.PHONY: test
test: ## Run tests
	go clean -testcache
	go test -race -v ./...

.PHONY: test-e2e
test-e2e: ## Run e2e tests
	go clean -testcache
	go test -race -v -timeout 900s -tags e2e ./e2e

.PHONY: test-e2e-local
test-e2e-local: container-image ## Run e2e tests locally
	go clean -testcache
	LOAD_IMAGE=${CONTAINER_IMAGE_REF} RELOADER_VERSION=dev OPERATOR_VERSION=$(OPERATOR_VERSION) WEBHOOK_VERSION=$(WEBHOOK_VERSION) LOG_VERBOSE=true ${MAKE} test-e2e

.PHONY: lint
lint: lint-go lint-helm lint-docker lint-yaml ## Run lint checks

.PHONY: lint-go
lint-go: # Run golang lint check
	$(GOLANGCI_LINT_BIN) run

.PHONY: lint-helm
lint-helm: # Run helm lint check
	$(HELM_BIN) lint deploy/charts/vault-secrets-reloader-namespaced

.PHONY: lint-docker
lint-docker: # Run Dockerfile lint check
	$(HADOLINT_BIN) Dockerfile

.PHONY: lint-yaml
lint-yaml:
	$(YAMLLINT_BIN) $(if ${CI},-f github,) --no-warnings .

.PHONY: fmt
fmt: ## Run go fmt against code
	$(GOLANGCI_LINT_BIN) run --fix

.PHONY: license-cache
license-cache: ## Populate license cache
	$(LICENSEI_BIN) cache

.PHONY: license-check
license-check: ## Run license check
	$(LICENSEI_BIN) check
	$(LICENSEI_BIN) header

##@ Autogeneration

.PHONY: generate
generate: gen-helm-docs ## Generate manifests, code, and docs resources

.PHONY: gen-helm-docs
gen-helm-docs: ## Generate Helm chart documentation
	$(HELM_DOCS_BIN) -s file -c deploy/charts/ -t README.md.gotmpl

##@ Deployment

.PHONY: deploy-kind
deploy-kind: upload-kind deploy ## Deploy Reloder controller resources to the kind cluster

.PHONY: upload-kind
upload-kind:
	$(KIND_BIN) load docker-image ${CONTAINER_IMAGE_REF} --name $(TEST_KIND_CLUSTER) ## Load docker image to kind cluster

.PHONY: deploy
deploy: ## Deploy Reloader controller resources to the K8s cluster
	kubectl create namespace bank-vaults-infra --dry-run=client -o yaml | kubectl apply -f -
	$(HELM_BIN) upgrade --install vault-secrets-reloader-namespaced deploy/charts/vault-secrets-reloader-namespaced \
		--set image.tag=dev \
		--set collectorSyncPeriod=30s \
		--set reloaderRunPeriod=1m \
		--set env.VAULT_ROLE=reloader \
		--set env.VAULT_ADDR=https://vault.default.svc.cluster.local:8200 \
		--set env.VAULT_TLS_SECRET=vault-tls \
		--set env.VAULT_TLS_SECRET_NS=bank-vaults-infra \
		--namespace bank-vaults-infra

.PHONY: undeploy
undeploy: ## Clean manager resources from the K8s cluster.
	$(HELM_BIN) uninstall vault-secrets-reloader-namespaced --namespace bank-vaults-infra

##@ Dependencies

deps: bin/golangci-lint bin/licensei bin/kind bin/helm bin/helm-docs
deps: ## Install dependencies

# Dependency versions
GOLANGCI_LINT_VERSION = 2.7.2
LICENSEI_VERSION = 0.9.0
KIND_VERSION = 0.30.0
HELM_VERSION = 4.0.1
HELM_DOCS_VERSION = 1.14.2

# Dependency binaries
GOLANGCI_LINT_BIN := golangci-lint
LICENSEI_BIN := licensei
KIND_BIN := kind
HELM_BIN := helm
HELM_DOCS_BIN := helm-docs

# TODO: add support for hadolint and yamllint dependencies
HADOLINT_BIN := hadolint
YAMLLINT_BIN := yamllint
# If we have "bin" dir, use those binaries instead
ifneq ($(wildcard ./bin/.),)
	GOLANGCI_LINT_BIN := bin/$(GOLANGCI_LINT_BIN)
	LICENSEI_BIN := bin/$(LICENSEI_BIN)
	KIND_BIN := bin/$(KIND_BIN)
	HELM_BIN := bin/$(HELM_BIN)
	HELM_DOCS_BIN := bin/$(HELM_DOCS_BIN)
endif

bin/golangci-lint:
	@mkdir -p bin
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | bash -s -- v${GOLANGCI_LINT_VERSION}

bin/licensei:
	@mkdir -p bin
	curl -sfL https://raw.githubusercontent.com/goph/licensei/master/install.sh | bash -s -- v${LICENSEI_VERSION}
bin/kind:
	@mkdir -p bin
	curl -Lo bin/kind https://kind.sigs.k8s.io/dl/v${KIND_VERSION}/kind-$(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m | sed -e "s/aarch64/arm64/; s/x86_64/amd64/")
	@chmod +x bin/kind

bin/helm:
	@mkdir -p bin
	curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | USE_SUDO=false HELM_INSTALL_DIR=bin DESIRED_VERSION=v$(HELM_VERSION) bash
	@chmod +x bin/helm

bin/helm-docs:
	@mkdir -p bin
	curl -L https://github.com/norwoodj/helm-docs/releases/download/v${HELM_DOCS_VERSION}/helm-docs_${HELM_DOCS_VERSION}_$(shell uname)_x86_64.tar.gz | tar -zOxf - helm-docs > ./bin/helm-docs
	@chmod +x bin/helm-docs
