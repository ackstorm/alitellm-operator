# ADR 0001 — Alpha-last-wins conflict resolution

- **Status:** Accepted (2026-05-26)
- **Deciders:** @juanca, @ackstorm-operator-team
- **Supersedes:** prior per-kind ad-hoc strategies (smallest team_id,
  first-reconcile silent overwrite, sibling-rejects, intra-discovery
  first-wins).

## Context

Each CR kind has its own LiteLLM-side natural key (team alias, model
name, MCP server name, agent name, guardrail name, etc.). Until this
ADR, every controller had its own conflict-resolution behavior:

| Kind                       | Old behavior                                  |
|----------------------------|-----------------------------------------------|
| LiteLLMTeam                | sort by LiteLLM `team_id` ASC, update smallest |
| LiteLLMModel               | first-reconcile silent overwrite + adoption  |
| LiteLLMMCPServer           | adopt first match by name (order-dependent)  |
| LiteLLMA2AAgent            | adopt first match by name (order-dependent)  |
| LiteLLMGuardRail           | reject if sibling already owns the name      |
| LiteLLMMCPServerDiscovery  | intra-discovery first-wins                    |
| LiteLLMModelAlias          | aggregate, alpha-last-wins per slot           |

This made the operator's behavior unpredictable from the user's point
of view (the answer to "which CR is in charge?" depended on the kind)
and made cross-kind tooling impossible.

## Decision

Adopt one rule for every conflict-bearing kind: sort the candidate CRs
by `<namespace>/<name>` ASC and the LAST one wins. Loser CRs surface
`Ready=False, Reason=Conflict, Message="superseded by <ns>/<name>"`
and do not call LiteLLM.

`LiteLLMConnection` has no conflict surface. `LiteLLMModelDiscovery`
resolves conflicts at the Kubernetes layer via Server-Side Apply with
`ForceOwnership` — that is a deliberate exception, documented as such.

## Consequences

- Behavior becomes a pure function of `(kind, namespace, name)`. Users
  can predict the winner without inspecting LiteLLM state.
- Renaming/moving a CR is the single supported "I want this one to win"
  affordance.
- `LiteLLMTeam`'s historical smallest-`team_id` rule is removed: any CR
  set that relied on the rule will see a different team_alias get the
  authoritative spec. This is a breaking behavior change; called out in
  `CHANGELOG.md` and `docs/concepts/conflict-resolution.md`.
- `LiteLLMGuardRail`'s reject behavior becomes resolve-with-loser: a
  sibling no longer blocks adoption; the alphabetically-last CR wins.
- New metric `litellm_operator_conflicts_total{kind, role}` and new
  events `ConflictDetected` / `ConflictWon` give operators visibility
  into resolution churn.

## Alternatives considered

1. **First-create-wins** — depends on `creationTimestamp`, which races
   with the apiserver clock and is invisible in normal listings.
   Rejected.
2. **Smallest-CR-name-wins** — same shape, opposite direction. Equally
   valid; we chose last-wins so that adding a Z-prefixed CR is the
   obvious "take over" affordance.
3. **Explicit `spec.priority` field** — adds API surface and a new way
   to be wrong (ties). The alpha-by-name rule has no degenerate tie
   case because `(namespace, name)` is unique within the cluster.
4. **Reject all conflicts** (GuardRail's old behavior, generalized) —
   leaves the LiteLLM entity in an indeterminate state because the
   first CR to reach Ready is the de-facto owner. Rejected.
