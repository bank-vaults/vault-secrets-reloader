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
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

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

.PHONY: lint
# lint-helm lint-docker lint-yaml
lint: lint-go ## Run lint checks

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
	# kubectl kustomize https://github.com/bank-vaults/vault-operator/deploy/rbac | kubectl apply -f -
	kubectl create namespace bank-vaults-infra --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f $(shell pwd)/e2e/deploy/vault/
	sleep 60
	helm upgrade --install vault-secrets-webhook oci://ghcr.io/bank-vaults/helm-charts/vault-secrets-webhook \
    --set replicaCount=1 \
    --set image.tag=latest \
    --set image.pullPolicy=IfNotPresent \
    --set podsFailurePolicy=Fail \
    --set secretsFailurePolicy=Fail \
    --set vaultEnv.tag=latest \
    --namespace bank-vaults-infra

.PHONY: down
down: ## Destroy kind development environment
	$(KIND) delete cluster --name $(TEST_KIND_CLUSTER)

##@ Build

.PHONY: build
build: ## Build manager binary
	@mkdir -p build
	go build -race -o build/controller .

##@ Deployment

##@ Dependencies

# Dependency tool chain
GOLANGCI_VERSION = 1.53.3
LICENSEI_VERSION = 0.8.0
KIND_VERSION = 0.20.0
# CODE_GENERATOR_VERSION = 0.27.1
# HELM_DOCS_VERSION = 1.11.0
# KUSTOMIZE_VERSION = 5.1.0
# CONTROLLER_TOOLS_VERSION = 0.12.1

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# KUSTOMIZE ?= $(or $(shell which kustomize),$(LOCALBIN)/kustomize)
# $(KUSTOMIZE): $(LOCALBIN)
# 	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q v$(KUSTOMIZE_VERSION); then \
# 		echo "$(LOCALBIN)/kustomize version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
# 		rm -rf $(LOCALBIN)/kustomize; \
# 	fi
# 	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) GO111MODULE=on go install sigs.k8s.io/kustomize/kustomize/v5@v$(KUSTOMIZE_VERSION)
#
# CONTROLLER_GEN ?= $(or $(shell which controller-gen),$(LOCALBIN)/controller-gen)
# $(CONTROLLER_GEN): $(LOCALBIN)
# 	test -s $(LOCALBIN)/controller-gen && $(LOCALBIN)/controller-gen --version | grep -q v$(CONTROLLER_TOOLS_VERSION) || \
# 	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v$(CONTROLLER_TOOLS_VERSION)
#
# ENVTEST ?= $(or $(shell which setup-envtest),$(LOCALBIN)/setup-envtest)
# $(ENVTEST): $(LOCALBIN)
# 	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

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

# HELM ?= $(or $(shell which helm),$(LOCALBIN)/helm)
# $(HELM): $(LOCALBIN)
# 	test -s $(LOCALBIN)/helm || curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | USE_SUDO=false HELM_INSTALL_DIR=$(LOCALBIN) bash
#
# HELM_DOCS ?= $(or $(shell which helm-docs),$(LOCALBIN)/helm-docs)
# $(HELM_DOCS): $(LOCALBIN)
# 	@if [ ! -s "$(LOCALBIN)/helm-docs" ]; then \
# 		curl -L https://github.com/norwoodj/helm-docs/releases/download/v${HELM_DOCS_VERSION}/helm-docs_${HELM_DOCS_VERSION}_$(shell uname)_x86_64.tar.gz | tar -zOxf - helm-docs > ./bin/helm-docs; \
# 		chmod +x $(LOCALBIN)/helm-docs; \
# 	fi

# TODO: add support for hadolint and yamllint dependencies
HADOLINT ?= hadolint
YAMLLINT ?= yamllint

.PHONY: deps
deps: $(HELM) $(CONTROLLER_GEN) $(KUSTOMIZE) $(KIND)
deps: $(HELM_DOCS) $(ENVTEST) $(GOLANGCI_LINT) $(LICENSEI)
deps: ## Download and install dependencies
