# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set).
# Guarded with `command -v go` so host-only targets (hooks, cluster-up, ...) do
# not surface a "make: go: No such file or directory" error: go lives inside the
# devtools container (see scripts/dev.sh), not on PATH.
ifeq (,$(shell command -v go >/dev/null 2>&1 && go env GOBIN))
GOBIN=$(shell command -v go >/dev/null 2>&1 && go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# --- execution-context routing (explicit opt-in; NO magic-by-prefix) -------
# container_target re-runs a PRIVATE target ($1, conventionally _name) inside
# the devtools container, unless we are already inside it. Each public target
# that needs the Go/helm toolchain calls this explicitly, so `make help` stays
# honest and a future host-only target is never auto-wrapped by accident.
#
# $(MAKEOVERRIDES) forwards the caller's command-line variable assignments
# (e.g. PKG=… FOCUS=… RUN=… TIMEOUT=… BASE_REF=…). It is REQUIRED on the
# dev.sh path: scripts/dev.sh only forwards an explicit -e allowlist into the
# container, so MAKEFLAGS (which normally carries command-line overrides to a
# sub-make) does NOT cross the docker boundary. Without this, arg-taking
# wrappers like envtest-pkg/e2e-focus would see empty $(PKG)/$(RUN).
LITELLM_IN_DEVTOOLS ?= 0
define container_target
	@if [ "$(LITELLM_IN_DEVTOOLS)" = "1" ]; then \
		$(MAKE) --no-print-directory $(1) $(foreach o,$(MAKEOVERRIDES),'$o'); \
	else \
		./scripts/dev.sh $(MAKE) --no-print-directory $(1) $(foreach o,$(MAKEOVERRIDES),'$o'); \
	fi
endef
# Each command-line override is single-quoted ($(foreach …,'$o')) so a value
# containing shell metacharacters — notably FOCUS='TestA|TestB' (regex
# alternation) — survives the dev.sh / sub-make hop as ONE argument instead
# of being split into a shell pipe.

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build-operator

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Diagnostics

.PHONY: doctor
doctor: ## Fast local preflight: docker, devtools image, socket, cache paths, in-container tools, kubeconfig (if present). No network.
	@echo "== alitellm-operator doctor (fast) =="
	@docker info >/dev/null 2>&1 && echo "OK   docker daemon reachable" || { echo "FAIL docker daemon unreachable"; exit 1; }
	@test -S /var/run/docker.sock && echo "OK   /var/run/docker.sock present" || echo "WARN /var/run/docker.sock not a socket on host"
	@docker image inspect litellm-devtools:latest >/dev/null 2>&1 && echo "OK   litellm-devtools:latest present" || echo "WARN litellm-devtools:latest absent (built on first ./scripts/dev.sh use)"
	@for d in .gocache/gopath .gocache/build .gocache/envtest .gocache/kube; do test -d "$$d" && echo "OK   $$d" || echo "WARN $$d missing (created on first dev.sh run)"; done
	@./scripts/dev.sh bash -c '\
	  for t in go helm kind kubectl controller-gen setup-envtest kustomize; do \
	    command -v $$t >/dev/null 2>&1 && echo "OK   (container) $$t" || echo "FAIL (container) $$t MISSING"; \
	  done; \
	  for t in golangci-lint; do \
	    if command -v $$t >/dev/null 2>&1 || test -x /workspace/bin/$$t; then \
	      echo "OK   (container) $$t (baked or installed-on-demand)"; \
	    else \
	      echo "INFO (container) $$t not yet installed (go-installed on first lint into ./bin)"; \
	    fi; \
	  done'
	@test -f .gocache/kube/config && echo "OK   kubeconfig present (.gocache/kube/config)" || echo "INFO no kubeconfig yet (run make cluster-up)"

.PHONY: shell
shell: ## Interactive shell inside the devtools container.
	./scripts/dev.sh bash

##@ Development

# NOTE: paths is scoped to ./api/... and ./internal/... (NOT the kubebuilder
# default ./...) because the repo also hosts a separate Go module under
# verification/ for the Phase 0 spike (plan 01-01). With paths="./...",
# controller-gen descends into verification/ and fails on its read-only
# Go module cache. ./internal/... was added in plan 01-04 so the
# NoOpReconciler's RBAC markers (+kubebuilder:rbac:...) are picked up.
.PHONY: gen-manifests
gen-manifests: ## Generate WebhookConfiguration, Role and CustomResourceDefinition objects.
	$(call container_target,_gen-manifests)
_gen-manifests: controller-gen
	# crd:allowDangerousTypes=true is required because Team.spec.budget.limit
	# is *float64 (per spec §6.7 "Float64 precision is adopted for v1alpha1").
	# controller-gen rejects float types by default; the spec explicitly chose
	# this contract, so the flag is the documented kubebuilder escape hatch.
	#
	# rbac:roleName= passes the legacy name; the post-rewrite below
	# normalizes the file to (a) kind: Role, (b) name: alitellm-operator-role,
	# (c) inject metadata.namespace: system. controller-gen has no flag to
	# emit a namespaced Role directly (see issue #21 plan, Task 3).
	$(CONTROLLER_GEN) rbac:roleName=alitellm-operator-manager-role crd:allowDangerousTypes=true webhook paths="./api/..." paths="./internal/..." output:crd:artifacts:config=config/crd/bases
	@scripts/normalize-manager-role.sh

.PHONY: gen-code
gen-code: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(call container_target,_gen-code)
_gen-code: controller-gen
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./api/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: qa-fmt-check
qa-fmt-check: ## Fail if any Go file is not gofmt-clean (no mutation).
	$(call container_target,_qa-fmt-check)
_qa-fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go' | grep -v -E 'zz_generated|/vendor/')); \
	if [ -n "$$out" ]; then echo "Not gofmt-clean:"; echo "$$out"; exit 1; fi; \
	echo "OK gofmt-clean"

##@ Test

.PHONY: test-unit
test-unit: ## Phase 1 — pure-logic tests, no envtest, no cluster. ~10s warm.
	$(call container_target,_test-unit)
_test-unit: fmt vet
	# `go test` defaults to -p=GOMAXPROCS across packages (speedup-ideas §5 confirmed).
	# Exclusions: internal/controller (envtest), internal/toolhive (envtest),
	# test/e2e (cluster). Anything else is pure-logic.
	go test -v -race -shuffle=on -count=1 \
		$$(go list ./... | grep -v -E "/internal/(controller|toolhive)|/test/e2e") \
		-coverprofile cover-unit.out

.PHONY: test-envtest
test-envtest: test-envtest-race ## Phase 2 — controller envtest (race-enabled). Alias for test-envtest-race; CI gate.

.PHONY: test-envtest-race
test-envtest-race: ## Phase 2 — controller envtest with -race. Slower (~7m) but catches data races. CI gate.
	$(call container_target,_test-envtest-race)
_test-envtest-race: gen-manifests gen-code fmt vet setup-envtest
	@# Runs envtest packages concurrently. Green runs show package status
	@# plus slow tests; failed packages dump their captured logs.
	@KUBEBUILDER_ASSETS="$$($(envtest_assets))"; \
	export KUBEBUILDER_ASSETS; \
	scripts/run-envtest-packages.sh --race --timeout 15m --coverprofile cover-envtest.out -- ./internal/controller/... ./internal/toolhive/...

.PHONY: test-envtest-fast
test-envtest-fast: ## Phase 2 — controller envtest WITHOUT -race. Dev inner loop (~3m, ~3x faster than test-envtest-race). Not a CI gate.
	$(call container_target,_test-envtest-fast)
_test-envtest-fast: setup-envtest
	@# Runs envtest packages concurrently. Green runs show package status
	@# plus slow tests; failed packages dump their captured logs.
	@KUBEBUILDER_ASSETS="$$($(envtest_assets))"; \
	export KUBEBUILDER_ASSETS; \
	scripts/run-envtest-packages.sh --timeout 10m -- ./internal/controller/... ./internal/toolhive/...

.PHONY: test-full
test-full: test-unit test-envtest ## All non-cluster tests (test-unit + test-envtest).

.PHONY: test-unit-pkg
test-unit-pkg: ## Phase 1 — run unit tests for one package. Usage: make test-unit-pkg PKG=./internal/litellm/...
	$(call container_target,_test-unit-pkg)
_test-unit-pkg:
	@test -n "$(PKG)" || (echo "ERROR: PKG=... required" >&2; exit 1)
	go test -v -race -count=1 $(PKG)

.PHONY: test-envtest-pkg
test-envtest-pkg: ## Phase 2 — run envtest for one package. Usage: make test-envtest-pkg PKG=./internal/controller/... [FOCUS=TestName] [TIMEOUT=10m]
	$(call container_target,_test-envtest-pkg)
_test-envtest-pkg: setup-envtest
	@test -n "$(PKG)" || (echo "ERROR: PKG=... required" >&2; exit 1)
	# `script -q /dev/null -c "..."` fakes a TTY so -v output streams.
	# FOCUS is single-quoted so a `-run` regex containing `|` (alternation,
	# e.g. FOCUS='TestA|TestB') is passed as ONE argument instead of being
	# split into a shell pipe by the inner `-c` shell.
	KUBEBUILDER_ASSETS="$(shell $(envtest_assets))" \
		script -q /dev/null -c "go test -v -count=1 -timeout $(or $(TIMEOUT),10m) $(if $(FOCUS),-run '$(FOCUS)',) $(PKG)"

.PHONY: test-smoke-idempotency
test-smoke-idempotency: ## Run the accelerated AC-R1 idempotency smoke (10s window, 1s safety re-list).
	$(call container_target,_test-smoke-idempotency)
_test-smoke-idempotency: gen-manifests gen-code fmt vet setup-envtest
	KUBEBUILDER_ASSETS="$(shell $(envtest_assets))" go test -count=1 -timeout 60s -run TestIdempotencyNoMutationSteadyState ./internal/controller/...

.PHONY: test-smoke-idempotency-long
test-smoke-idempotency-long: ## Run the real 35-min AC-R1 idempotency test (nightly cadence; longidempotency build tag).
	$(call container_target,_test-smoke-idempotency-long)
_test-smoke-idempotency-long: gen-manifests gen-code fmt vet setup-envtest
	KUBEBUILDER_ASSETS="$(shell $(envtest_assets))" go test -count=1 -timeout 40m -tags=longidempotency -run TestIdempotency35MinReal ./internal/controller/...

.PHONY: test-leak-soak
test-leak-soak: ## REL-03: run the 1000-reconcile leak harness (nightly cadence; longidempotency build tag).
	$(call container_target,_test-leak-soak)
_test-leak-soak: gen-manifests gen-code fmt vet setup-envtest
	KUBEBUILDER_ASSETS="$(shell $(envtest_assets))" go test -count=1 -timeout 5m -tags=longidempotency -run TestLeakHarness_1000Reconciles ./internal/controller/...

##@ QA

.PHONY: qa-lint
qa-lint: ## Run golangci-lint linter
	$(call container_target,_qa-lint)
_qa-lint: golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: qa-lint-fix
qa-lint-fix: ## Run golangci-lint linter and perform fixes
	$(call container_target,_qa-lint-fix)
_qa-lint-fix: golangci-lint
	$(GOLANGCI_LINT) run --fix

.PHONY: qa-lint-config
qa-lint-config: ## Verify golangci-lint linter configuration
	$(call container_target,_qa-lint-config)
_qa-lint-config: golangci-lint
	$(GOLANGCI_LINT) config verify

.PHONY: qa-lint-changed
qa-lint-changed: ## Lint only packages touched vs BASE_REF (default origin/main, fallback main). Inner-loop fast path (speedup-ideas §10).
	$(call container_target,_qa-lint-changed)
_qa-lint-changed: golangci-lint
	@BASE=$${BASE_REF:-origin/main}; \
	if ! git rev-parse --verify "$$BASE" >/dev/null 2>&1; then \
		BASE=main; \
		git rev-parse --verify "$$BASE" >/dev/null 2>&1 || { \
			echo "ERROR: neither origin/main nor main exists; pass BASE_REF=<ref>" >&2; exit 1; }; \
	fi; \
	CHANGED=$$(git diff --name-only "$$BASE...HEAD" -- '*.go' \
		| xargs -r -n1 dirname | sort -u | sed 's|^|./|; s|$$|/...|'); \
	BUILDABLE=""; \
	for p in $$CHANGED; do \
		if [ -n "$$(go list $$p 2>/dev/null)" ]; then BUILDABLE="$$BUILDABLE $$p"; fi; \
	done; \
	if [ -z "$$BUILDABLE" ]; then \
		echo "No buildable Go packages changed vs $$BASE (build-tag-only dirs like test/e2e are skipped)"; \
	else \
		echo "Linting (vs $$BASE):$$BUILDABLE"; \
		$(GOLANGCI_LINT) run $$BUILDABLE; \
	fi

##@ Security

FUZZ_TIME_SHORT ?= 60s
FUZZ_TIME_LONG  ?= 10m

.PHONY: qa-fuzz-short
qa-fuzz-short: ## Phase 4 — Go fuzz targets with 60s budget per target (CI cadence).
	$(call container_target,_qa-fuzz-short)
_qa-fuzz-short:
	go test -run='^$$' -fuzz=FuzzSubstitute -fuzztime=$(FUZZ_TIME_SHORT) ./internal/substitution/...
	go test -run='^$$' -fuzz=FuzzNormalize  -fuzztime=$(FUZZ_TIME_SHORT) ./internal/normalize/...

.PHONY: qa-fuzz-long
qa-fuzz-long: ## Go fuzz targets with 10-minute budget per target (nightly cadence).
	$(call container_target,_qa-fuzz-long)
_qa-fuzz-long:
	go test -run='^$$' -fuzz=FuzzSubstitute -fuzztime=$(FUZZ_TIME_LONG) ./internal/substitution/...
	go test -run='^$$' -fuzz=FuzzNormalize  -fuzztime=$(FUZZ_TIME_LONG) ./internal/normalize/...

.PHONY: qa-security
qa-security: ## Phase 4 — in-container security umbrella: gosec (via lint) + govulncheck (acknowledged-list aware) + qa-fuzz-short. Target <=6min warm. Runs inside devtools (./scripts/dev.sh make qa-security).
	$(call container_target,_qa-security)
_qa-security: qa-lint
	bash scripts/govulncheck-gate.sh
	$(MAKE) qa-fuzz-short

.PHONY: pre-push
pre-push: ## Host-only — 17-gate pre-publication check (gitleaks + trufflehog + qa-lint + test-unit + SPDX + govulncheck + ...). Uses docker on host; do NOT call via ./scripts/dev.sh.
	./scripts/pre-push-check.sh

.PHONY: verify
verify: ## Host-only — full pre-publication gate bundle: qa-lint + test-unit + in-container qa-security + host pre-push. Single command for all gates.
	$(MAKE) qa-lint
	$(MAKE) test-unit
	$(MAKE) qa-security
	$(MAKE) pre-push

.PHONY: hooks
hooks: ## Install git hooks (pre-push).
	./scripts/install-hooks.sh

##@ Release

.PHONY: release-bump
release-bump: ## Internal: bump version across all manifests. Used by release.yml. Prefer `make release-cut VERSION=X.Y.Z` for local cuts.
	@test -n "$(VERSION)" || (echo "ERROR: VERSION=X.Y.Z required (no leading 'v')" >&2; exit 1)
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$' || \
		(echo "ERROR: VERSION must be semver without leading 'v' (e.g. 0.0.3 or 0.1.0-rc1)" >&2; exit 1)
	@echo "Bumping all manifests to v$(VERSION)..."
	@sed -i -E 's/^version: .*/version: $(VERSION)/' deploy/helm/alitellm-operator/Chart.yaml
	@sed -i -E 's/^appVersion: .*/appVersion: v$(VERSION)/' deploy/helm/alitellm-operator/Chart.yaml
	@sed -i -E 's|^([[:space:]]+)tag: v.*|\1tag: v$(VERSION)|' deploy/helm/alitellm-operator/values.yaml
	@sed -i -E 's|^([[:space:]]+)newTag: v.*|\1newTag: v$(VERSION)|' config/manager/kustomization.yaml
	@sed -i -E 's|^([[:space:]]+)newTag: v.*|\1newTag: v$(VERSION)|' deploy/kustomize/kustomization.yaml
	@echo "Manifests bumped to v$(VERSION)."

.PHONY: release-cut
release-cut: ## Cut a release: empty `chore(release): vX.Y.Z` commit, run pre-push, push to main. Usage: make release-cut VERSION=X.Y.Z
	@test -n "$(VERSION)" || (echo "ERROR: VERSION=X.Y.Z required (no leading 'v')" >&2; exit 1)
	@echo "$(VERSION)" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$' || \
		(echo "ERROR: VERSION must be semver without leading 'v' (e.g. 0.0.3 or 0.1.0-rc1)" >&2; exit 1)
	@branch=$$(git rev-parse --abbrev-ref HEAD); \
	test "$$branch" = "main" || (echo "ERROR: must be on main (current: $$branch)" >&2; exit 1)
	@git diff --quiet || (echo "ERROR: working tree dirty; commit or stash first" >&2; exit 1)
	@git diff --cached --quiet || (echo "ERROR: index has staged changes; commit or reset first" >&2; exit 1)
	@git fetch origin main --quiet
	@local=$$(git rev-parse HEAD); remote=$$(git rev-parse origin/main); \
	test "$$local" = "$$remote" || (echo "ERROR: local main differs from origin/main; rebase or pull first" >&2; exit 1)
	git commit --allow-empty -m "chore(release): v$(VERSION)"
	$(MAKE) pre-push
	git push origin main
	@echo ""
	@echo "release.yml is now running. Watch with:"
	@echo "  gh run watch \$$(gh run list --branch main --limit 1 --json databaseId --jq '.[0].databaseId')"

##@ Build

.PHONY: build-operator
build-operator: ## Build alitellm-operator binary.
	$(call container_target,_build-operator)
_build-operator: gen-manifests gen-code fmt vet
	go build -o bin/alitellm-operator cmd/main.go

.PHONY: run
run: ## Run a controller from your host.
	$(call container_target,_run)
_run: gen-manifests gen-code fmt vet
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: build-image
build-image: ## Build docker image with the alitellm-operator binary.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-load
docker-load: ## Load IMG into the kind cluster (no push). Usage: make docker-load IMG=alitellm-operator:e2e
	kind load docker-image $(IMG) --name $${KIND_CLUSTER:-alitellm-operator-test}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name workspace-builder
	$(CONTAINER_TOOL) buildx use workspace-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm workspace-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: ## Generate a consolidated YAML with CRDs and deployment.
	$(call container_target,_build-installer)
_build-installer: gen-manifests gen-code kustomize
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(call container_target,_install)
_install: gen-manifests kustomize
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(call container_target,_uninstall)
_uninstall: gen-manifests kustomize
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(call container_target,_deploy)
_deploy: gen-manifests kustomize
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(call container_target,_undeploy)
_undeploy: kustomize
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

# Tool-installer targets (kustomize, controller-gen, setup-envtest, envtest,
# golangci-lint, crd-ref-docs) run `go install` via the go-install-tool macro.
# They are NOT wrapped in container_target: they are reached only as
# prerequisites of already-routed `_`-targets, so they execute in-container
# where `go` exists. Invoking one standalone on the host (e.g. `make
# setup-envtest`) is an unsupported path — go is absent on the host PATH.

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs

## Tool Versions
KUSTOMIZE_VERSION ?= v5.5.0
CONTROLLER_TOOLS_VERSION ?= v0.17.0
# ENVTEST_VERSION + ENVTEST_K8S_VERSION are derived from go.mod by shelling
# out to `go list`. The host has no `go` on PATH (see scripts/dev.sh), so
# we guard each call with `command -v go` and fall back to an empty value
# on the host. Targets that actually need these vars run via dev.sh, where
# go IS available — make then re-evaluates the Makefile inside the
# container and the derivation succeeds. This avoids the cosmetic
# `make: go: No such file or directory` noise on host-only targets such
# as `make hooks`, `make cluster-up`, etc.
#
# (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell command -v go >/dev/null 2>&1 && go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
# (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell command -v go >/dev/null 2>&1 && go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')

# envtest asset resolution. ENVTEST_ASSET_DIR is the PRIMARY store, probed
# first with -i (installed-only, NO network). In the devtools image it is
# /opt/envtest — Dockerfile.devtools bakes the k8s assets there and sets
# ENVTEST_BIN_DIR=/opt/envtest, which scripts/dev.sh propagates into the
# container env; make picks it up as a variable. On a miss (version skew, or
# a non-container path where ENVTEST_BIN_DIR is unset) it falls back to a
# download into the writable $(LOCALBIN). $(envtest_assets) is a shell command
# STRING (not a result), so it composes in both recipe-runtime `$$(...)` and
# make-time `$(shell ...)` call sites.
ENVTEST_ASSET_DIR ?= $(or $(ENVTEST_BIN_DIR),$(LOCALBIN))
envtest_assets = $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSET_DIR) -i -p path 2>/dev/null || $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

