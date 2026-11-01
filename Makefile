GO ?= go
TESTFLAGS ?=

# The build tools are pinned in hack/tools/go.mod and run through go tool.
GO_TOOL := $(GO) tool -modfile=hack/tools/go.mod
CONTROLLER_GEN := $(GO_TOOL) controller-gen
GOLANGCI_LINT := $(GO_TOOL) golangci-lint
SETUP_ENVTEST := $(GO_TOOL) setup-envtest

.DEFAULT_GOAL := help

.PHONY: help manifests generate fmt vet lint test tidy verify

help:
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

manifests:
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases

generate:
	$(CONTROLLER_GEN) object paths="./api/..."

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) run

test:
	$(GO) test -race -count=1 $(TESTFLAGS) ./...

tidy:
	$(GO) mod tidy
	cd hack/tools && $(GO) mod tidy

verify: manifests generate
	@git diff --exit-code -- api config || { echo "generated files differ, commit the result of make manifests generate"; exit 1; }
	@test -z "$$(git ls-files --others --exclude-standard -- api config)" || { echo "generated files are untracked, commit the result of make manifests generate"; exit 1; }
