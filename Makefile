GO ?= go
# controller-gen renders RBAC from the kubebuilder markers. It runs through
# go run so that its own dependencies never enter go.mod.
CONTROLLER_GEN ?= $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
RBAC_ARGS = rbac:roleName=manager-role paths="./..."

# Go packages in the module. Empty until the first package lands, in which
# case vet and test are no-ops instead of failing on "no packages".
PKGS = $(shell $(GO) list ./... 2>/dev/null)

.PHONY: all build fmt vet test manifests verify verify-fmt verify-mod verify-boilerplate verify-manifests

all: verify build

build:
	$(GO) build -o bin/manager ./cmd/manager

fmt:
	$(GO) fmt ./...

vet:
	@if [ -n "$(PKGS)" ]; then $(GO) vet $(PKGS); else echo "vet: no Go packages yet"; fi

test:
	@if [ -n "$(PKGS)" ]; then $(GO) test $(PKGS); else echo "test: no Go packages yet"; fi

verify-fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

verify-mod:
	$(GO) mod tidy -diff

verify-boilerplate:
	hack/verify-boilerplate.sh

manifests:
	$(CONTROLLER_GEN) $(RBAC_ARGS) output:rbac:artifacts:config=config/rbac

verify-manifests:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(CONTROLLER_GEN) $(RBAC_ARGS) output:rbac:artifacts:config="$$tmp" && \
	if ! diff -u config/rbac/role.yaml "$$tmp/role.yaml"; then echo "config/rbac/role.yaml is stale: run make manifests"; exit 1; fi

verify: verify-fmt verify-mod verify-boilerplate verify-manifests vet test