GOLANGCI_LINT_VERSION ?= v1.62.2
CRD_REF_DOCS_VERSION ?= v0.2.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(envtest_assets) || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary (renders docs/api-reference from CRD Go types).
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

##@ Packaging & Sync
include docs/Makefile

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

# --- helm / chart packaging ---

.PHONY: helm-sync
helm-sync: ## Plan 07-02: regenerate deploy/helm/alitellm-operator/templates/install.yaml from dist/install.yaml per D-01 (kustomize canonical, Helm veneer).
	$(call container_target,_helm-sync)
_helm-sync: build-installer
	bash scripts/kustomize-to-helm.sh dist/install.yaml deploy/helm/alitellm-operator/templates/install.yaml
	# CRDs land in crd-sources/ (NOT the reserved crds/ dir name) so the
	# templates/crds.yaml loop can range over them and emit each one as a
	# Helm-managed template. helm-inject-crd-annotation.py adds
	# `helm.sh/resource-policy: keep` so `helm uninstall` preserves
	# CRDs and the user's CR data.
	cp -f config/crd/bases/litellm.ackstorm.ai_*.yaml deploy/helm/alitellm-operator/crd-sources/
	python3 scripts/helm-inject-crd-annotation.py deploy/helm/alitellm-operator/crd-sources/*.yaml

.PHONY: helm-sync-check
helm-sync-check: ## CI gate: fail if `make helm-sync` produced uncommitted diff (drift between kustomize and chart).
	$(call container_target,_helm-sync-check)
_helm-sync-check: helm-sync
	@if ! git diff --quiet deploy/helm/alitellm-operator/; then \
	  echo "CHART DRIFT: deploy/helm/alitellm-operator/ is out of sync with kustomize. Run \`make helm-sync\` and commit."; \
	  git diff deploy/helm/alitellm-operator/; \
	  exit 1; \
	fi

# --- deploy/kustomize snapshot ---

.PHONY: deploy-kustomize-sync
deploy-kustomize-sync: ## Regenerate deploy/kustomize/manager-rbac.yaml from config/rbac/ (operator-runtime + metrics-auth subset).
	$(call container_target,_deploy-kustomize-sync)
_deploy-kustomize-sync:
	bash scripts/render-deploy-kustomize-rbac.sh

.PHONY: deploy-kustomize-sync-check
deploy-kustomize-sync-check: ## CI gate: fail if `make deploy-kustomize-sync` produced uncommitted diff (drift between config/rbac/ and the bundled snapshot).
	$(call container_target,_deploy-kustomize-sync-check)
_deploy-kustomize-sync-check: deploy-kustomize-sync
	@if ! git diff --quiet deploy/kustomize/manager-rbac.yaml; then \
	  echo "DEPLOY KUSTOMIZE DRIFT: deploy/kustomize/manager-rbac.yaml is out of sync with config/rbac/. Run \`make deploy-kustomize-sync\` and commit."; \
	  git diff deploy/kustomize/manager-rbac.yaml; \
	  exit 1; \
	fi

# ac-n3-audit and samples-audit are pure `grep` (no go/helm/kustomize): they
# are host-safe and intentionally left UNROUTED to avoid a container spin-up.
.PHONY: ac-n3-audit
ac-n3-audit: ## SCOPE-03 / AC-N3 static gate: fail if any non-test .go file references /user/ or /key/ as string literals.
	@hits=$$(grep -RnE '"/user/|"/key/' --include='*.go' --exclude='*_test.go' internal/ cmd/ 2>/dev/null \
	  | grep -v '^\s*//' || true); \
	if [ -n "$$hits" ]; then \
	  echo "AC-N3 VIOLATION: forbidden path-prefix string literals found:"; \
	  echo "$$hits"; \
	  exit 1; \
	fi; \
	echo "ac-n3-audit: PASS (zero /user/* or /key/* literals in non-test source)"

