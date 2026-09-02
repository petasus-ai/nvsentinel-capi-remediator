GO ?= go

# Go packages in the module. Empty until the first package lands, in which
# case vet and test are no-ops instead of failing on "no packages".
PKGS = $(shell $(GO) list ./... 2>/dev/null)

.PHONY: all fmt vet test verify verify-fmt verify-boilerplate

all: verify

fmt:
	$(GO) fmt ./...

vet:
	@if [ -n "$(PKGS)" ]; then $(GO) vet $(PKGS); else echo "vet: no Go packages yet"; fi

test:
	@if [ -n "$(PKGS)" ]; then $(GO) test $(PKGS); else echo "test: no Go packages yet"; fi

verify-fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

verify-boilerplate:
	hack/verify-boilerplate.sh

verify: verify-fmt verify-boilerplate vet test
