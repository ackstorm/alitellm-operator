# `make` command reference — the single developer interface

`make` is the deterministic entry point for every developer workflow in
this repo. You do not call `go`, `helm`, `kind`, `kubectl`,
`golangci-lint`, `controller-gen`, or `setup-envtest` directly, and you
do not call `scripts/dev.sh` or `scripts/cluster.sh` by hand — those are
internal plumbing. Pick a `make` target; it routes itself to the correct
execution context.

## Host requirements

Docker only. Everything else (Go, helm, kind, kubectl, golangci-lint,
controller-gen, setup-envtest) lives in the `litellm-devtools` container
and is reached through `make` targets that auto-route via
`scripts/dev.sh`.

Optional: host `kubectl` may be used for debugging against the kind
cluster. The kubeconfig is written to `./.gocache/kube/config`:

    KUBECONFIG=$PWD/.gocache/kube/config kubectl get pods -n default

This is OPTIONAL and not required by any `make` target or by `make doctor`.

### Host kernel: inotify limits (kind)

kind runs each Kubernetes node as a docker container; kubelet,
containerd, and the API server each consume `fs.inotify` instances. The
common distro default (`fs.inotify.max_user_instances=128`) gets
exhausted partway through hydration and the API server crashes with
`connection refused` MID-bringup (e.g. while helm installs the mocks or
litellm). `make cluster-up` / `make cluster-reset` run an
`ensure-inotify` prerequisite (context B, host) that raises the limits
via `sudo -n sysctl -w` if they are below threshold. It is best-effort
and non-fatal: with no passwordless sudo it prints the manual command
and continues. Because inotify is a HOST kernel knob (not namespaced),
this MUST run on the host before `cluster-up` routes into the devtools
container — hence a plain prerequisite, not a `container_target`. To
persist across reboots, add a drop-in:

    echo -e 'fs.inotify.max_user_instances=512\nfs.inotify.max_user_watches=524288' \
      | sudo tee /etc/sysctl.d/99-kind-inotify.conf && sudo sysctl --system

## The 3-context model

Every target runs in exactly one of three contexts. The Makefile picks
the right one for you; this table is so you understand WHERE a target's
work happens when you debug it.

| Ctx | Where it runs | Tools available | Examples |
|-----|---------------|-----------------|----------|
| **A** Devtools container | inside `litellm-devtools:latest` via `scripts/dev.sh` (auto-wrapped by the `container_target` macro) | go, helm, kind, kubectl, golangci-lint, controller-gen, setup-envtest, kustomize, crd-ref-docs | `gen-*`, `test-*`, `qa-*` (except gates), `build-operator`/`build-installer`, `install`/`deploy`, `helm-sync*`/`deploy-kustomize-sync*`, `cluster-*`, `e2e-run`/`e2e-focus`, `shell` |
| **B** Host + docker | directly on the host (needs only the docker CLI/daemon) | docker, kind | `build-image`, `build-image-mock`, `docker-push`/`docker-load`/`docker-buildx`, `doctor`, `ensure-inotify`, gate orchestrators `pre-push`/`verify`, `release-bump`/`release-cut`, `operator-redeploy`, `e2e-full` (orchestrates context-A children) |
| **C** Kubernetes infra | host `kubectl` against the kind cluster (kubeconfig at `./.gocache/kube/config`) | kubectl | `wait-*`, `logs-*`, `watch-crs`, `pf-*`, `mock-mode` |