# --- samples-audit ---

.PHONY: samples-audit
samples-audit: ## DEPLOY-02: fail the build if any sample manifest contains a TODO(user) placeholder (per plan 07-03 audit gate).
	@hits=$$(grep -RIE 'TODO\(user\)' config/samples/ 2>/dev/null || true); \
	if [ -n "$$hits" ]; then \
	  echo "DEPLOY-02 VIOLATION: TODO(user) placeholders found in samples:"; \
	  echo "$$hits"; \
	  exit 1; \
	fi; \
	echo "samples-audit: PASS (zero TODO(user) placeholders in config/samples/)"

##@ Cluster (e2e infra)

.PHONY: build-image-mock
build-image-mock: ## build the litellm-mock:e2e image
	$(CONTAINER_TOOL) build -t litellm-mock:e2e -f test/e2e/mock/Dockerfile test/e2e/mock/

# --- inotify preflight (host-only) -----------------------------------------
# kind runs each Kubernetes node as a docker container; kubelet, containerd,
# and the API server each consume fs.inotify instances. The common distro
# default (max_user_instances=128) gets exhausted partway through hydration
# and the API server crashes with "connection refused" mid-bringup. These are
# HOST kernel knobs (not namespaced), so they must be raised on the host
# BEFORE cluster-up routes into the devtools container — hence a plain
# prerequisite, not a container_target.
INOTIFY_MIN_INSTANCES ?= 512
INOTIFY_MIN_WATCHES   ?= 524288

