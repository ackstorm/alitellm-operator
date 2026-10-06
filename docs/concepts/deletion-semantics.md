# Deletion Semantics

When you delete a `LiteLLM*` custom resource, the operator runs a
finalizer that attempts to delete the corresponding entry from LiteLLM
(via a HTTP API call). If that call **cannot be confirmed** — because
LiteLLM is unavailable, the master key has been rotated and the
operator gets a 401, the network is partitioned, etc. — the operator
has to choose between two failure modes:

- **Orphan** (default): give up on the LiteLLM delete only when the
  cause is **permanent** — the `LiteLLMConnection` is being deleted
  (connection reason `Absent`, e.g. namespace teardown) or LiteLLM
  answered a deterministic `4xx` that retrying cannot fix. The finalizer
  is removed and the LiteLLM entry may persist. When the cause is
  **transient** — LiteLLM unreachable or still connecting, a bad master
  key, any `401` — the finalizer is kept and the delete is retried with
  backoff (capped at 30s), so it lands as soon as LiteLLM is back.
- **Delete**: refuse to remove the finalizer until LiteLLM acks,
  whatever the cause. The CR stays in `Terminating` indefinitely.
  Controller-runtime exponential backoff keeps retrying.

A confirmed-absent entry (`404`, or a name lookup that finds nothing)
drains under either policy: the goal is already met.

A `401` on the delete call is retried with per-CR exponential backoff
(capped at 30s). It does not invalidate the connection: the connection
probe alone decides whether the master key is bad, and once it does,
deferred deletes make no LiteLLM calls at all.

This is governed by `spec.deletionPolicy` on seven CRD kinds:
`LiteLLMModel`, `LiteLLMTeam`, `LiteLLMMCPServer`, `LiteLLMA2AAgent`,
`LiteLLMGuardRail`, `LiteLLMMCPToolset`, `LiteLLMAccessGroup`. `LiteLLMConnection` is excluded — its finalizer
runs no LiteLLM HTTP call.

## Why two modes

The operator's default behavior is **Orphan**: deleting the
`LiteLLMConnection` or the namespace never wedges on LiteLLM, and a
deterministic rejection never retries forever. No LiteLLM HTTP call is
made while the connection is not usable, so retrying a deferred delete
does not storm LiteLLM (REL-06).

Transient causes are retried rather than orphaned because nothing ever
revisits an orphaned row: a LiteLLM outage of 7.5h once let a Discovery
vanish-delete a child mid-outage, and the LiteLLM model row leaked with
no CR left to clean it up.

**Trade-off:** during a LiteLLM outage, deleted CRs stay `Terminating`
until LiteLLM is reachable again, and a Discovery being deleted waits
for its children. If you must unblock a CR before LiteLLM recovers
(accepting that its LiteLLM entry may leak), remove the finalizer by
hand:

```bash
kubectl patch litellmmodel my-model --type=merge -p '{"metadata":{"finalizers":[]}}'
```

For **GitOps users** (Argo CD, Flux), Orphan is the wrong default.
GitOps tooling assumes that "CR gone from cluster" implies "resource
gone from backing API". If LiteLLM keeps the entry alive after the CR
is removed, Argo will happily mark the application as `Synced` while
the LiteLLM proxy is still serving traffic for a model that no longer
appears in Git. That's a silent compliance and observability gap.

For GitOps, **Delete** is the right default.

## The annotation break-glass

`spec.deletionPolicy` is part of the desired state in Git. If a
production incident leaves LiteLLM permanently unreachable while
several CRs are stuck in `Terminating`, you don't want to have to push
a Git commit changing `Delete` to `Orphan` (and wait for the GitOps
sync) just to unstick the cluster.

The annotation override gives you a runtime escape hatch that
**does not mutate spec**:

```bash
kubectl annotate litellmmodel my-model \
  litellm.ackstorm.ai/deletion-policy-override=Orphan
```

On the next reconcile, the resolver sees the annotation and overrides
the spec. The CR then follows Orphan: it drains once the cause is
permanent (connection `Absent`, deterministic `4xx`). During a
transient outage Orphan also waits, so the annotation alone does not
unstick it — use the finalizer patch above. The annotation can be
removed once the incident is resolved.

The annotation accepts the same values as the field (`Orphan` |
`Delete`). Any other value is silently ignored.

## Discovery-owned children always Orphan

Children created by `LiteLLMModelDiscovery` or
`LiteLLMMCPServerDiscovery` have a controller-owner reference back to
their Discovery parent. For those children, the resolver **forces
Orphan** regardless of `spec.deletionPolicy` or annotation.

Why: Discovery's vanish-detection works by deleting child CRs when
the upstream source (Bedrock, OpenAI, ToolHive, ...) no longer
reports the entry. A `Delete`-policy child would wedge forever on a
deterministic rejection or a deleted connection. Under Orphan a child
vanish-deleted during a LiteLLM outage stays `Terminating` until
LiteLLM recovers, then its LiteLLM row is deleted.

If you want strict deletion of Discovery-managed entries, delete the
**parent** Discovery CR (which has its own deletion policy logic) —
not the child.

## Trade-off summary

| Property | `Orphan` (default) | `Delete` |
|----------|-------------------|----------|
| Finalizer drains when LiteLLM is transiently unavailable | No (retries) | No |
| Finalizer drains when the connection is deleted / 4xx | Yes | No |
| LiteLLM may end up with orphan entries | Only on a permanent cause | No |
| GitOps tooling correctly reports synced state | No | Yes |
| Risk of CR stuck in `Terminating` | Low | High |
| Recovery requires manual action | No | Annotation flip |

## Observability

When a Delete-policy CR is blocked on a missing ack, the operator:

1. Increments the `alitellm_operator_deletion_blocked{kind,namespace,name}` gauge.
2. Emits a `LiteLLMDeleteBlocked` Warning Event on the CR (rate-
   limited by the event recorder's default dedup window).
3. Returns an error from the reconcile so controller-runtime requeues
   with exponential backoff.

When an Orphan-policy CR defers its delete on a transient cause:

1. Sets the same `alitellm_operator_deletion_blocked` gauge.
2. Emits a `LiteLLMDeleteDeferred` Normal Event on the CR.
3. Returns an error so controller-runtime retries with backoff.

When an Orphan-policy CR drops a finalizer without ack (permanent cause):

1. Increments the `alitellm_operator_deletion_orphaned_total{kind}` counter.
2. Emits a `LiteLLMDeleteOrphaned` Normal Event on the CR.
3. Returns nil from the reconcile so the CR is garbage-collected.

Both events name the underlying reason (e.g. `401 on DeleteModel`,
`LiteLLM unavailable (reason "Unreachable")`) so you can correlate with
operator logs.

## Recovery procedures

**A CR is stuck in `Terminating` under `Delete` and LiteLLM is permanently unreachable.**

1. Check the events: `kubectl describe litellmmodel my-model | grep -A 2 LiteLLMDeleteBlocked`
2. Confirm LiteLLM unavailability is intentional/permanent (not a transient blip).
3. Remove the finalizer (the LiteLLM entry may persist; clean it up
   later through LiteLLM's admin API or UI):
   ```bash
   kubectl patch litellmmodel my-model --type=merge -p '{"metadata":{"finalizers":[]}}'
   ```
   The `deletion-policy-override=Orphan` annotation is not enough here:
   Orphan also waits out an unreachable LiteLLM.

**A LiteLLM entry was orphaned and you need to clean it up.**

The operator does NOT re-discover orphans automatically (Discovery
sources are upstream catalogs, not LiteLLM itself). Clean up directly
via LiteLLM's admin API or UI.
