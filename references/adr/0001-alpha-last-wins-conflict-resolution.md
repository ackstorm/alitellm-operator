# ADR 0001 — Alpha-last-wins conflict resolution

- **Status:** Accepted (2026-05-26)
- **Deciders:** @juanca, @ackstorm-operator-team
- **Supersedes:** prior per-kind ad-hoc strategies (MCPServerDiscovery intra-discovery first-wins, MCPServerDiscovery cross-Discovery DuplicateDiscovery skip).

## Context

The operator is **namespace-scoped** — the manager cache restricts
every kind to `WatchNamespace`. Apiserver uniqueness on
`(namespace, metadata.name)` means a cross-CR conflict only exists
when the LiteLLM-side natural key for a kind can collide between two
CRs **in the same namespace**, i.e. when the natural key is something
other than `metadata.name`.

Several CR kinds have a natural key that collapses to `metadata.name`
(LiteLLMTeam, LiteLLMModel, LiteLLMA2AAgent — Team additionally
forces `team_alias = metadata.name` at the controller layer). For
those kinds there is no reachable cross-CR conflict surface today;
their existing LiteLLM-side adoption logic (smallest-`team_id`
duplicate rule, `GetModelInfoByName`, `resolveAgentIDByName`) handles
"LiteLLM already has an entry with this name" correctly and stays
unchanged.

The kinds with a real conflict surface had inconsistent behavior:

| Kind                       | Old behavior                                                                  |
|----------------------------|-------------------------------------------------------------------------------|
| LiteLLMMCPServer           | None — sanitization collapse onto the same `server_name` went silently         |
| LiteLLMMCPServerDiscovery  | Intra-discovery: first-wins; Cross-discovery: `DuplicateDiscovery` skip-and-warn |
| LiteLLMModelAlias          | Aggregate, alpha-last-wins per slot (already aligned)                          |

This made the operator's behavior unpredictable across kinds and gave
users no single mental model for "which CR is in charge?".

`LiteLLMGuardRail` was initially classified as a conflict-bearing
kind under the assumption that its `anySiblingOwns` gate was
sibling-rejection. On closer reading the gate enables a deliberate
load-balancing-pool pattern: multiple CRs sharing `spec.guardrailName`
each create their own LiteLLM row and form a pool. Applying
alpha-last-wins here would silently delete pool members. The kind is
therefore left wired to its existing LB-pool semantics and is
documented as having no alpha-last-wins conflict surface.

## Decision

Adopt one rule for every kind that has a reachable conflict surface in
namespace-scoped mode: sort the candidate CRs by `<namespace>/<name>`
ASC and the LAST one wins. Loser CRs surface
`Ready=False, Reason=Conflict, Message="superseded by <ns>/<name>"`
and do not call LiteLLM.

The rule is applied to:

- `LiteLLMMCPServer` (new — handles sanitization-collapse case)
- `LiteLLMMCPServerDiscovery` intra-discovery dedup (flipped from
  first-wins to last-wins)
- `LiteLLMMCPServerDiscovery` cross-Discovery child ownership
  (replaces `DuplicateDiscovery` skip with alpha-last-Discovery wins)

`LiteLLMModelAlias` already implements the rule per slot.
`LiteLLMConnection` and `LiteLLMGuardRail` have no alpha-last-wins conflict surface; GuardRail's shared-name pattern is the LB-pool feature.
`LiteLLMTeam`, `LiteLLMModel`, `LiteLLMA2AAgent` have no reachable
conflict surface in namespace-scoped mode and are not wired (would be
dead code).
`LiteLLMModelDiscovery` resolves conflicts at the Kubernetes layer via
Server-Side Apply with `ForceOwnership` — deliberate exception.

## Consequences

- For the wired kinds, behavior becomes a pure function of
  `(kind, namespace, name)`. Users can predict the winner without
  inspecting LiteLLM state or operator logs.
- Renaming a CR is the single supported "I want this one to win"
  affordance.
- `LiteLLMMCPServer` gains a resolver on the sanitized server_name. CR
  sets that previously relied on silently overwriting each other after
  sanitization will see the alphabetically-last CR own the LiteLLM
  entry and the others surface `Reason=Conflict`.
- `LiteLLMMCPServerDiscovery` intra-discovery dedup direction flips:
  the alphabetically-last upstream source survives instead of the
  first. Source ordering is still deterministic (sorted by
  `<sourceNamespace>/<sourceName>`), but the surviving entry changes.
- `LiteLLMMCPServerDiscovery` cross-Discovery: `DuplicateDiscovery`
  classification is replaced with alpha-last-Discovery wins.
- New metric `litellm_operator_conflicts_total{kind, role}` and new
  events `ConflictDetected` / `ConflictWon` give operators visibility
  into resolution churn.
- If the operator ever moves to multi-namespace watch, the
  `metadata.name`-keyed kinds (Team, Model, A2AAgent) will gain a
  reachable conflict surface; wiring the resolver there is left as
  future work (called out in the ROADMAP).

## Alternatives considered

1. **First-create-wins** — depends on `creationTimestamp`, which races
   with the apiserver clock and is invisible in normal listings.
   Rejected.
2. **Smallest-CR-name-wins** — same shape, opposite direction. Equally
   valid; we chose last-wins so that adding a Z-prefixed CR is the
   obvious "take over" affordance.
3. **Explicit `spec.priority` field** — adds API surface and a new way
   to be wrong (ties). The alpha-by-name rule has no degenerate tie
   case because `(namespace, name)` is unique within a namespace.
4. **Wire the resolver on every kind for forward-compat** — would add
   dead code today (resolver returns single-candidate winner trivially
   on `metadata.name`-keyed kinds). Rejected in favor of wiring only
   where the conflict can actually occur; a future multi-namespace
   change can revisit.
5. **Reject all conflicts** (GuardRail's old behavior, generalized) —
   leaves the LiteLLM entity in an indeterminate state because the
   first CR to reach Ready is the de-facto owner. Rejected.