.PHONY: ensure-inotify
ensure-inotify: ## Host-only: raise fs.inotify limits if below kind's needs (best-effort, non-fatal).
	@if [ "$(LITELLM_IN_DEVTOOLS)" = "1" ]; then exit 0; fi; \
	cur_i=$$(cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || echo 0); \
	cur_w=$$(cat /proc/sys/fs/inotify/max_user_watches 2>/dev/null || echo 0); \
	if [ "$$cur_i" -ge "$(INOTIFY_MIN_INSTANCES)" ] && [ "$$cur_w" -ge "$(INOTIFY_MIN_WATCHES)" ]; then \
	  echo "OK   inotify limits sufficient (instances=$$cur_i watches=$$cur_w)"; \
	else \
	  echo "INFO raising inotify limits for kind (instances=$$cur_i->$(INOTIFY_MIN_INSTANCES), watches=$$cur_w->$(INOTIFY_MIN_WATCHES))"; \
	  if sudo -n sysctl -w fs.inotify.max_user_instances=$(INOTIFY_MIN_INSTANCES) fs.inotify.max_user_watches=$(INOTIFY_MIN_WATCHES) >/dev/null 2>&1; then \
	    echo "OK   inotify limits raised (live only; add an /etc/sysctl.d drop-in to persist across reboots)"; \
	  else \
	    echo "WARN could not raise inotify limits (no passwordless sudo). kind may die mid-bringup with 'connection refused'."; \
	    echo "WARN raise them manually, then re-run:"; \
	    echo "       sudo sysctl -w fs.inotify.max_user_instances=$(INOTIFY_MIN_INSTANCES) fs.inotify.max_user_watches=$(INOTIFY_MIN_WATCHES)"; \
	  fi; \
	fi

