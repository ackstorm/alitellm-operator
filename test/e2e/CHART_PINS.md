# Tier 2 chart pins (canonical source)

Update protocol:
1. Edit this file.
2. Edit `test/e2e/cluster/01-deps/{litellm,toolhive}.values.yaml` to match.
3. Run `make cluster-sync` and `make test-tier2`.
4. Commit all three files in one commit with scope `chore(tier2): bump charts`.

| Chart | Version | Digest | App / Image | Notes |
|---|---|---|---|---|
| `oci://docker.litellm.ai/berriai/litellm-helm` | `1.93.0` | `sha256:eb4ecbd3c82f9c393c017b73bed7afe6e6cb22002f6793b4173e1d3b711dfa91` | image override `v1.93.0` (chart appVersion is `1.93.0`, no `v` prefix) | **BETA chart** — schema can drift between bumps. Re-run full Tier 2 suite + assert rendered tag on every bump. |
| `oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds` | `0.41.0` | `sha256:6293a550141ca7bc85a15f281742477d53bc3132654c53994cd9ca3bdf000e78` | CRDs only (no image) | **Floor: >= 0.41.0** — first release serving `v1beta1`, which the operator reads exclusively. |
| `oci://ghcr.io/stacklok/toolhive/toolhive-operator` | `0.41.0` | `sha256:f1a3147e5b16bd24da45150f93d1b9ebfcfa26ef62ef654734025b793c0fe570` | chart default operator image `ghcr.io/stacklok/toolhive/operator:v0.41.0` | Chart semver now tracks the ToolHive release; still pinned independently of the CRDs chart. |

## Notes

- **ToolHive CRDs vs Operator** used to live on independent version streams (`0.0.x` vs `0.5.x`) and converged on the ToolHive release version at `0.41.0` (chart `appVersion` = `0.41.0` for both). Spec §13.1's original co-versioning claim was wrong for the old streams and is incidentally true today — do NOT rely on it; keep pinning each chart separately. Install order remains CRDs-first → operator (`Established=True` on the CRD is a prereq for the operator chart).
- **Bumped ToolHive `0.0.55`/`0.5.5` → `0.41.0`/`0.41.0`** (2026-07-31). `0.41.0` CRDs serve `v1beta1` (storage) alongside a deprecated `v1alpha1`, which retires the vendored `toolhive-v1beta1-crds.yaml` fixture and the `cluster.sh` step that applied it. The `crdsChartVersion` floor is asserted at hydration time: `install_toolhive` fails the run if any of `mcpservers` / `virtualmcpservers` / `mcpremoteproxies` stops serving `v1beta1`. The `operator.{liveness,readiness}Probe` overrides in `toolhive.values.yaml` are unchanged — the `operator:` values schema is the same in `0.41.0`.
- **LiteLLM chart `1.93.0`** has `appVersion: 1.93.0` — note the missing `v` prefix, which does NOT resolve to a published `ghcr.io/berriai/litellm-database` tag. `test/e2e/cluster/01-deps/litellm.values.yaml` therefore overrides `image.tag` to `v1.93.0`. A Tier 2 scope assertion (AC-N1+N2, Task 9.11) verifies the rendered LiteLLM Deployment actually uses the overridden tag. **This file is the canonical operator image target** — the old `litellm_version_target.md` auto-memory no longer exists.
- **No more `-stable` tags.** Upstream stopped publishing the `vX.Y.Z-stable` suffix after the 1.83.x line (probed: `v1.91.4-stable`, `v1.92.1-stable`, `v1.93.0-stable` all 404). Pin the bare `vX.Y.Z` tag.
- **Bumped to 1.93.0** (2026-07-27) ahead of MCP toolset support (`GET|POST /v1/mcp/toolset`, `DELETE /v1/mcp/toolset/{id}`) — the endpoints do not exist on 1.83.10.
