# Roadmap

_TBD — milestones will be drafted post-v0.0.1._

## Deferred items

### Per-CR reachability probe (FIX.txt M-4)

Status: **deferred — design SPEC required**.

Source: FIX.txt MEDIUM-4 (2026-05-22 EKS smoke-test). LiteLLMA2AAgent,
LiteLLMModel, and LiteLLMMCPServer all surface Ready=True after
LiteLLM's 2xx registration response, but the inference / invoke / session
path may still fail (unreachable Service endpoint, IAM able-to-List but
not-to-Invoke, etc.). The reconciler has no visibility into the
inference-time failure mode.

Phase 4–5 scope (NOT a fast fix):
- Per-kind synthetic-call semantics (A2AAgent invoke, Model embed/chat,
  MCPServer session open).
- New optional `spec.probe.enabled` flag on the relevant CRDs.
- New `Probed` status condition (separate from `Ready` so existing
  semantics don't shift).
- Rate-limit ceiling for the synthetic calls (cost containment).
- Optionally surface LiteLLM's own per-server health endpoint
  (`/v1/mcp/server/health` available since v1.85.1).

Recommended next step: `/gsd:spec-phase per-cr-reachability-probe` to
produce a SPEC.md, then `/gsd:plan-phase` once the contract is locked.