.PHONY: cluster-up cluster-down cluster-hydrate cluster-sync cluster-keep cluster-status cluster-verify
cluster-up: ensure-inotify ## bring up canonical kind cluster + hydration
	$(call container_target,_cluster-up)
_cluster-up:
	bash scripts/cluster.sh up
cluster-down:    ## tear down canonical kind cluster
	$(call container_target,_cluster-down)
_cluster-down:
	bash scripts/cluster.sh down
cluster-hydrate: ## re-apply hydration on an already-up cluster
	$(call container_target,_cluster-hydrate)
_cluster-hydrate:
	bash scripts/cluster.sh hydrate
cluster-keep:    ## same as cluster-up (kept for naming consistency with spec §5)
	$(call container_target,_cluster-keep)
_cluster-keep:
	bash scripts/cluster.sh keep
cluster-status:  ## print kubectl get on hydration fixtures
	$(call container_target,_cluster-status)
_cluster-status:
	bash scripts/cluster.sh status
cluster-sync:    ## re-apply phases on a running cluster (alias of cluster-hydrate; parity with ../ach)
	$(call container_target,_cluster-sync)
_cluster-sync:
	bash scripts/cluster.sh sync
cluster-verify:  ## health-gate the standing state on a running cluster (no mutation)
	$(call container_target,_cluster-verify)