> **Context C caveat — these targets are NOT auto-routed.** `wait-*`,
> `logs-*`, `watch-crs`, `pf-*`, and `mock-mode` are bare-`kubectl`
> targets that run on whatever host invokes them. The kind kubeconfig
> lives at `./.gocache/kube/config` (wired into the devtools container by
> `scripts/dev.sh`, NOT the host's default `~/.kube/config`), so on the
> canonical Go-less dev host — which has no `kubectl` and no kind
> context — `make wait-operator` / `make logs-operator` will fail. On
> such a host, invoke them as `./scripts/dev.sh make <target>` (the
> devtools container has `kubectl` and `KUBECONFIG=.gocache/kube/config`
> pre-wired).

> **Why the split is explicit.** Context-A targets opt in to container
> routing by calling the `container_target` macro (see below). There is
> NO magic-by-prefix — a target is only wrapped if it asks to be. That
> keeps `make help` honest and prevents a future host-only target from
> being auto-wrapped by accident.

## How auto-routing works (`LITELLM_IN_DEVTOOLS` + `container_target`)

A context-A public target is a thin wrapper over a private `_`-prefixed
target that holds the real recipe:

```makefile
test-unit: ## Phase 1 — pure-logic tests (~10s warm).
	$(call container_target,_test-unit)
_test-unit: qa-fmt-check vet
	go test ...
```

`container_target` expands to: "if `LITELLM_IN_DEVTOOLS=1`, run
`$(MAKE) _test-unit` directly; otherwise run
`./scripts/dev.sh $(MAKE) _test-unit`." `scripts/dev.sh` sets
`LITELLM_IN_DEVTOOLS=1` inside the container, so:

- On the **host**: `make test-unit` → `./scripts/dev.sh make _test-unit`
  → container starts → `_test-unit` runs with the Go toolchain present.
- **Inside** the container (`LITELLM_IN_DEVTOOLS=1`): the macro
  short-circuits to `$(MAKE) _test-unit` — no nested container.

`scripts/dev.sh` itself has a matching guard: if `LITELLM_IN_DEVTOOLS=1`
it `exec`s the command directly instead of launching another container.
So `./scripts/dev.sh make test-unit` from the host still works (one
container, not two), and CI can keep or drop the explicit `dev.sh`
prefix freely.

The macro also forwards `$(MAKEOVERRIDES)` so command-line variable
assignments (`PKG=…`, `FOCUS=…`, `TIMEOUT=…`, `BASE_REF=…`) cross the
docker boundary — `scripts/dev.sh` only forwards an explicit `-e`
allowlist, so without this an arg-taking wrapper like `test-envtest-pkg`
or `e2e-focus` would see empty values inside the container.

## Cluster lifecycle

`scripts/cluster.sh` cannot run on the Go-less host (it needs
helm/kind/kubectl, which live only in the devtools container), so always
drive it through `make cluster-*` —
the `cluster-*` targets are context A (they route into the container and
drive kind/helm via the mounted docker socket).

| Target | Semantics |
|--------|-----------|
| `cluster-up` | create kind cluster + apply `test/e2e/cluster/` phases (namespaces, deps, mocks, operator, hydration) + wait Ready + `verify`. Runs `ensure-inotify` first (host). |
| `cluster-hydrate` | re-apply hydration (namespaces + install + fixtures) on an ALREADY-running cluster (never recreates the kind node). |
| `cluster-sync` | parity alias of `cluster-hydrate` (matches `../ach`): re-apply all phases in place + `verify`. |
| `cluster-verify` | health-gate the standing state on a running cluster (rollout/Ready checks, no mutation). |
| `cluster-keep` | alias of `cluster-up` (kept for naming consistency with the spec). |
| `cluster-reset` | Make compose: `cluster-down` then `cluster-up` (clean recreate). |
| `cluster-down` | delete the kind cluster. |
| `cluster-status` | print kind / namespace / fixture state. |
| `cluster-image-load` | Make compose: `build-image` (host docker) + `kind load` the operator image (`IMG=…`). |

There is **no** `cluster-reset` script verb and **no** cluster-preflight
verb — `cluster-reset` is composed from `cluster-down`+`cluster-up` in
the Makefile, and `scripts/cluster.sh` only knows `up`, `hydrate`, `sync`,
`down`, `keep`, `status`, `verify`. There is no `doctor-cluster` target.

## Command vocabulary

### Diagnostics
| Target | Ctx | Description |
|--------|-----|-------------|
| `doctor` | B | Fast local preflight: docker, devtools image, socket, cache paths, in-container tools, kubeconfig. No network. |
| `shell` | A | Interactive shell inside the devtools container. |

### Code generation & formatting (`gen-`, `fmt`, `vet`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `gen-manifests` | A | controller-gen CRDs + RBAC + webhook manifests. |
| `gen-code` | A | controller-gen DeepCopy methods. |
| `gen-crd-ref-docs` | A | Render `docs/api-reference/` from CRD Go types (crd-ref-docs). |
| `fmt` | A | `go fmt` against code (mutates). |
| `vet` | A | `go vet` against code. |
| `qa-fmt-check` | A | Fail if any Go file is not gofmt-clean (no mutation). |

### Tests (`test-`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `test-full` | A | All non-cluster tests (`test-unit` + `test-envtest`, race-enabled). |
| `test-unit` | A | Pure-logic unit tests (~10s warm). |
| `test-envtest` | A | Controller envtest with -race (alias of `test-envtest-race`; CI gate, ~7m). |
| `test-envtest-race` | A | Controller envtest with -race. |
| `test-envtest-fast` | A | Controller envtest WITHOUT -race (dev loop, ~3m). |
| `test-unit-pkg PKG=…` | A | Unit tests for one package. |
| `test-envtest-pkg PKG=… [FOCUS=…] [TIMEOUT=…]` | A | envtest for one package. |
| `test-smoke-idempotency` | A | Accelerated AC-R1 idempotency smoke (10s). |
| `test-smoke-idempotency-long` | A | Real 35-min AC-R1 idempotency (nightly). |
| `test-leak-soak` | A | REL-03 1000-reconcile leak harness (nightly). |

### QA / security (`qa-`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `qa-lint` | A | golangci-lint full sweep. |
| `qa-lint-fix` | A | golangci-lint with `--fix`. |
| `qa-lint-config` | A | Verify golangci-lint config. |
| `qa-lint-changed [BASE_REF=…]` | A | Lint only packages touched vs BASE_REF (default `origin/main`). |
| `qa-security` | A | gosec (via lint) + govulncheck (ack-list aware) + `qa-fuzz-short`, ≤6m. |
| `qa-fuzz-short` | A | Go fuzz targets, 60s budget each (CI cadence). |
| `qa-fuzz-long` | A | Go fuzz targets, 10-min budget each (nightly). |

### Build (`build-`, `docker-`, `run`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `build-operator` | A | Build the `alitellm-operator` binary. |
| `run` | A | Run a controller from your host (via the container Go toolchain). |
| `build-installer` | A | Generate consolidated `dist/install.yaml` (CRDs + deployment). |
| `build-image` | B | Build the operator container image (`IMG=…`). |
| `build-image-mock` | B | Build `litellm-mock:e2e` (LiteLLM-shaped mock). |
| `docker-push` | B | Push the operator image (`IMG=…`). |
| `docker-load` | B | `kind load` the operator image into the kind cluster (`IMG=…`). |
| `docker-buildx` | B | Cross-platform buildx build + push (`IMG=…`, `PLATFORMS=…`). |

### Deployment (`install`, `deploy`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `install` | A | Install CRDs into the cluster in `~/.kube/config`. |
| `uninstall` | A | Uninstall CRDs (`ignore-not-found=true` to ignore missing). |
| `deploy` | A | Deploy the controller into the cluster. |
| `undeploy` | A | Undeploy the controller (`ignore-not-found=true` to ignore missing). |

### Packaging & Sync
| Target | Ctx | Description |
|--------|-----|-------------|
| `helm-sync` | A | Regenerate the Helm `install.yaml` from `dist/install.yaml` (kustomize canonical, Helm veneer). |
| `helm-sync-check` | A | CI gate: fail if `helm-sync` produced an uncommitted diff. |
| `deploy-kustomize-sync` | A | Regenerate `deploy/kustomize/manager-rbac.yaml` from `config/rbac/`. |
| `deploy-kustomize-sync-check` | A | CI gate: fail on drift between `config/rbac/` and the bundled snapshot. |
| `ac-n3-audit` | A | SCOPE-03/AC-N3 static gate: fail if any non-test `.go` references `/user/` or `/key/` literals. |
| `samples-audit` | A | DEPLOY-02 gate: fail if any sample manifest carries a `TODO(user)` placeholder. |

### Cluster (`cluster-`)
See "Cluster lifecycle" above.

### Waiters (`wait-`) — context C
`wait-operator`, `wait-litellm`, `wait-mocks`,
`wait-cr-ready KIND=… NAME=… NS=…`,
`wait-container NAME=… [TIMEOUT=…]`. Default `WAIT_TIMEOUT=300s`;
`wait-container` takes `TIMEOUT=<seconds>` (default 600). Never write
ad-hoc `until …; do sleep N; done` loops — add a `wait-*` target. Each
`wait-*` uses one blessed pattern: `kubectl wait --timeout`,
`kubectl rollout status --timeout`, `timeout N docker logs -f <cid> |
grep -m1`, or `docker wait <cid>`.

### Logs & debug (`logs-`, `watch-crs`, `pf-`, `mock-mode`, `operator-redeploy`) — context C/B
`logs-operator` (operator in `default`), `logs-litellm` (litellm in
`litellm-system`), `logs-mocks` (openai-mock + kubeai-mock in `mocks`),
`watch-crs` (`kubectl get -w` across all 7 in-scope kinds in `default`),
`pf-litellm` / `pf-openai-mock` / `pf-kubeai-mock` (port-forwards),
`mock-mode INSTANCE=… MODE=…` (flip a mock auth mode),
`operator-redeploy` (host: `build-image` + `kind load` + `rollout
restart`, ~20s inner loop).

### E2E (`e2e-`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `e2e-full` | B | Make compose: `cluster-up` (with `ensure-inotify`) → `e2e-run`. The cluster is **KEPT** for fast re-runs — teardown is explicit (`make cluster-down`). There is NO `e2e-keep` target; `e2e-full` is the keep-cluster path. |
| `e2e-run` | A | Run the full e2e suite against an already-up cluster. |
| `e2e-focus FOCUS='…'` | A | Run a single Ginkgo `It` (ginkgo focus regexp). |

### Release (`release-`) — context B
| Target | Description |
|--------|-------------|
| `release-bump VERSION=X.Y.Z` | Internal: bump version across all manifests (used by `release.yml`). |
| `release-cut VERSION=X.Y.Z` | Empty `chore(release): vX.Y.Z` commit + pre-push + push to main. |

### Gates (no prefix) — context B (host-only)
| Target | Description |
|--------|-------------|
| `pre-push` | 17-gate publication check (scanners + lint + unit + SPDX + govulncheck + …). Installed git hook. |
| `verify` | `qa-lint` + `test-unit` + in-container `qa-security` + host `pre-push` — full gate bundle. |
| `hooks` | Install the pre-push git hook (and remove any stale pre-commit hook). |

> `pre-push`/`verify` are host-only — they spawn
> gitleaks/trufflehog containers on host docker. Do NOT call them via
> `./scripts/dev.sh` (it would nest docker mounts that don't resolve).

### Documentation (`docs-`, `gen-crd-ref-docs`)
| Target | Ctx | Description |
|--------|-----|-------------|
| `gen-crd-ref-docs` | A | Render `docs/api-reference/` from CRD Go types (crd-ref-docs tool). |
| `docs-build` | B | Build the mkdocs site (regenerates api-reference first) via docker. |
| `docs-serve` | B | Local mkdocs preview at :8000 via docker. |

## Debugging against the kind cluster

Host `kubectl` has no context for the kind cluster by default — the
kubeconfig lives at `./.gocache/kube/config` (written by `cluster.sh`
inside the container, bind-mounted to the host). Two ways in:

```bash
# Option 1 — point host kubectl at the kind kubeconfig (optional).
KUBECONFIG=$PWD/.gocache/kube/config kubectl get pods -n default

# Option 2 — run kubectl inside the devtools container.
make shell
kubectl get pods -n default
```

`make logs-*` and `make wait-*` already read this kubeconfig. Namespaces
in the e2e layout: the operator runs in `default`, LiteLLM in
`litellm-system`, the mocks (openai-mock + kubeai-mock) in `mocks`.
