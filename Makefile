# Tools are pinned via "go run <module>@<version>" so that local builds and CI
# resolve identical versions with no install step. CONTROLLER_GEN must stay in
# step with the version stamped into config/crd/bases/*.yaml, otherwise
# "make manifests" produces a spurious diff.
CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0
KUSTOMIZE = go run sigs.k8s.io/kustomize/kustomize/v4@v4.5.2

IMG ?= ghcr.io/purelb/k8gobgp:latest

# Endpoint used by "make run" to reach an externally-run gobgpd.
GOBGP_ENDPOINT ?= localhost:50051

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

##@ Deployment

.PHONY: install
install: ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	kubectl apply -f config/crd/bases/

.PHONY: deploy
# LoadRestrictionsNone is required because config/default/kustomization.yaml
# refers to files above its own root (../crd/bases, ../rbac, ../daemonset),
# which kustomize forbids by default.
deploy: ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone config/default | \
	sed -E 's|image: ghcr.io/purelb/k8gobgp:[^[:space:]]+|image: ${IMG}|g' | \
	kubectl apply -f -

.PHONY: run
run: ## Run the controller locally against an externally-run gobgpd.
	go run ./cmd/manager --gobgp-endpoint=$(GOBGP_ENDPOINT)

##@ Testing

.PHONY: test
test: ## Run unit tests.
	go test ./... -short -v

.PHONY: test-e2e
test-e2e: ## Run E2E tests against the current kubeconfig context.
	@echo "Running E2E tests against current kubeconfig context..."
	@echo "KUBECONFIG: $${KUBECONFIG:-~/.kube/config}"
	go test -v -tags=e2e ./test/e2e/...
