# Conflict resolution

The operator runs one reconciler per CRD, but multiple CRs of the same
kind can describe the same LiteLLM-side entity. When two or more CRs
share the same **natural key** for their kind, the operator applies a
single, deterministic rule.

## The rule

> Sort the colliding CRs by `<namespace>/<name>` in lexicographic
> ascending order. The **last** CR in that order wins. Every other
> ("loser") CR stops calling LiteLLM and surfaces:
>
> ```
> status.conditions:
>   - type: Ready
>     status: "False"
>     reason: Conflict
>     message: superseded by <winner-namespace>/<winner-name>
> ```

The rule is the same across every kind that has a conflict surface
(`LiteLLMTeam`, `LiteLLMModel`, `LiteLLMMCPServer`, `LiteLLMA2AAgent`,
`LiteLLMGuardRail`, `LiteLLMMCPServerDiscovery`). `LiteLLMModelAlias`
already aggregates this way per alias slot. `LiteLLMConnection` has no
conflict surface. `LiteLLMModelDiscovery` is owned by Kubernetes SSA
with `ForceOwnership` and resolves conflicts at the K8s layer instead.

## Why "last wins" and not "first wins"

A common alternative is first-create-wins. We chose last-by-name-wins
because it is **purely a function of `metadata.namespace` and
`metadata.name`** — values the user already controls and that are
visible in `kubectl get` listings. There is no hidden timestamp, no
ordering by `creationTimestamp`, no race with the apiserver clock. The
same CR set produces the same winner across operator restarts and CR
re-creations.

If you want a different CR to win, rename it (or move it to a namespace)
so it sorts last.

## Natural keys per kind

| Kind                         | Natural key on the LiteLLM side                                |
|------------------------------|----------------------------------------------------------------|
| `LiteLLMTeam`                | `spec.params.team_alias` (defaults to `metadata.name`)         |
| `LiteLLMModel`               | `metadata.name` (the LiteLLM `model_name`)                     |
| `LiteLLMMCPServer`           | sanitized `metadata.name` (the LiteLLM `server_name`)          |
| `LiteLLMA2AAgent`            | `metadata.name` (the LiteLLM agent name)                       |
| `LiteLLMGuardRail`           | `spec.guardrailName`                                           |
| `LiteLLMMCPServerDiscovery`  | child `metadata.name` derived from the upstream object         |
| `LiteLLMModelAlias`          | each entry in `spec.aliases[].name` (slot-level, last-wins)    |

## Recovery

When the current winner is deleted (or renamed so it no longer
collides), the operator re-evaluates the candidate set. The next CR in
sort order is promoted, its `Conflict` condition is cleared, and a
normal reconcile drives the LiteLLM entity to its spec.

## Observability

- Loser CRs emit a `ConflictDetected` Kubernetes Event with the winner key.
- Winner CRs emit a `ConflictWon` Event when they take over after a
  previous winner left the set.
- The metric `litellm_operator_conflicts_total{kind, role}` increments
  with `role=loser` on every transition into Conflict and `role=winner`
  on every promotion.

## What is **not** a conflict

- Two `LiteLLMModel` CRs with different `metadata.name` that happen to
  point at the same upstream provider model: no conflict (different
  natural keys; LiteLLM stores them as separate entries).
- A `LiteLLMTeam` CR whose `metadata.name` matches an existing pre-LiteLLM
  team that was created outside the operator: the operator adopts the
  existing team on first reconcile (same key, no sibling CR), then the
  resolver only kicks in when a second CR appears.
