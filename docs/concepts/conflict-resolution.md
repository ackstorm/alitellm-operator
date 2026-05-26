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

## Where the rule applies (and where it does not)

The operator is **namespace-scoped**: the manager's informer cache is
restricted to `WatchNamespace`. The apiserver enforces uniqueness on
`(namespace, metadata.name)`. So a cross-CR conflict only exists when
the LiteLLM-side natural key for a kind can collide between two CRs
**in the same namespace** — which requires the natural key to be
something other than `metadata.name`.

| Kind                         | Natural key on the LiteLLM side                                     | Conflict surface in namespace-scoped mode                                              |
|------------------------------|---------------------------------------------------------------------|----------------------------------------------------------------------------------------|
| `LiteLLMGuardRail`           | `spec.guardrailName` (user-set)                                     | Real. Two CRs can set the same guardrailName in one namespace.                         |
| `LiteLLMMCPServer`           | sanitized `metadata.name` (the LiteLLM `server_name`)               | Real. Sanitization (dot↔dash, depending on connection separator) can collapse two distinct `metadata.name` values onto the same LiteLLM `server_name`. |
| `LiteLLMMCPServerDiscovery`  | child `metadata.name` produced into `WatchNamespace`                | Real. Two Discovery CRs can produce the same child name.                               |
| `LiteLLMModelAlias`          | each `spec.aliases[].name` slot                                     | Real (slot-level). Aggregated last-wins per slot across all CRs.                       |
| `LiteLLMTeam`                | `metadata.name` (the operator forces `team_alias = metadata.name`)  | None in namespace-scoped mode (apiserver uniqueness forbids two CRs in one namespace). |
| `LiteLLMModel`               | `metadata.name`                                                     | None in namespace-scoped mode.                                                         |
| `LiteLLMA2AAgent`            | `metadata.name`                                                     | None in namespace-scoped mode.                                                         |
| `LiteLLMConnection`          | —                                                                   | No LiteLLM-side natural key; no conflict surface.                                      |
| `LiteLLMModelDiscovery`      | child `metadata.name` (Kubernetes-layer)                            | Resolved at the Kubernetes layer via Server-Side Apply with `ForceOwnership` — deliberate exception. |

Kinds with "None" rows above are listed for completeness; the resolver
is not wired on them because the conflict cannot occur.

## Why "last wins" and not "first wins"

A common alternative is first-create-wins. We chose last-by-name-wins
because it is **purely a function of `metadata.namespace` and
`metadata.name`** — values the user already controls and that are
visible in `kubectl get` listings. There is no hidden timestamp, no
ordering by `creationTimestamp`, no race with the apiserver clock. The
same CR set produces the same winner across operator restarts and CR
re-creations.

If you want a different CR to win, rename it so it sorts last.

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

- Two CRs of any kind with different `metadata.name` whose specs happen
  to describe the same upstream provider model or remote URL: not a
  conflict (different natural keys; LiteLLM stores them as separate
  entries).
- A `LiteLLMTeam` (or `LiteLLMModel`, `LiteLLMA2AAgent`) CR whose name
  matches an existing pre-LiteLLM entry created outside the operator:
  the operator adopts the existing entry on first reconcile via its
  per-kind adoption logic (LiteLLM `team_id` smallest-wins,
  `GetModelInfoByName`, `resolveAgentIDByName`, etc.). This is
  LiteLLM-side adoption, not cross-CR conflict.

## Why `LiteLLMModelDiscovery` is different

`LiteLLMModelDiscovery` does not use the alpha-last-wins rule. It
writes child `LiteLLMModel` CRs via Kubernetes Server-Side Apply with
`ForceOwnership`. Conflict resolution happens at the Kubernetes layer:
if a child CR is already owned by a different discovery (or by a
user), SSA either takes ownership (when `ForceOwnership` is asserted)
or rejects the write. The discovery-level reconciler raises
`Ready=False, Reason=ChildConflict` with a message identifying the
foreign owner.
