GO ?= go
# controller-gen renders RBAC from the kubebuilder markers. It runs through
# go run so that its own dependencies never enter go.mod.
CONTROLLER_GEN ?= $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
RBAC_ARGS = rbac:roleName=manager-role paths="./..."
# kustomize renders config/ the same way, so building and deploying need Go,
# docker and kubectl and nothing else.
KUSTOMIZE ?= $(GO) run sigs.k8s.io/kustomize/kustomize/v5@v5.8.1
HELM ?= $(GO) run helm.sh/helm/v3/cmd/helm@v3.20.2
# setup-envtest downloads the API server and etcd the integration tests run
# against. It is released with controller-runtime, and pinned to the version
# of it the module requires.
ENVTEST ?= $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.24.1
ENVTEST_K8S_VERSION ?= 1.36.x
KUBECTL ?= kubectl

CHART = charts/nvsentinel-capi-remediator
# controller-gen writes the manager's role and the role of device quarantine
# into one manifest. The latter goes into the component that binds it, so
# that an install without device quarantine does not even create it.
QUARANTINE_ROLE = config/components/device-quarantine/role.yaml
# The roles controller-gen is expected to make, and so split.
RBAC_ROLES = device-quarantine-role manager-role
# The chart's roles take their rules from these files, which are cut from
# the roles of the manifests so that the two installs cannot drift apart.
CHART_RULES = $(CHART)/files/manager-role-rules.yaml
CHART_QUARANTINE_RULES = $(CHART)/files/device-quarantine-role-rules.yaml
CHART_LEADER_RULES = $(CHART)/files/leader-election-role-rules.yaml
# Prints the documents of a manifest that name $(1) at metadata depth, each
# after a separator, which is how controller-gen writes a manifest of one
# role. In a binding that also matches its roleRef and subjects.
DOC_OF = awk -v want=$(1) 'function flush() { if (d) printf "---\n%s", buf; buf = ""; d = 0 } \
  /^---$$/ { flush(); next } { buf = buf $$0 "\n" } /^  name: / && $$2 == want { d = 1 } END { flush() }'
# Prints the rules of the role named $(1) out of a manifest of roles: from its
# rules line to the end of its document.
RULES_OF = awk -v role=$(1) '/^---$$/{d=0; p=0; next} /^  name: /{d=($$2==role)} d&&/^rules:/{p=1} p'
# Fails unless the manifest $(1) holds exactly the roles of RBAC_ROLES. A role
# added with another roleName marker has to be split here too.
CHECK_ROLES = names="$$(awk '/^  name: /{print $$2}' $(1) | sort | xargs)" && \
  if [ "$$names" != "$(RBAC_ROLES)" ]; then echo "controller-gen made the roles $$names, not $(RBAC_ROLES): split them in the Makefile"; exit 1; fi
# A release tag: v<major>.<minor>.<patch> with an optional pre-release suffix
# and no build metadata, which an image tag cannot carry.
RELEASE_TAG = v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?

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
# The commit docker-buildx labels the image with, as
# org.opencontainers.image.revision. Empty leaves the label out.
REVISION ?=
# The commit verify-dco checks from: every commit after it must be signed off.
DCO_BASE ?= origin/main

# Go packages in the module, without the ones behind a build tag. Empty
# until the first package lands, in which case test is a no-op instead of
# failing on "no packages".
PKGS = $(shell $(GO) list ./... 2>/dev/null)

.PHONY: all build fmt vet test test-integration manifests verify verify-fmt verify-mod verify-boilerplate verify-dco verify-manifests verify-kustomize verify-chart
.PHONY: docker-build docker-buildx build-installer chart-package deploy undeploy

all: verify build

build:
	$(GO) build -o bin/manager ./cmd/manager

fmt:
	$(GO) fmt ./...

# The integration tests are behind a build tag, so vet is given it to see
# them, while test leaves them to test-integration.
vet:
	$(GO) vet -tags integration ./...

test:
	@if [ -n "$(PKGS)" ]; then $(GO) test $(PKGS); else echo "test: no Go packages yet"; fi

# The integration tests run the manager against two real API servers, a
# management and a workload one. A KUBEBUILDER_ASSETS already set is used as
# it is. The result is never taken from the test cache: it depends on those
# binaries and on timing, which the cache does not see.
test-integration:
	@assets="$${KUBEBUILDER_ASSETS:-$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)}" && \
	KUBEBUILDER_ASSETS="$$assets" $(GO) test -count=1 -tags integration ./test/integration/...

verify-fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

verify-mod:
	$(GO) mod tidy -diff

verify-boilerplate:
	hack/verify-boilerplate.sh

