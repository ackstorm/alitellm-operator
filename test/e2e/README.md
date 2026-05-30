# E2E — kind + Helm test environment

## Three test phases

This file documents **Phase 3 (e2e)**. The full taxonomy:

| Phase     | Where                                                             | What                                          | Speed       |
|-----------|-------------------------------------------------------------------|-----------------------------------------------|-------------|
| `unit`    | `internal/{litellm,providers,normalize,filters,substitution,connection,metrics,...}/*_test.go` | Pure Go, no apiserver                         | ~5s warm    |
| `envtest` | `internal/controller/*_test.go` + `internal/toolhive/*_test.go`   | Real kube-apiserver+etcd, no pods             | ~7 min      |
| `e2e`     | `test/e2e/*_test.go`                                              | Real kind cluster, Helm-deployed operator     | ~6–7 min    |

Run with:

- `make test-unit`                  → ~5s
- `make test-envtest`           → ~7 min
- `make e2e-full`              → cluster-up → e2e, cluster KEPT (CI gate; teardown is explicit `make cluster-down`)
- `make e2e-run`                    → dev loop (re-run e2e against the kept cluster)

Two equally-supported e2e workflows below.

## CI gate (PR-blocking)

```
make e2e-full          # cluster-up → e2e, cluster KEPT (teardown explicit: make cluster-down)
```

Wall clock ≤10min target. See the `e2e` job in `.github/workflows/ci.yml`.

## Dev iteration loop (recommended local default)

```
make cluster-keep       # ~3min once per session
# ── edit code ──
make operator-redeploy  # ~20s; hot-reloads our operator only
make e2e-focus FOCUS='registers via POST /model/new'   # re-run one It
# ── edit values ──
make cluster-hydrate    # ~30-60s; re-applies all Helm releases (peer operators incl.)
make cluster-status     # one-screen view
# ── done ──
make cluster-down
```

## Useful helpers

```
make logs-operator      # tail our operator (timestamps)
make logs-litellm       # tail LiteLLM
make logs-mocks         # both mocks
make watch-crs          # kubectl get -w across all 7 in-scope kinds
make pf-litellm         # port-forward localhost:4000 → litellm svc
make mock-mode INSTANCE=openai-mock MODE=reject-401   # toggle 401 fast-path
```

## Phase 2 inner-loop (developer envtest)

To skip the per-invocation `setup-envtest use` call when iterating on
the controller envtest layer, export the assets path once per shell
inside the devtools container:

```
export KUBEBUILDER_ASSETS="$(./scripts/envtest-assets-path.sh)"
```

Then run `go test ./internal/controller/...` directly. Saves ~1s per
invocation; re-export after upgrading controller-runtime.

## Config surface

All tunables live in `test/e2e/values/*.values.yaml`. Edit + `make cluster-hydrate`.
Chart version pins are in `test/e2e/CHART_PINS.md`.

## Forensics

On any Ginkgo failure, `AfterEach` dumps to `/tmp/e2e-*.log` inside the devtools
container (operator + LiteLLM Pod logs, all CRs YAML, events). CI uploads them
as artifacts under the `e2e-forensics` name.

## Dual-vintage ToolHive CRDs

Phase 9 (Task 09-08) added v1beta1 coverage to the MCPServerDiscovery suite.

The published `toolhive-operator-crds` OCI chart (pinned in `CHART_PINS.md`) ships
`v1alpha1` only. The `v1beta1` CRD version is hydrated from
`test/e2e/fixtures/toolhive-v1beta1-crds.yaml`, which is vendored from
`stacklok/toolhive v0.28.0` (commit `748a64228710ce241a225f5530022ce2c96cc23e`).

`scripts/cluster.sh hydrate` installs the fixture via `kubectl apply` immediately
after the ToolHive OCI chart finishes. The apply is idempotent — re-running
`make cluster-hydrate` is safe. After hydration, both CRD versions are served:

```
kubectl get crd mcpservers.toolhive.stacklok.dev \
  -o jsonpath='{.spec.versions[*].name}'
# Expected output: v1alpha1 v1beta1
```

The operator's dedup rule (v1alpha1 wins on same-name collision across both
versions) is exercised in `internal/toolhive/informer_test.go::TestInformer_DualVersionDedup`
(envtest). The end-to-end behavioral-parity assertion lives in
`test/e2e/mcpserverdiscovery_test.go` as the `"propagates v1beta1 ToolHive
MCPServer into child MCPServer (dual-version coverage)"` It block.

When `stacklok/toolhive` publishes a chart that natively carries v1beta1,
drop this fixture and revisit dedup defaults.

## Devtools container

Host has no Go toolchain. All `make`, `go`, `helm`, `kubectl`, `kind`,
`docker build` invocations must go through `./scripts/dev.sh`. Wrapper
mounts the repo + host docker socket and builds the devtools image
lazily on first call.

## Names

The e2e cluster + images use the `litellm-` / `alitellm-` prefix:

- kind cluster: `alitellm-operator-test`
- mock image:   `litellm-mock:e2e`
- operator img: `alitellm-operator:e2e`

The operator watches the `default` namespace by default.