_cluster-verify:
	bash scripts/cluster.sh verify

.PHONY: cluster-reset
cluster-reset: ## tear down then bring up a clean cluster
	$(MAKE) cluster-down
	$(MAKE) cluster-up

.PHONY: cluster-image-load
cluster-image-load: ## Build + kind-load the operator image. Usage: make cluster-image-load IMG=alitellm-operator:e2e
	$(call container_target,_cluster-image-load)
_cluster-image-load:
	$(MAKE) build-image IMG=$(IMG)
	kind load docker-image $(IMG) --name $${KIND_CLUSTER:-alitellm-operator-test}

##@ Waiters (use these; never write ad-hoc until/while loops)

WAIT_TIMEOUT ?= 300s

.PHONY: wait-cr-ready
wait-cr-ready: ## Wait for a CR Ready condition. Usage: make wait-cr-ready KIND=litellmconnection NAME=default NS=default
	@test -n "$(KIND)" -a -n "$(NAME)" -a -n "$(NS)" || { echo "ERROR: KIND= NAME= NS= all required" >&2; exit 1; }
	kubectl -n $(NS) wait --for=condition=Ready --timeout=$(WAIT_TIMEOUT) $(KIND)/$(NAME)

.PHONY: wait-operator
wait-operator: ## Wait operator Deployment Ready (bounded).
	kubectl -n default rollout status deploy/alitellm-operator --timeout=$(WAIT_TIMEOUT)

