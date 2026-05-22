# MCPServerDiscovery event-driven Watches on ToolHive sources Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Cut MCPServerDiscovery latency for picking up ToolHive MCPServer (and VirtualMCPServer) source events from up to one `spec.refresh.interval` (1m floor, CEL-enforced) down to single-digit seconds. Add controller-runtime event-driven watches on the four ToolHive GVKs (`MCPServer` + `VirtualMCPServer` × `v1alpha1` + `v1beta1`) and enqueue affected `LiteLLMMCPServerDiscovery` CRs on each source Add/Update/Delete.

**Architecture:** The current `MCPServerDiscoveryReconciler` watches `For(&LiteLLMMCPServerDiscovery{})` and `Owns(&LiteLLMMCPServer{})`, and re-enqueues itself periodically via `ctrl.Result{RequeueAfter: spec.refresh.interval}`. It has **no event subscription on the source ToolHive objects**, so when a ToolHive MCPServer is created (or deleted) in a namespace listed in some `LiteLLMMCPServerDiscovery.spec.toolhive.namespaces`, no reconcile fires until the next `refresh.interval` tick. The CEL floor on `refresh.interval` is `1m` (production rate-limit), so p99 discovery latency is `~1m`.

The existing `internal/toolhive.Informer` already registers controller-runtime dynamic informers (`mgr.GetCache().GetInformer(ctx, u)`) for the four ToolHive GVKs as part of `mgr.Start`. The cache is shared between the informer and the controller client, so we can attach `Watches()` directly using `&unstructured.Unstructured{}` typed with the right GVK.