# Not part of verify: it checks a branch against DCO_BASE, which only makes
# sense before the branch is merged. Pull requests run it in the dco workflow.
verify-dco:
	hack/verify-dco.sh $(DCO_BASE) HEAD

manifests:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(CONTROLLER_GEN) $(RBAC_ARGS) output:rbac:artifacts:config="$$tmp" && \
	$(call CHECK_ROLES,"$$tmp/role.yaml") && \
	$(call DOC_OF,manager-role) "$$tmp/role.yaml" > config/rbac/role.yaml && \
	$(call DOC_OF,device-quarantine-role) "$$tmp/role.yaml" > $(QUARANTINE_ROLE)
	$(call RULES_OF,manager-role) config/rbac/role.yaml > $(CHART_RULES)
	$(call RULES_OF,device-quarantine-role) $(QUARANTINE_ROLE) > $(CHART_QUARANTINE_RULES)
	$(call RULES_OF,leader-election-role) config/rbac/leader_election_role.yaml > $(CHART_LEADER_RULES)

verify-manifests:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(CONTROLLER_GEN) $(RBAC_ARGS) output:rbac:artifacts:config="$$tmp" && \
	$(call CHECK_ROLES,"$$tmp/role.yaml") && \
	$(call DOC_OF,manager-role) "$$tmp/role.yaml" > "$$tmp/manager-role.yaml" && \
	if ! diff -u config/rbac/role.yaml "$$tmp/manager-role.yaml"; then echo "config/rbac/role.yaml is stale: run make manifests"; exit 1; fi && \
	$(call DOC_OF,device-quarantine-role) "$$tmp/role.yaml" > "$$tmp/quarantine-role.yaml" && \
	if ! diff -u $(QUARANTINE_ROLE) "$$tmp/quarantine-role.yaml"; then echo "$(QUARANTINE_ROLE) is stale: run make manifests"; exit 1; fi && \
	$(call RULES_OF,manager-role) "$$tmp/manager-role.yaml" > "$$tmp/rules.yaml" && \
	if ! diff -u $(CHART_RULES) "$$tmp/rules.yaml"; then echo "$(CHART_RULES) is stale: run make manifests"; exit 1; fi && \
	$(call RULES_OF,device-quarantine-role) "$$tmp/quarantine-role.yaml" > "$$tmp/quarantine-rules.yaml" && \
	if ! diff -u $(CHART_QUARANTINE_RULES) "$$tmp/quarantine-rules.yaml"; then echo "$(CHART_QUARANTINE_RULES) is stale: run make manifests"; exit 1; fi && \
	$(call RULES_OF,leader-election-role) config/rbac/leader_election_role.yaml > "$$tmp/leader-rules.yaml" && \
	if ! diff -u $(CHART_LEADER_RULES) "$$tmp/leader-rules.yaml"; then echo "$(CHART_LEADER_RULES) is stale: run make manifests"; exit 1; fi

# kustomize renders without complaint when the images transformer matches
# nothing, so both renders are checked for the image they should carry:
# the checked-in default, and the edit-and-render path that deploy and
# build-installer use. The default must carry nothing of device quarantine;
# an overlay adding its component must create its role under the prefixed
# name, and bind it to the prefixed ServiceAccount in the install namespace.
verify-kustomize:
	@out="$$($(KUSTOMIZE) build config/default)" && \
	printf '%s\n' "$$out" | grep -q 'image: quay.io/edgestack/nvsentinel-capi-remediator:latest' && \
	! printf '%s\n' "$$out" | grep -q -e 'device-quarantine' -e 'devicetaintrules' || \
	  { echo "config/default did not render the default image without device quarantine"; exit 1; }
	@tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT && \
	cp -R config "$$tmp/config" && mkdir "$$tmp/config/overlay" && \
	printf '%s\n' 'resources: [../default]' 'components: [../components/device-quarantine]' > "$$tmp/config/overlay/kustomization.yaml" && \
	out="$$($(KUSTOMIZE) build "$$tmp/config/overlay")" && \
	role="$$(printf '%s\n' "$$out" | $(call DOC_OF,nvsentinel-capi-remediator-device-quarantine-role) | awk '/^---$$/{n++} n==1')" && \
	printf '%s\n' "$$role" | grep -q '^kind: ClusterRole$$' && \
	printf '%s\n' "$$role" | grep -q -- '- devicetaintrules' && \
	printf '%s\n' "$$role" | grep -q 'app.kubernetes.io/name: nvsentinel-capi-remediator' && \
	binding="$$(printf '%s\n' "$$out" | $(call DOC_OF,nvsentinel-capi-remediator-device-quarantine-rolebinding))" && \
	printf '%s\n' "$$binding" | grep -q '^kind: ClusterRoleBinding$$' && \
	printf '%s\n' "$$binding" | grep -A3 '^roleRef:' | grep -q '^  name: nvsentinel-capi-remediator-device-quarantine-role$$' && \
	printf '%s\n' "$$binding" | grep -A3 '^subjects:' | grep -q '^  name: nvsentinel-capi-remediator-controller-manager$$' && \
	printf '%s\n' "$$binding" | grep -A3 '^subjects:' | grep -q '^  namespace: nvsentinel-capi-remediator-system$$' || \
	  { echo "the device-quarantine component did not create and bind its role under the prefixed names"; exit 1; }
	@$(call render,example.com/verify/manager:kustomize) | grep -q 'image: example.com/verify/manager:kustomize' || \
	  { echo "config/default did not render the image passed to kustomize"; exit 1; }

