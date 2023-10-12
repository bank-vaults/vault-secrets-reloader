# A Self-Documenting Makefile: http://marmelab.com/blog/2016/02/29/auto-documented-makefile.html

# Default values for environment variables used in the Makefile
KUBECONFIG ?= $(HOME)/.kube/config
TEST_KIND_CLUSTER ?= vault-secrets-reloader
# Target image name
IMG ?= ghcr.io/bank-vaults/vault-secrets-reloader:dev


# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

##@ General

# Targets commented with ## will be visible in "make help" info.
# Comments marked with ##@ will be used as categories for a group of targets.

.PHONY: help
default: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "	\033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Checks

.PHONY: license-check
license-check: ## Run license check
	$(LICENSEI) check
	$(LICENSEI) header

.PHONY: fmt
fmt: ## Run go fmt against code
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-go
lint-go: # Run golang lint check
	$(GOLANGCI_LINT) run $(if ${CI},--out-format github-actions,)

.PHONY: lint-helm
lint-helm: # Run helm lint check
	$(HELM) lint deploy/charts/vault-secrets-reloader

.PHONY: lint-docker
lint-docker: # Run Dockerfile lint check
	$(HADOLINT) --failure-threshold error Dockerfile

.PHONY: lint
lint: lint-go lint-helm lint-docker ## Run lint checks

.PHONY: test
test: ## Run tests
		go clean -testcache
		go test -race -v ./pkg/reloader

.PHONY: test-e2e
test-e2e: ## Run acceptance tests. If running on a local kind cluster, run "make import-test" before this
		go clean -testcache
		go test -race -v -timeout 900s -tags e2e ./e2e

.PHONY: test-e2e-local
test-e2e-local: ## Run e2e tests locally
		go clean -testcache
		LOAD_IMAGE=${IMG} RELOADER_VERSION=dev LOG_VERBOSE=true ${MAKE} test-e2e

##@ Development

.PHONY: run
run: ## Run manager from your host
	go run main.go -log_level=debug -collector_sync_period=30s -reloader_run_period=1m

.PHONY: up
up: ## Start kind development environment
	$(KIND) create cluster --name $(TEST_KIND_CLUSTER)
	sleep 10
	helm upgrade --install vault-operator oci://ghcr.io/bank-vaults/helm-charts/vault-operator \
		--set image.tag=latest \
		--set image.bankVaultsTag=latest \
		--wait
	kubectl create namespace bank-vaults-infra --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f $(shell pwd)/e2e/deploy/vault/
	sleep 60
	helm upgrade --install vault-secrets-webhook oci://ghcr.io/bank-vaults/helm-charts/vault-secrets-webhook \
		--set replicaCount=1 \
		--set image.tag=latest \
		--set image.pullPolicy=IfNotPresent \
		--set podsFailurePolicy=Fail \
		--set vaultEnv.tag=latest \
		--namespace bank-vaults-infra
	kind load docker-image ghcr.io/bank-vaults/vault-secrets-reloader:dev --name $(TEST_KIND_CLUSTER)

.PHONY: down
down: ## Destroy kind development environment
	$(KIND) delete cluster --name $(TEST_KIND_CLUSTER)

##@ Build

.PHONY: artifacts
artifacts: build container-image helm-chart ## Build artifacts

.PHONY: build
build: ## Build manager binary
	@mkdir -p build
	go build -race -o build/vault-secrets-reloader .

.PHONY: container-image
container-image: ## Build docker image
	docker build -t ${IMG} .

.PHONY: helm-chart
helm-chart: ## Build Helm chart
	@mkdir -p build
	helm package -d build/ deploy/charts/vault-secrets-reloader

##@ Autogeneration

.PHONY: gen-helm-docs
gen-helm-docs: ## Generate Helm chart documentation
	$(HELM_DOCS) -s file -c deploy/charts/ -t README.md.gotmpl

.PHONY: generate
generate: gen-helm-docs ## Generate manifests, code, and docs resources

##@ Deployment

.PHONY: deploy
deploy: ## Deploy manager resources to the K8s cluster
	kubectl create namespace bank-vaults-infra --dry-run=client -o yaml | kubectl apply -f -
	$(HELM) upgrade --install vault-secrets-reloader deploy/charts/vault-secrets-reloader \
		--set image.tag=dev \
		--set collectorSyncPeriod=30s \
		--set reloaderRunPeriod=1m \
		--namespace bank-vaults-infra

.PHONY: undeploy
undeploy: ## Clean manager resources from the K8s cluster.
	$(HELM) uninstall vault-secrets-reloader --namespace bank-vaults-infra

##@ Dependencies

# Dependency tool chain
GOLANGCI_VERSION = 1.53.3
LICENSEI_VERSION = 0.8.0
KIND_VERSION = 0.20.0
HELM_DOCS_VERSION = 1.11.0

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ENVTEST ?= $(or $(shell which setup-envtest),$(LOCALBIN)/setup-envtest)
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

GOLANGCI_LINT ?= $(or $(shell which golangci-lint),$(LOCALBIN)/golangci-lint)
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(LOCALBIN)/golangci-lint || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | bash -s -- v${GOLANGCI_VERSION}

LICENSEI ?= $(or $(shell which licensei),$(LOCALBIN)/licensei)
$(LICENSEI): $(LOCALBIN)
	test -s $(LOCALBIN)/licensei || curl -sfL https://raw.githubusercontent.com/goph/licensei/master/install.sh | bash -s -- v${LICENSEI_VERSION}

KIND ?= $(or $(shell which kind),$(LOCALBIN)/kind)
$(KIND): $(LOCALBIN)
	@if [ ! -s "$(LOCALBIN)/kind" ]; then \
		curl -Lo $(LOCALBIN)/kind https://kind.sigs.k8s.io/dl/v${KIND_VERSION}/kind-$(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m | sed -e "s/aarch64/arm64/; s/x86_64/amd64/"); \
		chmod +x $(LOCALBIN)/kind; \
	fi

HELM ?= $(or $(shell which helm),$(LOCALBIN)/helm)
$(HELM): $(LOCALBIN)
	test -s $(LOCALBIN)/helm || curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | USE_SUDO=false HELM_INSTALL_DIR=$(LOCALBIN) bash

HELM_DOCS ?= $(or $(shell which helm-docs),$(LOCALBIN)/helm-docs)
$(HELM_DOCS): $(LOCALBIN)
	@if [ ! -s "$(LOCALBIN)/helm-docs" ]; then \
		curl -L https://github.com/norwoodj/helm-docs/releases/download/v${HELM_DOCS_VERSION}/helm-docs_${HELM_DOCS_VERSION}_$(shell uname)_x86_64.tar.gz | tar -zOxf - helm-docs > ./bin/helm-docs; \
		chmod +x $(LOCALBIN)/helm-docs; \
	fi

# TODO: add support for hadolint and yamllint dependencies
HADOLINT ?= hadolint
YAMLLINT ?= yamllint

.PHONY: deps
deps: $(HELM) $(HELM_DOCS) $(ENVTEST) $(GOLANGCI_LINT) $(LICENSEI) $(KIND) ## Download and install dependencies
