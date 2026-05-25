# Development

Local dev loop for `alitellm-operator`. The repo deliberately keeps
the host clean — there is NO `go`, `kubebuilder`, `controller-gen`,
`kustomize`, `setup-envtest`, or `golangci-lint` on PATH. Everything
runs through the devtools container.

For higher-level orientation see [Architecture](architecture.md).
For PR / commit / release flow see [Contributions](contributions.md).

## Prerequisites

- **Docker** (host) — image pulls + the devtools container.
- **kind** + **kubectl** + **helm** (host) — for the E2E loop. Only
  invoked outside the devtools container.
- That's it. The devtools container ships pinned Go 1.24.13,
  kubebuilder v4.4.0, controller-runtime v0.19.4, k8s.io/* v0.31.0,
  govulncheck v1.3.0.

## Devtools container

```bash
./scripts/dev.sh <command>                # one-shot
./scripts/dev.sh bash                     # interactive shell
LITELLM_DEVTOOLS_REBUILD=1 ./scripts/dev.sh true   # force rebuild
```

The wrapper:

- Mounts the repo at `/workspace`.
- Mounts `/var/run/docker.sock` (so e2e can spawn kind / mock images
  from inside).
- Preserves host UID:GID — no root-owned files in the working tree.
- Persists Go module + build caches under `.gocache/`.
- Resolves `KUBEBUILDER_ASSETS` for envtest.

`make` targets that shell out to `go` MUST be prefixed
`./scripts/dev.sh`. Targets that only call `kubectl`/`docker`/`helm`/
`kind`/`bash` (`make cluster-up`, `make operator-redeploy`,
`make logs-*`, `make pre-push`) run on the host.

## Inner loop — code → unit → envtest

```bash
./scripts/dev.sh make unit                # ~5s warm, every iteration
./scripts/dev.sh make unit-pkg PKG=./internal/litellm/...
./scripts/dev.sh make envtest-fast        # ~3m, no -race, dev loop
./scripts/dev.sh make envtest-run         # ~7m, -race, before commit
./scripts/dev.sh make envtest-pkg PKG=./internal/controller/... \
                                  FOCUS=TestTeamReconciler_AC_T4 \
                                  TIMEOUT=10m
./scripts/dev.sh make lint                # full repo
./scripts/dev.sh make lint-changed        # only packages touched vs origin/main
```

`make test-all` = `unit` + `envtest-run`.

## Codegen + manifests

```bash
./scripts/dev.sh make manifests           # regenerate CRDs + RBAC + webhooks
./scripts/dev.sh make generate            # regenerate zz_generated_deepcopy.go
./scripts/dev.sh make gen-crd-ref-docs    # regenerate docs/api-reference/
./scripts/dev.sh make fmt                 # go fmt
```

CRD output lands in `config/crd/bases/` + `deploy/helm/alitellm-operator/crd-sources/`
(the chart bundle). The Helm chart's `templates/install.yaml` is
regenerated from kustomize via `make helm-sync` — there is a CI gate
(`helm-sync-check`) that fails on drift.

## E2E loop (kind + Helm)

`make e2e-full` is the clean-room final gate (~10m from cold). It
brings kind up, installs the chart, runs Ginkgo, tears down. For
iteration use the kept-cluster loop:

```bash
# 1. Bring cluster up once; keep state across iterations
./scripts/dev.sh make e2e-keep
# = scripts/cluster.sh keep + make e2e (NO teardown after)

# 2. Diagnose live
./scripts/dev.sh bash -c "kubectl -n default logs deploy/alitellm-operator --tail=200"
./scripts/dev.sh bash -c "kubectl -n default describe team finance"

# 3. Iterate with focused tests
./scripts/dev.sh make e2e-focus FOCUS="rateLimits composite"

# 4. After code edit → hot-reload → re-test (~30s)
./scripts/dev.sh make operator-redeploy
./scripts/dev.sh make e2e-focus FOCUS="..."

# 5. Final gate from clean
make cluster-down
./scripts/dev.sh make e2e-full
```

Never push a change touching `internal/controller/`,
`internal/litellm/`, `api/litellm/v1alpha1/`,
`deploy/helm/alitellm-operator/`, or `test/e2e/` without confirming
E2E green.

## Waiting on cluster state — use blessed targets

Naked polling loops (`until ...; do sleep N; done`) are banned —
they hang when the polled target disappears. Use the make targets
that wrap `kubectl wait` / `kubectl rollout status` / `docker wait`
with explicit timeouts:

| Need                            | Target                                               |
|---------------------------------|------------------------------------------------------|
| CR `Ready=True`                 | `make wait-cr-ready KIND=... NAME=... NS=...`        |
| Operator Deployment ready       | `make wait-operator`                                 |
| LiteLLM Deployment ready        | `make wait-litellm`                                  |
| Mock pods ready                 | `make wait-mocks`                                    |
| Container exit + PASS/FAIL      | `make wait-container NAME=<container>`               |
| Full cluster hydration          | `make cluster-up` (synchronous; don't poll after)    |
| Operator hot-reload + ready     | `make operator-redeploy` (bounded `rollout status`)  |

Default `WAIT_TIMEOUT=300s` (override per call). Add a new `wait-*`
target if the need isn't covered.

## Security + pre-push gates

```bash
./scripts/dev.sh make security    # gosec + govulncheck + fuzz-short (~6m)
make pre-push                     # host-only; 15 publication gates
```

`make pre-push` MUST run on the host — it spawns gitleaks/trufflehog
containers on host docker; calling it via `./scripts/dev.sh` nests
docker mounts that don't resolve.

`make verify` = `security` + `pre-push`. `make hooks` installs the
git pre-push hook.

If a govulncheck advisory is reachable, fix it. If it cannot be
fixed (e.g. waiting for an upstream stdlib release), add an entry to
`references/security/govulncheck-acknowledged.md` in the SAME commit
that introduces the dependency change.

## Useful inspection targets

```bash
make logs-operator      # tail operator with timestamps
make logs-litellm       # tail LiteLLM
make logs-mocks         # tail openai-mock + kubeai-mock in parallel
make pf-litellm         # port-forward LiteLLM svc → localhost:4000
make pf-openai-mock     # port-forward openai-mock → localhost:8081
make pf-kubeai-mock     # port-forward kubeai-mock → localhost:8082
make mock-mode INSTANCE=openai-mock MODE=reject-401   # flip a mock's auth mode
```

## Environment variables (development)

| Variable                                 | Effect                                                          |
|------------------------------------------|-----------------------------------------------------------------|
| `LITELLM_DEVTOOLS_REBUILD=1`             | Force rebuild of the devtools container on next `./scripts/dev.sh` call. |
| `KUBEBUILDER_ASSETS`                     | Resolved by `scripts/dev.sh`; do not override.                  |
| `IMG`                                    | Image tag for `make docker-load` (kind sideload). Default `alitellm-operator:e2e`. |
| `LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL`| Override the vanish-probe cadence at operator runtime. Floor 5s. Useful for tightening drift recovery in dev. |
| `WATCH_NAMESPACE`                        | Override the operator's watch namespace. Default `default`.     |

## Where to read next

- [Architecture](architecture.md) — reconciler shape, list cache,
  vanish probe.
- [Contributions](contributions.md) — PR + commit + review flow.
- [Release Process](release-process.md) — cutting a release.
- [`CLAUDE.md`](https://github.com/ackstorm/alitellm-operator/blob/main/CLAUDE.md)
  — agent-facing surgical reference card (authoritative for the
  guardrails on dev loop, waits, and pre-push).

## Help

```bash
make help              # all Makefile targets
./scripts/dev.sh make help
```