# The chart is linted and rendered: with its defaults, which must stay in
# dry-run, carry the rules of both roles and nothing of device quarantine;
# without a deviceQuarantine value at all, as with values reused from an
# earlier chart, which must render the same; with device quarantine on,
# which must add its arguments, ClusterRole and binding; with the settings
# that become arguments; and with device quarantine but no drivers, a
# restart fallback and a decision the manager would reject, each of which
# the chart must refuse by name. It is then
# packaged the way a release packages it, which must make the release's
# image the default.
verify-chart:
	@$(HELM) lint --quiet $(CHART)
	@out="$$($(HELM) template remediator $(CHART))" && \
	printf '%s\n' "$$out" | grep -q 'image: "quay.io/edgestack/nvsentinel-capi-remediator:v' && \
	printf '%s\n' "$$out" | grep -q -- '- --dry-run=true' && \
	printf '%s\n' "$$out" | grep -q -- '- --leader-elect' && \
	printf '%s\n' "$$out" | grep -q -- '- machinehealthchecks' && \
	printf '%s\n' "$$out" | grep -q -- '- leases' && \
	! printf '%s\n' "$$out" | grep -q -- '--decisions' && \
	! printf '%s\n' "$$out" | grep -q -- '--device-quarantine' && \
	! printf '%s\n' "$$out" | grep -q -- '- devicetaintrules' || \
	  { echo "$(CHART) did not render its default image, dry-run and role rules, without device quarantine"; exit 1; }
	@out="$$($(HELM) template remediator $(CHART) --set deviceQuarantine.enabled=true \
	  --set 'deviceQuarantine.drivers={gpu.example.com,nic.example.com}')" && \
	printf '%s\n' "$$out" | grep -q -- '- --device-quarantine' && \
	printf '%s\n' "$$out" | grep -q -- '- "--quarantine-drivers=gpu.example.com,nic.example.com"' && \
	role="$$(printf '%s\n' "$$out" | $(call DOC_OF,remediator-nvsentinel-capi-remediator-device-quarantine))" && \
	printf '%s\n' "$$role" | grep -q '^kind: ClusterRole$$' && \
	printf '%s\n' "$$role" | grep -q -- '- devicetaintrules' && \
	printf '%s\n' "$$role" | grep -q '^kind: ClusterRoleBinding$$' && \
	printf '%s\n' "$$role" | grep -A3 '^roleRef:' | grep -q '^  name: remediator-nvsentinel-capi-remediator-device-quarantine$$' || \
	  { echo "$(CHART) did not render device quarantine with its role and binding"; exit 1; }
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(HELM) template remediator $(CHART) > "$$tmp/defaults.yaml" && \
	$(HELM) template remediator $(CHART) --set deviceQuarantine=null > "$$tmp/without.yaml" && \
	if ! diff -u "$$tmp/defaults.yaml" "$$tmp/without.yaml"; then \
	  echo "$(CHART) did not render values without deviceQuarantine the way it renders its defaults"; exit 1; fi
	@if out="$$($(HELM) template remediator $(CHART) --set deviceQuarantine.enabled=true 2>&1)"; then \
	  echo "$(CHART) rendered device quarantine without drivers"; exit 1; \
	elif ! printf '%s\n' "$$out" | grep -q 'deviceQuarantine.drivers'; then \
	  echo "$(CHART) failed on device quarantine without drivers without naming them:"; printf '%s\n' "$$out"; exit 1; fi
	@out="$$($(HELM) template remediator $(CHART) --set image.repository=example.com/verify/manager \
	  --set image.tag=chart --set dryRun=false --set leaderElect=false --set restartFallback=replace \
	  --set clusterSelector=a=b --set pollInterval=30s --set metricsBindAddress=:8080 \
	  --set decisions.reset-fabric=report --set decisions.COMPONENT_RESET=restart \
	  --set 'extraArgs={--zap-log-level=debug}')" && \
	printf '%s\n' "$$out" | grep -q 'image: "example.com/verify/manager:chart"' && \
	printf '%s\n' "$$out" | grep -q -- '- --dry-run=false' && \
	printf '%s\n' "$$out" | grep -q -- '- --restart-fallback=replace' && \
	printf '%s\n' "$$out" | grep -q -- '- "--cluster-selector=a=b"' && \
	printf '%s\n' "$$out" | grep -q -- '- "--poll-interval=30s"' && \
	printf '%s\n' "$$out" | grep -q -- '- "--metrics-bind-address=:8080"' && \
	printf '%s\n' "$$out" | grep -q -- '- "--decisions=COMPONENT_RESET=restart,reset-fabric=report"' && \
	printf '%s\n' "$$out" | grep -q -- '- "--zap-log-level=debug"' && \
	! printf '%s\n' "$$out" | grep -q -- '- --leader-elect' || \
	  { echo "$(CHART) did not render the values passed to it"; exit 1; }
	@if out="$$($(HELM) template remediator $(CHART) --set restartFallback=reboot 2>&1)"; then \
	  echo "$(CHART) rendered an unknown restartFallback"; exit 1; \
	elif ! printf '%s\n' "$$out" | grep -q 'restartFallback'; then \
	  echo "$(CHART) failed on an unknown restartFallback without naming it:"; printf '%s\n' "$$out"; exit 1; fi
	@if out="$$($(HELM) template remediator $(CHART) --set pollInterval=2min 2>&1)"; then \
	  echo "$(CHART) rendered a pollInterval that is not a duration"; exit 1; \
	elif ! printf '%s\n' "$$out" | grep -q 'pollInterval'; then \
	  echo "$(CHART) failed on a pollInterval that is not a duration without naming it:"; printf '%s\n' "$$out"; exit 1; fi
	@$(HELM) template remediator $(CHART) --set pollInterval=1m30s | grep -q -- '- "--poll-interval=1m30s"' || \
	  { echo "$(CHART) did not render a pollInterval of two units"; exit 1; }
	@if out="$$($(HELM) template remediator $(CHART) --set decisions.COMPONENT_RESET=reboot 2>&1)"; then \
	  echo "$(CHART) rendered an unknown decision"; exit 1; \
	elif ! printf '%s\n' "$$out" | grep -q 'decisions'; then \
	  echo "$(CHART) failed on an unknown decision without naming it:"; printf '%s\n' "$$out"; exit 1; fi
	@if $(HELM) template remediator $(CHART) --set-string dryRun=false >/dev/null 2>&1; then \
	  echo "$(CHART) rendered a dryRun that is not a boolean"; exit 1; fi
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	$(HELM) package $(CHART) --version 0.0.0 --app-version v0.0.0 -d "$$tmp" >/dev/null && \
	$(HELM) template remediator "$$tmp/nvsentinel-capi-remediator-0.0.0.tgz" | \
	  grep -q 'image: "quay.io/edgestack/nvsentinel-capi-remediator:v0.0.0"' || \
	  { echo "the packaged $(CHART) did not default its image to the release"; exit 1; }

