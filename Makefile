GO ?= go
# controller-gen renders RBAC from the kubebuilder markers. It runs through
# go run so that its own dependencies never enter go.mod.
CONTROLLER_GEN ?= $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
RBAC_ARGS = rbac:roleName=manager-role paths="./..."
# kustomize renders config/ the same way, so building and deploying need Go,
# docker and kubectl and nothing else.
KUSTOMIZE ?= $(GO) run sigs.k8s.io/kustomize/kustomize/v5@v5.8.1
KUBECTL ?= kubectl

# The image the docker and deploy targets use. The default names no
# registry, so pushing anywhere is a deliberate IMG=<registry>/<name>:<tag>.
IMG ?= nvsentinel-capi-remediator:dev
# The platforms docker-buildx builds. The Dockerfile cross-compiles, so
# neither needs emulation.
PLATFORMS ?= linux/amd64,linux/arm64
# PUSH=false, that exact word, leaves the multi-platform build in the
# builder's cache, which proves the Dockerfile for every platform without a
# registry. Anything else pushes.
PUSH ?= true

# Go packages in the module. Empty until the first package lands, in which
# case vet and test are no-ops instead of failing on "no packages".
PKGS = $(shell $(GO) list ./... 2>/dev/null)

.PHONY: all build fmt vet test manifests verify verify-fmt verify-mod verify-boilerplate verify-manifests verify-kustomize
.PHONY: docker-build docker-buildx build-installer deploy undeploy

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

# kustomize renders without complaint when the images transformer matches
# nothing, so both renders are checked for the image they should carry:
# the checked-in default, and the edit-and-render path that deploy and
# build-installer use.
verify-kustomize:
	@$(KUSTOMIZE) build config/default | grep -q 'image: ghcr.io/petasus-ai/nvsentinel-capi-remediator:latest' || \
	  { echo "config/default did not render the default image"; exit 1; }
	@$(call render,example.com/verify/manager:kustomize) | grep -q 'image: example.com/verify/manager:kustomize' || \
	  { echo "config/default did not render the image passed to kustomize"; exit 1; }

verify: verify-fmt verify-mod verify-boilerplate verify-manifests verify-kustomize vet test

docker-build:
	docker build -t $(IMG) .

docker-buildx:
	docker buildx build --platform $(PLATFORMS) -t $(IMG) $(if $(filter false,$(PUSH)),--output type=cacheonly,--push) .

# Renders config/default with the image $(1) in place of the default. The
# checked-in kustomization stays untouched: kustomize edits a copy.
define render
	tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT && \
	cp -R config "$$tmp/config" && \
	(cd "$$tmp/config/manager" && $(KUSTOMIZE) edit set image controller=$(1)) && \
	$(KUSTOMIZE) build "$$tmp/config/default"
endef

build-installer:
	@mkdir -p dist
	@$(call render,$(IMG)) > dist/install.yaml
	@echo "wrote dist/install.yaml with image $(IMG)"

deploy:
	@$(call render,$(IMG)) | $(KUBECTL) apply -f -

undeploy:
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found -f -