The catch: `Watches(&unstructured.Unstructured{}, ...)` against a CRD that is *absent at manager start* fails the manager bootstrap (the informer can't register, manager refuses to start). The existing `toolhive.Informer.retryLoop` is the project's blessed pattern for "ToolHive CRDs may be absent" — we must NOT regress that. Two viable shapes:

- **Shape A (rejected):** four direct `Watches(&unstructured...)` in `SetupWithManager`. Hard-fails if any ToolHive CRD is absent at startup. Regresses MSDISC-09 "ToolHive CRDs may be absent" invariant. ❌
- **Shape B (chosen):** extend `toolhive.Informer` to register a `cache.ResourceEventHandler` on each successfully-registered informer; the handler pushes `reconcile.Request` objects onto a `chan event.GenericEvent` (or `chan reconcile.Request`). The MSDisc controller subscribes via `WatchesRawSource(source.Channel(ch, handler))`. The channel is owned by `*Informer` and survives partial CRD presence — only kinds that registered successfully emit events; the others stay silent until the retry loop succeeds, at which point their handlers also start emitting.

Shape B reuses the existing `WatchesRawSource(source.Channel(...))` pattern already used by `LiteLLMConnectionReconciler` (see `internal/controller/litellmconnection_controller.go:508`) so the contract is familiar and lint-clean.

**Tech Stack:** Go 1.24, controller-runtime v0.19.4, k8s.io/* v0.31.0. Devtools container via `./scripts/dev.sh`.

---

### Task 1: Audit current state and define the channel contract

**Files:**
- Read: `internal/controller/mcpserverdiscovery_controller.go:900-921` (current `SetupWithManager`)
- Read: `internal/toolhive/informer.go:198-364` (Informer struct + `tryRegister`)
- Read: `internal/controller/litellmconnection_controller.go:475-511` (existing Channel-source pattern)

**Step 1:** Confirm no other controller currently consumes ToolHive source events. The change must be additive only (MSDisc subscriber; no new producers outside `toolhive.Informer`).

```
./scripts/dev.sh bash -c 'cd /workspace && grep -rn "toolhive.Informer\b\|toolhive\.MCPServerGVK\|toolhive\.VirtualMCPServerGVK" internal/'
```

Expected: only `cmd/main.go` (constructor), `internal/controller/mcpserverdiscovery_controller.go` (consumer of `.List` + `.IsReady`), `internal/toolhive/*.go` (the package itself). If anything else surfaces, expand scope to inventory all consumers before changing the API.

**Step 2:** Decide channel element type. Options:
- `chan event.GenericEvent` — controller-runtime native; carries `client.Object`. Plays cleanly with `source.Channel(ch, handler.EnqueueRequestsFromMapFunc(mapFn))`.
- `chan reconcile.Request` — pre-resolved; bypasses the mapFunc. Tighter but moves the namespace→discovery-CR fan-out into `Informer`, which is the wrong layer (the informer doesn't know the set of MSDiscs).

**Chosen:** `chan event.GenericEvent`. The mapFunc (MSDisc-aware) lives in the controller, not the informer.

**Step 3:** Decide buffer size. The handler is called from cache reflector goroutines; a blocked send would back up the cache. Default `make(chan event.GenericEvent, 1024)` matches controller-runtime's internal queue sizing. Verify by reading `vendor/sigs.k8s.io/controller-runtime/pkg/source/channel.go` for any hard-coded expectation.

---

### Task 2: Plumb a `chan event.GenericEvent` through `toolhive.Informer`

**Files:**
- Edit: `internal/toolhive/informer.go`

**Step 1:** Add a new exported field/accessor to `Informer`:

```go
type Informer struct {
    // ... existing fields
    events    chan event.GenericEvent
    eventsMu  sync.Mutex // protects against double-registration if tryRegister fires multiple times
}

func (i *Informer) Events() <-chan event.GenericEvent {
    return i.events
}
```

Initialize `events` lazily in `Start` (or eagerly in a constructor) with `make(chan event.GenericEvent, 1024)`.

**Step 2:** In `tryRegister`, after each successful `mgr.GetCache().GetInformer(ctx, u)`, register a `cache.ResourceEventHandler` on the returned informer that pushes Add/Update/Delete events onto `i.events`.

Use the `i.kindReady[kind]` flag to **guard against double-registration**: if the kind has just transitioned from unready→ready, register the handler once; if it was already ready, skip. The existing dual-version logic ("one version ready ⇒ skip the other") means we register the handler only on the first successful version per Kind.

Wait — actually, **both** versions should emit events. The current List() aggregates from both v1alpha1 and v1beta1; symmetric event delivery preserves the v1alpha1-wins dedup invariant. So handler-register per-version, NOT per-kind. Need a second flag, e.g. `i.handlerInstalled[gvk]`.

Pseudocode:

```go
for _, gvk := range gvks {
    if i.handlerInstalled[gvk] {
        continue
    }
    u := &unstructured.Unstructured{}
    u.SetGroupVersionKind(gvk)
    inf, err := i.Manager.GetCache().GetInformer(ctx, u)
    if err != nil { /* unchanged */ continue }

    if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
        AddFunc:    func(obj interface{}) { i.push(obj) },
        UpdateFunc: func(_, newObj interface{}) { i.push(newObj) },
        DeleteFunc: func(obj interface{}) { i.push(obj) },
    }); err != nil {
        i.Log.V(1).Info("toolhive event handler install failed", "gvk", gvk.String(), "err", err.Error())
        continue
    }
    i.handlerInstalled[gvk] = true
    // ... existing readiness bookkeeping
}
```

`push` performs the type assertion to `client.Object` and a non-blocking send (drop if channel full, log V(1)). Channel-full = backpressure; better to log+drop than block the reflector.

**Step 3:** Predicate guard. ResyncPeriod = 0 in controller-runtime cache by default, so AddEventHandler doesn't emit synthetic resync events. Good. But on initial cache fill (informer warm-up after CRD becomes available), every existing object fires an Add. That's correct behavior — we want to reconcile MSDiscs to pick up sources that already existed.

**Acceptance for Task 2:** `Informer.Events()` returns a channel that emits `GenericEvent` for every Add/Update/Delete on the registered ToolHive GVKs, scoped to all namespaces the manager's cache is configured to watch.

---

### Task 3: Wire MSDisc controller to consume the Informer's event channel

**Files:**
- Edit: `internal/controller/mcpserverdiscovery_controller.go`

**Step 1:** Define `toolhiveSourceToDiscoveries` mapFunc on `*MCPServerDiscoveryReconciler`. Signature: `func(ctx context.Context, obj client.Object) []reconcile.Request`. Behavior:

1. List all `LiteLLMMCPServerDiscovery` CRs (cluster-scoped list via controller's cached client). N is small (operator-managed CRs), no indexer needed initially.
2. For each MSDisc whose `spec.toolhive.namespaces` contains `obj.GetNamespace()` AND whose `spec.toolhive.kinds` contains `obj.GetObjectKind().GroupVersionKind().Kind` (`MCPServer` or `VirtualMCPServer`), emit `{Namespace, Name}`.
3. Empty `spec.toolhive.namespaces` semantics: if the convention is "all namespaces", honor it; verify against `internal/controller/mcpserverdiscovery_controller.go` reconciler logic before implementing.
4. Return `nil` if no MSDisc CR matches — cheap no-op.

**Step 2:** Wire the Channel source into `SetupWithManager`:

```go
return ctrl.NewControllerManagedBy(mgr).
    For(&litellmv1alpha1.LiteLLMMCPServerDiscovery{}, builder.WithPredicates()).
    Owns(&litellmv1alpha1.LiteLLMMCPServer{}).
    WatchesRawSource(
        source.Channel(r.ToolHiveInformer.Events(), handler.EnqueueRequestsFromMapFunc(r.toolhiveSourceToDiscoveries)),
    ).
    WithOptions(transientBackoffOptions()).
    Named("mcpserverdiscovery").
    Complete(r)
