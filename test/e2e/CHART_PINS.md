# Tier 2 chart pins (canonical source)

Update protocol:
1. Edit this file.
2. Edit `test/e2e/values/{litellm,toolhive}.values.yaml` to match.
3. Run `make cluster-hydrate` and `make test-tier2`.
4. Commit all three files in one commit with scope `chore(tier2): bump charts`.

| Chart | Version | Digest | App / Image | Notes |
|---|---|---|---|---|
| `oci://docker.litellm.ai/berriai/litellm-helm` | `1.84.0` | (pull-and-record) | image override `v1.83.10-stable` (chart default would be `v1.84.0`) | **BETA chart** — schema can drift between bumps. Re-run full Tier 2 suite + assert rendered tag on every bump. |
| `oci://ghcr.io/stacklok/toolhive/toolhive-operator-crds` | `0.0.55` | `sha256:3e1c76c11947b99ccc848d71f3b59cb87ae76eec3e14033c91c6975eea69c85a` | CRDs only (no image) | Independent version stream from the operator chart — do NOT assume they match. |
| `oci://ghcr.io/stacklok/toolhive/toolhive-operator` | `0.5.5` | `sha256:f0d046eabe4ba3b82462a3c238d193d7625b21be2bce60a68e50c0455dde37af` | chart default operator image | Underlying ToolHive binary is `v0.27.2` (May 2026); neither chart's semver matches it. |

## Notes

- **ToolHive CRDs vs Operator** live on independent version streams (`0.0.x` vs `0.5.x`). Spec §13.1 originally claimed co-versioning — that is incorrect. Install order remains CRDs-first → operator (`Established=True` on the CRD is a prereq for the operator chart).
- **LiteLLM chart `1.84.0`** has default `appVersion: v1.84.0`. The operator (this repo) targets `v1.83.10-stable` per auto-memory `litellm_version_target.md`. `test/e2e/values/litellm.values.yaml` overrides `image.tag` accordingly. A Tier 2 scope assertion (AC-N1+N2, Task 9.11) verifies the rendered LiteLLM Deployment actually uses the overridden tag.
- **Digest field** for litellm-helm is left as `(pull-and-record)` because the first plan-time pull lands in Phase 2 (Task 2.4 `helm pull oci://...`). Record the resolved digest here on first successful `helm pull`.
