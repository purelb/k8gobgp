CONTROLLER_GEN=$(shell go env GOPATH)/bin/controller-gen
IMG ?= ghcr.io/adamdunstan/registry/k8gobgp:latest

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions included.
.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make <target>\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  %-15s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) } ' $(MAKEFILE_LIST) 

##@ Development

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	@echo "Generating CRDs and RBAC..."
	@$(CONTROLLER_GEN) rbac:roleName=k8gobgp-manager-role crd paths="./api/...;./controllers/..." output:crd:dir=./config/crd/bases output:rbac:dir=./config/rbac

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	@echo "Generating deepcopy code..."
	@$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

##@ Build

.PHONY: build
build: ## Build manager binary.
	go build -o bin/manager ./cmd/manager

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}