```

**Step 3:** Reconciler dependency. The reconciler already has a `ToolHiveInformerReader` interface for `.List` + `.IsReady`. Either:
- (i) Extend the interface with `Events() <-chan event.GenericEvent`. Cleanest.
- (ii) Keep `ToolHiveInformerReader` minimal; add a second field `ToolHiveEvents <-chan event.GenericEvent` on the reconciler struct, populated by `cmd/main.go`.

Prefer (i) — single dependency.

**Step 4:** Predicate to filter out resourceVersion-only updates? Probably YES for hot clusters but the events channel is the natural place — push a predicate-filtered handler in Task 2 if log review during validation shows noisy spurious events.

---

### Task 4: Envtest coverage for the new path

**Files:**
- Edit: `internal/controller/mcpserverdiscovery_controller_test.go`

**Cases (all envtest, race on):**

1. **source-create → MSDisc reconcile within 5s:** create MSDisc CR (interval 5m to suppress periodic-tick noise), then create a ToolHive MCPServer in a watched namespace, assert the child `LiteLLMMCPServer` exists within 5s (not the 5m interval).

2. **source-delete → MSDisc cascade within 5s:** with a child present, delete the ToolHive source, assert child vanishes within 5s.

3. **source in non-watching namespace → no requeue:** MSDisc watches `nsA`. Create source in `nsB`. Assert no child appears within 5s and no reconcile fires (count requeues via metric or instrumented mapFunc).

4. **two MSDiscs share a namespace → both enqueued:** MSDisc-1 and MSDisc-2 both watch `nsA`. One source created in `nsA`. Both MSDiscs reconcile.

5. **CRD-absent at start → manager starts cleanly, no panic:** strip the v1beta1 ToolHive CRD from the envtest manifests, start manager, assert it reaches Ready and `Informer.IsReady` is true (v1alpha1 ready). Create a v1alpha1 source; MSDisc reconciles.

6. **Backpressure: 2000 source events in burst:** push 2000 synthetic events through the channel (e.g. via direct `Events()` write — exposed test-only helper); assert no reflector starvation (channel-full log present, no manager hang).

**Pattern reuse:** envtest already exercises `MCPServerDiscoveryReconciler`; reuse the existing harness in `internal/controller/mcpserverdiscovery_controller_test.go`.

---

### Task 5: Remove the e2e `tickleMSDisc` workaround

**Files:**
- Edit: `test/e2e/mcpserverdiscovery_test.go`

The e2e workaround landed in `2026-05-22` to force out-of-band reconciles via annotation patches inside three Eventually blocks. Once the event-driven path is verified by the new envtest cases (Task 4), the tickle is dead code.

**Step 1:** Run e2e WITHOUT the tickle to confirm the new watches deliver in single-digit seconds.

**Step 2:** Delete `tickleMSDisc` helper and the three `tickleMSDisc(dyn, ..., ourNs)` calls. Drop the `"fmt"`, `"k8s.io/apimachinery/pkg/types"`, `"k8s.io/client-go/dynamic"` imports that became orphaned.

**Step 3:** Run `./scripts/dev.sh make e2e-full` and confirm the 3 MSDisc specs now complete in ≤5s each (was ~60s).

---

### Task 6: Documentation + acceptance gates

**Files:**
- Edit: `CLAUDE.md` — under "Common failure modes", add an entry pointing future readers at this watches path if they investigate MSDisc latency questions.
- Edit: `internal/controller/mcpserverdiscovery_controller.go` — update the `SetupWithManager` doc comment to reflect the new `WatchesRawSource(source.Channel(...))`.
- Edit: `api/litellm/v1alpha1/mcpserverdiscovery_types.go` — update the `refresh.interval` doc comment to note that the interval is now a *bounded ceiling* (event-driven path is faster); the CEL floor is unchanged.

**Acceptance gates (CLAUDE.md "Test phases"):**

- `./scripts/dev.sh make unit` — green
- `./scripts/dev.sh make envtest-run` — green (covers Task 4 cases)
- `./scripts/dev.sh make e2e-full` — green; the three MSDisc specs each complete in ≤5s
- `make pre-push` — clean (no SPDX/license/gitleaks/govulncheck drift)

**Out of scope (explicit non-goals):**

- Lowering the CEL `refresh.interval` floor below 1m. Production rate-limit semantics unchanged.
- Watches on `LiteLLMModelDiscovery` sources (separate phase if needed; same shape can be reused).
- A controller-runtime indexer on `spec.toolhive.namespaces` (current mapFunc is O(N) over MSDisc CRs — fine for operator-managed N).

---

### Risk register

| Risk | Mitigation |
|---|---|
| Channel send blocks reflector goroutine | Non-blocking send; log+drop on full, instrumented in Task 2 |
| Manager fails to start with v1beta1 CRD absent | Channel-source pattern avoids direct `Watches(&unstructured...)`; informer retry loop handles delayed CRD availability — covered by Task 4 case 5 |
| MapFunc fires reconciles for every source event | Acceptable: MSDisc reconciler is idempotent + cheap. If hot, add a predicate to skip resourceVersion-only Updates |
| Double-registered event handlers on retry | `handlerInstalled[gvk]` guard in Task 2 |
| Existing 1m periodic requeue + new event path causes double reconciles | The reconciler is idempotent; controller-runtime workqueue dedups bursts. Verify in Task 4 case 6 |

### Estimate

- Task 1: 30 min (audit)
- Task 2: 1-2 h (Informer event channel + handler install + tests)
- Task 3: 1 h (controller wiring + mapFunc)
- Task 4: 2-3 h (envtest cases — case 5 needs envtest CRD manipulation)
- Task 5: 30 min (e2e cleanup + verify)
- Task 6: 30 min (docs)

**Total: 6-8 h focused work.**
