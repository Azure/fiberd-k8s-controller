GO ?= go
TESTFLAGS ?=

# The build tools are pinned in hack/tools/go.mod and run through go tool.
GO_TOOL := $(GO) tool -modfile=hack/tools/go.mod
CONTROLLER_GEN := $(GO_TOOL) controller-gen
GOLANGCI_LINT := $(GO_TOOL) golangci-lint
SETUP_ENVTEST := $(GO_TOOL) setup-envtest

# envtest runs a real kube-apiserver and etcd. setup-envtest downloads them
# into bin/, which git ignores. CI overrides the version per matrix row.
ENVTEST_K8S_VERSION ?= 1.37.x
ENVTEST_BIN_DIR ?= $(CURDIR)/bin/envtest

.DEFAULT_GOAL := help

.PHONY: help manifests generate fmt vet lint test test-envtest tidy verify

help:
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

manifests:
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=fiberd-k8s-controller paths="./cmd/...;./pkg/controller/..." output:rbac:artifacts:config=config/rbac/controller
	$(CONTROLLER_GEN) rbac:roleName=fiberd-k8s-controller-tenant paths="./pkg/workload/..." output:rbac:artifacts:config=config/rbac/tenant

generate:
	$(CONTROLLER_GEN) object paths="./api/..."

fmt:
	$(GO) fmt ./...

# The envtest tag adds test/envtest, which builds only under it.
vet:
	$(GO) vet -tags envtest ./...

lint:
	$(GOLANGCI_LINT) run

test:
	$(GO) test -race -count=1 $(TESTFLAGS) ./...

test-envtest:
	assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_BIN_DIR) -p path)" && \
		KUBEBUILDER_ASSETS="$$assets" $(GO) test -tags envtest -race -count=1 $(TESTFLAGS) ./test/envtest/...

tidy:
	$(GO) mod tidy
	cd hack/tools && $(GO) mod tidy

verify: manifests generate
	@git diff --exit-code -- api config || { echo "generated files differ, commit the result of make manifests generate"; exit 1; }
	@test -z "$$(git ls-files --others --exclude-standard -- api config)" || { echo "generated files are untracked, commit the result of make manifests generate"; exit 1; }
