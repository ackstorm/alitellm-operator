# Deletion Semantics

When you delete a `LiteLLM*` custom resource, the operator runs a
finalizer that attempts to delete the corresponding entry from LiteLLM
(via a HTTP API call). If that call **cannot be confirmed** — because
LiteLLM is unavailable, the master key has been rotated and the
operator gets a 401, the network is partitioned, etc. — the operator
has to choose between two failure modes:

- **Orphan** (default): remove the finalizer anyway. The CR is freed,
  the Kubernetes garbage collector reaps it, and the cluster moves on.
  The LiteLLM-side entry may persist until the operator regains
  connectivity (next reconcile cycle).
- **Delete**: refuse to remove the finalizer until LiteLLM acks.
  The CR stays in `Terminating` indefinitely. Controller-runtime
  exponential backoff keeps retrying.

This is governed by `spec.deletionPolicy` on five CRD kinds:
`LiteLLMModel`, `LiteLLMTeam`, `LiteLLMMCPServer`, `LiteLLMA2AAgent`,
`LiteLLMGuardRail`. `LiteLLMConnection` is excluded — its finalizer
runs no LiteLLM HTTP call.

## Why two modes

The operator's default behavior is **Orphan**. This is REL-06's
"anti-storm" trade-off: if LiteLLM is down or auth has broken, the
cluster operator can still rip out workloads and re-apply later
without manual finalizer surgery. The downside is that LiteLLM may
end up with orphan entries (a model registered in LiteLLM whose
matching `LiteLLMModel` CR no longer exists).

For most users this is fine — orphan entries don't break anything;
they're just stale state that the next manual reconcile or cleanup
will catch.

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
the spec. The finalizer drains. The annotation can be removed once
the incident is resolved.

The annotation accepts the same values as the field (`Orphan` |
`Delete`). Any other value is silently ignored.

## Discovery-owned children always Orphan

Children created by `LiteLLMModelDiscovery` or
`LiteLLMMCPServerDiscovery` have a controller-owner reference back to
their Discovery parent. For those children, the resolver **forces
Orphan** regardless of `spec.deletionPolicy` or annotation.

Why: Discovery's vanish-detection works by deleting child CRs when
the upstream source (Bedrock, OpenAI, ToolHive, ...) no longer
reports the entry. If a child were `Delete`-policy and LiteLLM
became unreachable, the child's finalizer would never drain,
vanish-detection would deadlock, and Discovery itself would get
stuck.

If you want strict deletion of Discovery-managed entries, delete the
**parent** Discovery CR (which has its own deletion policy logic) —
not the child.

## Trade-off summary

| Property | `Orphan` (default) | `Delete` |
|----------|-------------------|----------|
| Finalizer drains when LiteLLM is unavailable | Yes | No |
| LiteLLM may end up with orphan entries | Yes | No |
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

When an Orphan-policy CR drops a finalizer without ack:

1. Increments the `alitellm_operator_deletion_orphaned_total{kind}` counter.
2. Emits a `LiteLLMDeleteOrphaned` Normal Event on the CR.
3. Returns nil from the reconcile so the CR is garbage-collected.

Both events name the underlying reason (e.g. `401 on DeleteModel`,
`LiteLLM unavailable`) so you can correlate with operator logs.

## Recovery procedures

**A CR is stuck in `Terminating` under `Delete` and LiteLLM is permanently unreachable.**

1. Check the events: `kubectl describe litellmmodel my-model | grep -A 2 LiteLLMDeleteBlocked`
2. Confirm LiteLLM unavailability is intentional/permanent (not a transient blip).
3. Apply the annotation override:
   ```bash
   kubectl annotate litellmmodel my-model \
     litellm.ackstorm.ai/deletion-policy-override=Orphan
   ```
4. The finalizer drains on the next reconcile (within seconds).
5. Remove the annotation if/when LiteLLM comes back, so future deletions honor `Delete` again.

**A LiteLLM entry was orphaned and you need to clean it up.**

The operator does NOT re-discover orphans automatically (Discovery
sources are upstream catalogs, not LiteLLM itself). Clean up directly
via LiteLLM's admin API or UI.
