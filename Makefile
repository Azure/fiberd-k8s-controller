GO ?= go
TESTFLAGS ?=

# The build tools are pinned in hack/tools/go.mod and run through go tool.
GO_TOOL := $(GO) tool -modfile=hack/tools/go.mod
CONTROLLER_GEN := $(GO_TOOL) controller-gen
GOLANGCI_LINT := $(GO_TOOL) golangci-lint
SETUP_ENVTEST := $(GO_TOOL) setup-envtest
KUSTOMIZE := $(GO_TOOL) kustomize

# envtest runs a real kube-apiserver and etcd. setup-envtest downloads them
# into bin/, which git ignores. CI overrides the version per matrix row.
ENVTEST_K8S_VERSION ?= 1.37.x
ENVTEST_BIN_DIR ?= $(CURDIR)/bin/envtest

CONTROLLER_IMAGE ?= localhost/fiberd-k8s-controller:dev
CONTROLLER_DOCKERFILE := dockerfiles/controller/Dockerfile

.DEFAULT_GOAL := help

.PHONY: help manifests generate chart fmt vet lint test test-envtest test-chart image image-multiarch kind-deps tidy verify

help:
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

manifests:
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=fiberd-k8s-controller paths="./cmd/...;./pkg/controller/..." output:rbac:artifacts:config=config/rbac/controller
	$(CONTROLLER_GEN) rbac:roleName=fiberd-k8s-controller-tenant paths="./pkg/workload/..." output:rbac:artifacts:config=config/rbac/tenant

generate:
	$(CONTROLLER_GEN) object paths="./api/..."

# The chart's crds and templates/generated come from config, never by hand.
chart:
	$(GO) run ./hack/chart -kustomize "$(KUSTOMIZE)"

fmt:
	$(GO) fmt ./...

# The envtest and chart tags add test/envtest and test/chart, which build only
# under them.
vet:
	$(GO) vet -tags envtest,chart ./...

lint:
	$(GOLANGCI_LINT) run

test:
	$(GO) test -race -count=1 $(TESTFLAGS) ./...

test-envtest:
	assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_BIN_DIR) -p path)" && \
		KUBEBUILDER_ASSETS="$$assets" $(GO) test -tags envtest -race -count=1 $(TESTFLAGS) ./test/envtest/...

# Renders the chart and compares it with config. It needs helm but no cluster.
test-chart:
	KUSTOMIZE="$(KUSTOMIZE)" $(GO) test -tags chart -race -count=1 $(TESTFLAGS) ./test/chart/...

# The host architecture, with no attestation manifest, so kind can load it.
image:
	docker buildx build --provenance=false --load -t $(CONTROLLER_IMAGE) -f $(CONTROLLER_DOCKERFILE) .

# Both architectures with no push. The commit's time stamps every layer, so
# the same commit gives the same image.
image-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 --provenance=false \
		--build-arg SOURCE_DATE_EPOCH="$$(git log -1 --format=%ct)" \
		--output type=image,push=false,rewrite-timestamp=true -f $(CONTROLLER_DOCKERFILE) .

kind-deps:
	hack/tests/kind/deps.sh

tidy:
	$(GO) mod tidy
	cd hack/tools && $(GO) mod tidy

verify: manifests generate chart
	@git diff --exit-code -- api config helm || { echo "generated files differ, commit the result of make manifests generate chart"; exit 1; }
	@test -z "$$(git ls-files --others --exclude-standard -- api config helm)" || { echo "generated files are untracked, commit the result of make manifests generate chart"; exit 1; }
