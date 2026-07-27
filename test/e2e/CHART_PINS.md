# Tier 2 chart pins (canonical source)

Update protocol:
1. Edit this file.
2. Edit `test/e2e/cluster/01-deps/{litellm,toolhive}.values.yaml` to match.
3. Run `make cluster-sync` and `make test-tier2`.
4. Commit all three files in one commit with scope `chore(tier2): bump charts`.

| Chart | Version | Digest | App / Image | Notes |
|---|---|---|---|---|
| `oci://docker.litellm.ai/berriai/litellm-helm` | `1.93.0` | `sha256:eb4ecbd3c82f9c393c017b73bed7afe6e6cb22002f6793b4173e1d3b711dfa91` | image override `v1.93.0` (chart appVersion is `1.93.0`, no `v` prefix) | **BETA chart** — schema can drift between bumps. Re-run full Tier 2 suite + assert rendered tag on every bump. |
| `oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds` | `0.0.55` | `sha256:3e1c76c11947b99ccc848d71f3b59cb87ae76eec3e14033c91c6975eea69c85a` | CRDs only (no image) | Independent version stream from the operator chart — do NOT assume they match. |
| `oci://ghcr.io/stacklok/toolhive/toolhive-operator` | `0.5.5` | `sha256:f0d046eabe4ba3b82462a3c238d193d7625b21be2bce60a68e50c0455dde37af` | chart default operator image | Underlying ToolHive binary is `v0.27.2` (May 2026); neither chart's semver matches it. |

## Notes

- **ToolHive CRDs vs Operator** live on independent version streams (`0.0.x` vs `0.5.x`). Spec §13.1 originally claimed co-versioning — that is incorrect. Install order remains CRDs-first → operator (`Established=True` on the CRD is a prereq for the operator chart).
- **LiteLLM chart `1.93.0`** has `appVersion: 1.93.0` — note the missing `v` prefix, which does NOT resolve to a published `ghcr.io/berriai/litellm-database` tag. `test/e2e/cluster/01-deps/litellm.values.yaml` therefore overrides `image.tag` to `v1.93.0`. A Tier 2 scope assertion (AC-N1+N2, Task 9.11) verifies the rendered LiteLLM Deployment actually uses the overridden tag. **This file is the canonical operator image target** — the old `litellm_version_target.md` auto-memory no longer exists.
- **No more `-stable` tags.** Upstream stopped publishing the `vX.Y.Z-stable` suffix after the 1.83.x line (probed: `v1.91.4-stable`, `v1.92.1-stable`, `v1.93.0-stable` all 404). Pin the bare `vX.Y.Z` tag.
- **Bumped to 1.93.0** (2026-07-27) ahead of MCP toolset support (`GET|POST /v1/mcp/toolset`, `DELETE /v1/mcp/toolset/{id}`) — the endpoints do not exist on 1.83.10.