.PHONY: wait-litellm
wait-litellm: ## Wait LiteLLM Deployment Ready (bounded).
	kubectl -n litellm-system rollout status deploy/litellm --timeout=$(WAIT_TIMEOUT)

.PHONY: wait-mocks
wait-mocks: ## Wait all mock Pods Ready (bounded).
	kubectl -n mocks wait --for=condition=Ready --timeout=$(WAIT_TIMEOUT) pod --all

.PHONY: wait-container
wait-container: ## Wait for named container exit + PASS/FAIL marker. Usage: make wait-container NAME=<container> [TIMEOUT=600]
	@test -n "$(NAME)" || { echo "ERROR: NAME= required" >&2; exit 1; }
	@cid=$$(docker ps -q -f name=$(NAME)); \
	test -n "$$cid" || { echo "ERROR: no running container named '$(NAME)'" >&2; exit 1; }; \
	timeout $${TIMEOUT:-600} docker logs -f $$cid 2>&1 \
		| grep -m1 -E "PASS|FAIL|ok\s+github|--- FAIL|Ginkgo ran" \
		|| { echo "FAIL: marker not seen within $${TIMEOUT:-600}s (container may have exited early)" >&2; exit 1; }

.PHONY: operator-redeploy
operator-redeploy: ## rebuild operator image, kind-load, restart deploy (~20s inner loop)
	$(MAKE) build-image IMG=alitellm-operator:e2e
	kind load docker-image alitellm-operator:e2e --name alitellm-operator-test
	# kubectl runs THROUGH the devtools container: the kind kubeconfig lives at
	# /workspace/.gocache/kube/config (set by scripts/dev.sh), so host kubectl
	# has no context for the kind cluster (would fail "deployments not found").
	./scripts/dev.sh kubectl -n default rollout restart deploy/alitellm-operator
	./scripts/dev.sh kubectl -n default rollout status deploy/alitellm-operator --timeout=60s

