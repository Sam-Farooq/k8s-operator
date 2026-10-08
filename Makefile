# k8s-operator. Every target here runs without a cluster.

VERSION ?= 0.14.2

CONTROLLER_TOOLS_VERSION ?= v0.17.3
SETUP_ENVTEST_VERSION    ?= release-0.20
GOLANGCI_LINT_VERSION    ?= v2.14.0
ENVTEST_K8S_VERSION      ?= 1.32.0

# The files controller-gen owns. verify-manifests diffs exactly these, not all
# of api/ and config/, so an uncommitted hand-written change does not look
# like stale generator output.
GENERATED := api/v1alpha1/zz_generated.deepcopy.go config/crd/bases config/rbac

# Relative on purpose. make has no way to escape a space in a target or a
# prerequisite, so an absolute $(CURDIR)/bin breaks the moment the checkout path
# contains one: make reports "Circular ... dependency dropped" for each path
# segment and the recipe runs with the path split into words. A GitHub runner
# checks out to /home/runner/work/<repo>/<repo>, so CI never sees it.
LOCALBIN       := bin
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
SETUP_ENVTEST  := $(LOCALBIN)/setup-envtest
GOLANGCI_LINT  := $(LOCALBIN)/golangci-lint

# go install needs an absolute GOBIN, and setup-envtest --bin-dir has to be
# absolute too: it echoes back the path it was given, and go test runs each
# package in that package's own directory, so a relative KUBEBUILDER_ASSETS
# resolves differently per package and envtest dies on fork/exec of etcd.
# Quoted at every use, because this path has spaces in it here.
BIN_ABS := $(CURDIR)/bin

export PATH := $(BIN_ABS):$(PATH)

.PHONY: help
help: ## List the targets.
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN{FS=":.*?## "}{printf "  %-16s %s\n", $$1, $$2}'

$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

.PHONY: tools
tools: $(CONTROLLER_GEN) $(SETUP_ENVTEST) ## Fetch controller-gen and setup-envtest into ./bin.

$(CONTROLLER_GEN): | $(LOCALBIN)
	GOBIN="$(BIN_ABS)" go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(SETUP_ENVTEST): | $(LOCALBIN)
	GOBIN="$(BIN_ABS)" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

$(GOLANGCI_LINT): | $(LOCALBIN)
	GOBIN="$(BIN_ABS)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate deepcopy functions.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate the CRD and the RBAC role.
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=false rbac:roleName=postgresbackup-manager \
		paths=./api/... paths=./internal/... \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: verify-manifests
verify-manifests: manifests generate ## Fail if the committed generated files are stale.
	git diff --exit-code -- $(GENERATED)

.PHONY: fmt
fmt: ## Rewrite files with gofmt.
	gofmt -w $(shell git ls-files '*.go')

.PHONY: fmt-check
fmt-check: ## Fail if any file needs gofmt.
	@out=$$(gofmt -l $(shell git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: vet
vet: ## go vet.
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: envtest
envtest: $(SETUP_ENVTEST) ## Download the envtest control plane binaries.
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_ABS)" -p path

.PHONY: test
test: envtest ## Run every test, control plane included.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_ABS)" -p path)" \
		go test ./... -count=1

.PHONY: test-race
test-race: envtest ## Run every test under the race detector.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(BIN_ABS)" -p path)" \
		go test ./... -count=1 -race

.PHONY: build
build: ## Build the manager into ./bin.
	go build -ldflags "-X main.operatorVersion=$(VERSION)" -o "$(LOCALBIN)/manager" ./cmd

.PHONY: check
check: fmt-check vet lint verify-manifests test ## What CI runs.

.PHONY: clean
clean: ## Remove ./bin.
	rm -rf "$(LOCALBIN)"