verify: verify-fmt verify-mod verify-boilerplate verify-manifests verify-kustomize verify-chart vet test test-integration

docker-build:
	docker build -t $(IMG) .

# No provenance or SBOM attestations: they are added to the image index as
# entries for an unknown platform, which tools that mirror an image
# platform by platform cannot copy.
docker-buildx:
	docker buildx build --platform $(PLATFORMS) --provenance=false --sbom=false -t $(IMG) \
	  $(if $(REVISION),--label org.opencontainers.image.revision=$(REVISION)) \
	  $(if $(filter false,$(PUSH)),--output type=cacheonly,--push) .

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

# Packages the chart for the release VERSION, a tag such as v0.1.0: the
# chart version is the tag without its v, and the appVersion, which the
# image tag defaults to, is the tag itself. The recipe reads VERSION from
# the environment, where make puts it, so that it is checked before it is
# part of a shell command line.
chart-package:
	@printf '%s\n' "$$VERSION" | grep -Eqx '$(RELEASE_TAG)' || \
	  { echo "VERSION must be a release tag such as v0.1.0 or v0.1.0-rc.1"; exit 1; }
	@mkdir -p dist
	$(HELM) package $(CHART) --version "$${VERSION#v}" --app-version "$$VERSION" -d dist

deploy:
	@$(call render,$(IMG)) | $(KUBECTL) apply -f -

undeploy:
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found -f -
