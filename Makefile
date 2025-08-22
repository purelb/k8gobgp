CONTROLLER_GEN=$(shell go env GOPATH)/bin/controller-gen

.PHONY: manifests
manifests:
	@echo "Generating CRDs and RBAC..."
	@$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./api/..." output:crd:dir=./config/crd/bases output:rbac:dir=./config/rbac

.PHONY: generate
generate:
	@echo "Generating deepcopy code..."
	@$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."