##@ Logs & Debug

.PHONY: logs-operator logs-litellm logs-mocks
logs-operator: ## tail operator logs with timestamps
	kubectl -n default logs -f --timestamps deploy/alitellm-operator
logs-litellm:  ## tail LiteLLM logs with timestamps
	kubectl -n litellm-system logs -f --timestamps deploy/litellm
logs-mocks:    ## tail openai-mock + kubeai-mock logs in parallel (uses stern if present, else kubectl)
	@if command -v stern >/dev/null 2>&1; then \
	  stern -n mocks --timestamps . ; \
	else \
	  kubectl -n mocks logs -f --timestamps -l app=openai-mock & \
	  kubectl -n mocks logs -f --timestamps -l app=kubeai-mock ; wait ; \
	fi

.PHONY: watch-crs
watch-crs: ## kubectl get -w across all 7 in-scope kinds in default
	kubectl -n default get \
	  litellmconnections,models,modeldiscoveries,mcpservers,mcpserverdiscoveries,a2aagents,teams \
	  -w

.PHONY: pf-litellm pf-openai-mock pf-kubeai-mock
pf-litellm:     ## port-forward litellm svc to localhost:4000
	kubectl -n litellm-system port-forward svc/litellm 4000:4000
pf-openai-mock: ## port-forward openai-mock to localhost:8081
	kubectl -n mocks port-forward svc/openai-mock 8081:8080
pf-kubeai-mock: ## port-forward kubeai-mock to localhost:8082
	kubectl -n mocks port-forward svc/kubeai-mock 8082:8080

.PHONY: mock-mode
mock-mode: ## flip a mock auth mode (usage: make mock-mode INSTANCE=openai-mock MODE=reject-401)
	bash scripts/mock-set-mode.sh $(INSTANCE) $(MODE)

##@ E2E

.PHONY: e2e-run e2e-focus
e2e-run:    ## Phase 3 — run full e2e suite against running cluster
	$(call container_target,_e2e-run)
_e2e-run:
	go test -tags=e2e -v -count=1 -timeout 15m ./test/e2e/...

e2e-focus:  ## Phase 3 — run a single Ginkgo It (usage: make e2e-focus FOCUS='registers via POST /model/new')
	$(call container_target,_e2e-focus)
_e2e-focus:
	# `-args` is required: without it, `go test` parses the value after
	# `-ginkgo.focus=` as a package path and reports "no Go files in /workspace".
	go test -tags=e2e -v -count=1 -timeout 5m ./test/e2e/... -args -ginkgo.focus="$(FOCUS)"

.PHONY: e2e-full
e2e-full: ## Phase 3 — cluster-up (with ensure-inotify) → e2e-run, cluster KEPT for fast re-runs. Teardown is explicit: `make cluster-down`.
	$(MAKE) cluster-up
	$(MAKE) e2e-run
