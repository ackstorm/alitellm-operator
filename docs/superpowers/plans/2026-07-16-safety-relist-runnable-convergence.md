# Safety-Relist Runnable Convergence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `SafetyRelistRunnable` the single periodic drift-detection mechanism for all five domain reconcilers, and delete every `RequeueAfter` safety-relist path.

**Architecture:** Today two mechanisms coexist. `RequeueAfter` re-enqueues a CR from inside `Reconcile`, so it only fires if the reconcile *reaches a return site that carries it* — every early return is a hole in the safety net, which is precisely the failure family of issue #102 (a CR wedged at a return site with no requeue can never re-tick itself). `SafetyRelistRunnable` ticks from *outside* the reconcile on a `time.Ticker`, lists every CR of one kind, and enqueues them — it cannot be defeated by a bad branch. Model and GuardRail already have Runnables; Team, MCPServer and A2AAgent do not. This plan adds the three missing Runnables, then removes all 10 `RequeueAfter` sites so exactly one mechanism remains.

**Tech Stack:** Go 1.26.4, controller-runtime v0.19.4, k8s.io/* v0.31.0, envtest, Ginkgo v2 (e2e only).

## Global Constraints

- Every `*.go` file outside `vendor/`, `zz_generated*.go`, `mock_*.go` starts with `// SPDX-License-Identifier: Apache-2.0`. Pre-push gate 15 enforces.
- Host has NO Go toolchain. Run `make` targets **bare** — they self-route into the devtools container via `container_target`. Never prefix with `./scripts/dev.sh`.
- Absolute paths for all Edit/Write. Sibling repos with similar layouts live next to this one.
- No naked polling loops. Use blessed `wait-*` targets or bounded `timeout`/`for i in $(seq ...)` forms.
- Never `git push --no-verify`. The pre-push gate is the contract.
- `LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL` keeps its **exact current user-facing meaning** ("vanish-probe cadence per reconciler kind", floor 5s, default 10m). This plan changes only *which internal machinery* consumes it. Helm value `safetyRelistInterval` and its docs rows stay valid.
- `make test-envtest` on a resource-starved host can fail at suite SETUP (`WaitForCacheSync: did not sync within 30s`). That is host starvation, not a regression — verify on CI's Envtest job.

## Non-Goals (deliberate, do not "fix" these)

- **Tick spread / thundering herd.** A Runnable tick enqueues N requests at once, where `RequeueAfter` + `withJitter` self-spread per-CR. This is accepted: the workqueue de-duplicates by key, and actual concurrency is bounded by `MaxConcurrentReconciles`, so the burst is queue *depth*, not a burst of LiteLLM HTTP calls. Model and GuardRail have shipped this shape in production since v0.4.x. Do not add a `Spread` knob speculatively — measure first. If it ever matters, the upgrade path is `q.AddAfter(req, perItemOffset)` in the raw-source consumer, not a new field on the Runnable.
- **Leader election semantics.** `mgr.Add` already treats Runnables as leader-only. Unchanged.
- **The `Gate` field.** Production leaves it nil; the envtest suite sets it. Unchanged (#74).

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/controller/shared_helpers.go` | `SafetyRelistRunnable` (shared tick loop) | Modify: blocking send |
| `internal/controller/safety_relist.go` | env parsing + interval vars | Modify: delete `SetSafetyRelistIntervals`, fix stale doc |
| `internal/controller/team_controller.go` | Team reconciler | Modify: add `ListTeamRequests`, drop 2 requeues + interval var |
| `internal/controller/mcpserver_controller.go` | MCPServer reconciler | Modify: add `ListMCPServerRequests`, channel param, drop 3 requeues + interval var |
| `internal/controller/a2aagent_controller.go` | A2AAgent reconciler | Modify: add `ListA2AAgentRequests`, channel param, drop 2 requeues + interval var |
| `internal/controller/model_controller.go` | Model reconciler | Modify: drop 3 requeues + interval var |
| `internal/controller/predicates.go` | shared predicates + `withJitter` | Modify: delete `withJitter` (becomes dead) |
| `internal/controller/predicates_withjitter_test.go` | `withJitter` unit test | **Delete** |
| `internal/controller/safety_relist_test.go` | env-parse tests | Modify: add Runnable no-drop test, drop `SetSafetyRelistIntervals` test |
| `internal/controller/suite_test.go` | envtest bootstrap | Modify: wire 3 new gated Runnables |
| `cmd/main.go` | production wiring | Modify: add 3 Runnables, drop `SetSafetyRelistIntervals` call |
| `CLAUDE.md` | agent reference | Modify: failure-mode entry + pattern note |
| `docs/developer-guide/architecture.md` | public docs | Modify: mechanism description |

**Channel reuse (important):** Team does **not** get a new channel. `cmd/main.go` already creates `teamDefaultRequeueCh` for `TeamDefaultRunnable` and passes it to `TeamReconciler.SetupWithManager`. Two producers on one channel is fine — the single consumer goroutine `q.Add()`s everything. MCPServer and A2AAgent get new channels.

---

### Task 1: Make the Runnable's enqueue lossless

`SafetyRelistRunnable.Start` sends with `select { case ch <- req: default: }` — a **silent drop** when the channel is full. With `cap=256` and >256 CRs of one kind, items are dropped every tick. Today Model and GuardRail already carry this; converging three more kinds onto the Runnable would extend it. Fix before converging.

**Files:**
- Modify: `internal/controller/shared_helpers.go` (the `for _, req := range reqs` loop inside `Start`)
- Test: `internal/controller/safety_relist_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces: `SafetyRelistRunnable.Start(ctx context.Context) error` — unchanged signature, now blocks on a full `RequeueCh` instead of dropping, and returns `nil` on ctx cancel.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/safety_relist_test.go`. The `ListRequests` stub returns its three items **only on the first tick**, then empty — so a dropped item is dropped *forever* and the test fails deterministically rather than being papered over by the next tick's retry.

```go
// TestSafetyRelistRunnable_EnqueueIsLossless — the tick loop must not drop
// items when RequeueCh is full. ListRequests yields its batch exactly once,
// so any dropped item is never re-offered and the receive below times out.
func TestSafetyRelistRunnable_EnqueueIsLossless(t *testing.T) {
	reqs := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: "a", Namespace: "ns"}},
		{NamespacedName: types.NamespacedName{Name: "b", Namespace: "ns"}},
		{NamespacedName: types.NamespacedName{Name: "c", Namespace: "ns"}},
	}
	var served atomic.Bool
	ch := make(chan reconcile.Request, 1) // cap 1 < len(reqs): forces the full-channel path

	r := &SafetyRelistRunnable{
		Interval:  10 * time.Millisecond,
		Log:       logr.Discard(),
		RequeueCh: ch,
		ListRequests: func(_ context.Context, _ client.Client, _ string) ([]reconcile.Request, error) {
			if served.CompareAndSwap(false, true) {
				return reqs, nil
			}
			return nil, nil
		},
		LogLabel: "test",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	got := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(got) < len(reqs) {
		select {
		case req := <-ch:
			got[req.Name] = true
		case <-deadline:
			t.Fatalf("lossy enqueue: got %d/%d requests (%v)", len(got), len(reqs), got)
		}
	}
}
```

Ensure these imports exist in the file: `context`, `sync/atomic`, `testing`, `time`, `github.com/go-logr/logr`, `k8s.io/apimachinery/pkg/types`, `sigs.k8s.io/controller-runtime/pkg/client`, `sigs.k8s.io/controller-runtime/pkg/reconcile`.

- [ ] **Step 2: Run test to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestSafetyRelistRunnable_EnqueueIsLossless`

Expected: `FAIL` — `lossy enqueue: got 1/3 requests`.

- [ ] **Step 3: Write minimal implementation**

In `internal/controller/shared_helpers.go`, inside `Start`, replace:

```go
			for _, req := range reqs {
				select {
				case r.RequeueCh <- req:
				default:
					// Channel full — skip this item; retried on next tick.
				}
			}
```

with:

```go
			for _, req := range reqs {
				select {
				case r.RequeueCh <- req:
				case <-ctx.Done():
					return nil
				}
			}
```

Also update the `SafetyRelistRunnable` doc comment above the type: replace the phrase `non-blockingly enqueues their reconcile.Requests on RequeueCh (a full channel skips the item — the next tick retries)` with:

```go
// enqueues their reconcile.Requests on RequeueCh. The send blocks until the
// consumer drains or ctx is cancelled — never drops. A silent drop here is
// invisible drift (the safety net skipping the very CR it exists to catch),
// and the consumer installed by SetupWithManager only does a non-blocking
// q.Add, so it drains continuously and the block is transient.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestSafetyRelistRunnable_EnqueueIsLossless`

Expected: `PASS`.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/shared_helpers.go internal/controller/safety_relist_test.go
git commit -m "fix(relist): make SafetyRelistRunnable enqueue lossless"
```

---

### Task 2: Add the three missing `List*Requests` functions

`ListModelRequests` and `ListGuardRailRequests` exist. Team, MCPServer and A2AAgent need identical mirrors so their Runnables have a `ListRequests` to call.

**Files:**
- Modify: `internal/controller/team_controller.go` (append at end)
- Modify: `internal/controller/mcpserver_controller.go` (append at end)
- Modify: `internal/controller/a2aagent_controller.go` (append at end)
- Test: `internal/controller/safety_relist_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces (all match the `SafetyRelistRunnable.ListRequests` field type
  `func(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error)`):
  - `ListTeamRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error)`
  - `ListMCPServerRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error)`
  - `ListA2AAgentRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/safety_relist_test.go`. Uses the envtest `k8sClient` + `WatchNamespace` already provided by the suite.

```go
// TestListRequests_CoversEveryKind — each List*Requests must return one
// reconcile.Request per CR of its kind in the namespace. These feed the
// SafetyRelistRunnable; a kind missing from the list is a kind with no
// safety net.
func TestListRequests_CoversEveryKind(t *testing.T) {
	ctx := context.Background()

	team := &litellmv1alpha1.LiteLLMTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "relist-team", Namespace: WatchNamespace},
		Spec:       litellmv1alpha1.LiteLLMTeamSpec{},
	}
	mcp := &litellmv1alpha1.LiteLLMMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "relist-mcp", Namespace: WatchNamespace},
		Spec:       litellmv1alpha1.LiteLLMMCPServerSpec{URL: "http://example.invalid", Transport: "http"},
	}
	a2a := &litellmv1alpha1.LiteLLMA2AAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "relist-a2a", Namespace: WatchNamespace},
		Spec:       litellmv1alpha1.LiteLLMA2AAgentSpec{URL: "http://example.invalid"},
	}
	for _, o := range []client.Object{team, mcp, a2a} {
		if err := k8sClient.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T: %v", o, err)
		}
		obj := o
		t.Cleanup(func() {
			var fresh litellmv1alpha1.LiteLLMTeam
			_ = fresh // per-kind cleanup below
			o := obj
			o.SetFinalizers(nil)
			_ = k8sClient.Update(context.Background(), o)
			_ = k8sClient.Delete(context.Background(), o)
		})
	}

	cases := []struct {
		name string
		fn   func(context.Context, client.Client, string) ([]reconcile.Request, error)
		want string
	}{
		{"teams", ListTeamRequests, "relist-team"},
		{"mcpservers", ListMCPServerRequests, "relist-mcp"},
		{"a2aagents", ListA2AAgentRequests, "relist-a2a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := tc.fn(ctx, k8sClient, WatchNamespace)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			for _, r := range reqs {
				if r.Name == tc.want && r.Namespace == WatchNamespace {
					return
				}
			}
			t.Fatalf("%s: %q not in %v", tc.name, tc.want, reqs)
		});
	}
}
```

Before writing, confirm the exact spec field names by reading
`api/litellm/v1alpha1/` for `LiteLLMTeamSpec`, `LiteLLMMCPServerSpec`, and
`LiteLLMA2AAgentSpec` — the CRDs carry CEL validation and required fields, so a
guessed literal will be rejected by the apiserver. Adjust the three literals
above to the minimal valid spec for each kind (mirror the `*SampleCR` helpers
already in `team_controller_test.go`, `mcpserver_controller_test.go`,
`a2aagent_controller_test.go` and reuse them if they fit).

- [ ] **Step 2: Run test to verify it fails**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestListRequests_CoversEveryKind`

Expected: FAIL — compile error `undefined: ListTeamRequests` (and the other two).

- [ ] **Step 3: Write minimal implementation**

Append to `internal/controller/team_controller.go`:

```go
// ListTeamRequests lists every LiteLLMTeam in namespace and returns their
// reconcile.Requests. Feeds SafetyRelistRunnable.ListRequests — see
// ListModelRequests for the shared contract.
func ListTeamRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMTeamList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return reqs, nil
}
```

Append to `internal/controller/mcpserver_controller.go`:

```go
// ListMCPServerRequests lists every LiteLLMMCPServer in namespace and returns
// their reconcile.Requests. Feeds SafetyRelistRunnable.ListRequests — see
// ListModelRequests for the shared contract.
func ListMCPServerRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMMCPServerList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return reqs, nil
}
```

Append to `internal/controller/a2aagent_controller.go`:

```go
// ListA2AAgentRequests lists every LiteLLMA2AAgent in namespace and returns
// their reconcile.Requests. Feeds SafetyRelistRunnable.ListRequests — see
// ListModelRequests for the shared contract.
func ListA2AAgentRequests(ctx context.Context, c client.Client, namespace string) ([]reconcile.Request, error) {
	var list litellmv1alpha1.LiteLLMA2AAgentList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
		})
	}
	return reqs, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS=TestListRequests_CoversEveryKind`

Expected: `PASS` (all three subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/team_controller.go internal/controller/mcpserver_controller.go \
        internal/controller/a2aagent_controller.go internal/controller/safety_relist_test.go
git commit -m "feat(relist): add ListTeamRequests, ListMCPServerRequests, ListA2AAgentRequests"
```

---

### Task 3: Give MCPServer and A2AAgent a relist channel parameter

`ModelReconciler`, `TeamReconciler` and `GuardRailReconciler` already accept a variadic `chan reconcile.Request` and install a raw-source consumer. `MCPServerReconciler.SetupWithManager(mgr)` and `A2AAgentReconciler.SetupWithManager(mgr)` do not. Add it, copying the existing block verbatim.

**Files:**
- Modify: `internal/controller/mcpserver_controller.go:879` (`SetupWithManager`)
- Modify: `internal/controller/a2aagent_controller.go:760` (`SetupWithManager`)
- Test: covered by Task 5's envtest wiring; no standalone test (a signature change with no behavior of its own — the behavior is tested end-to-end in Task 6).

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error`
  - `func (r *A2AAgentReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error`
  - Both variadic → **every existing call site keeps compiling unchanged.**

- [ ] **Step 1: Change the MCPServer signature**

In `internal/controller/mcpserver_controller.go`, change:

```go
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
```

to:

```go
func (r *MCPServerReconciler) SetupWithManager(mgr ctrl.Manager, safetyRelistCh ...chan reconcile.Request) error {
```

- [ ] **Step 2: Install the MCPServer raw-source consumer**

In the same function, immediately before the final `return b.Complete(r)`, insert (this is the exact block from `model_controller.go:1028-1046`):

```go
	if len(safetyRelistCh) > 0 && safetyRelistCh[0] != nil {
		ch := safetyRelistCh[0]
		b = b.WatchesRawSource(source.TypedFunc[reconcile.Request](
			func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case req, ok := <-ch:
							if !ok {
								return
							}
							q.Add(req)
						}
					}
				}()
				return nil
			},
		))
	}
```

Add any missing imports to the file: `"k8s.io/client-go/util/workqueue"`, `"sigs.k8s.io/controller-runtime/pkg/source"`, `"sigs.k8s.io/controller-runtime/pkg/reconcile"`.

- [ ] **Step 3: Repeat both steps for A2AAgent**

Apply the identical signature change and the identical raw-source block to
`internal/controller/a2aagent_controller.go:760` (`func (r *A2AAgentReconciler) SetupWithManager`), inserting before its final `return b.Complete(r)`.

- [ ] **Step 4: Verify it compiles and nothing regressed**

Run: `make build-operator && make test-envtest-pkg PKG=./internal/controller/...`

Expected: build succeeds; envtest suite passes unchanged (no call site passes a channel yet, so `len(safetyRelistCh) == 0` and behavior is identical).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/mcpserver_controller.go internal/controller/a2aagent_controller.go
git commit -m "refactor(relist): accept a safety-relist channel in MCPServer + A2AAgent SetupWithManager"
```

---

### Task 4: Wire the three Runnables in production (`cmd/main.go`)

**Files:**
- Modify: `cmd/main.go` (three insertions; anchors given below)

**Interfaces:**
- Consumes: `ListTeamRequests` / `ListMCPServerRequests` / `ListA2AAgentRequests` (Task 2); the variadic `SetupWithManager` signatures (Task 3); the existing `relistInterval` local (`cmd/main.go:147`).
- Produces: three `SafetyRelistRunnable`s registered on the manager.

- [ ] **Step 1: Add the Team Runnable**

Team reuses the **existing** `teamDefaultRequeueCh` (declared `cmd/main.go:418`, already passed to `TeamReconciler.SetupWithManager` at line 442). Insert immediately after the `mgr.Add(teamDefaultRunnable)` error block (ends ~line 430) and before `if err := (&controller.TeamReconciler{`:

```go
	// Team safety-relist. Shares teamDefaultRequeueCh with TeamDefaultRunnable:
	// two producers, one channel, one consumer goroutine (installed by
	// TeamReconciler.SetupWithManager) that q.Add()s everything. A second
	// channel would need a second variadic slot for no benefit.
	if err := mgr.Add(&controller.SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    watchNS,
		Interval:     relistInterval,
		Log:          ctrl.Log.WithName("team-safety-relist"),
		RequeueCh:    teamDefaultRequeueCh,
		ListRequests: controller.ListTeamRequests,
		LogLabel:     "teams",
	}); err != nil {
		setupLog.Error(err, "unable to add team SafetyRelistRunnable")
		os.Exit(1)
	}
```

- [ ] **Step 2: Add the MCPServer Runnable**

Insert immediately before `if err := (&controller.MCPServerReconciler{` (line 333), and change that literal's terminator at line 343 from `}).SetupWithManager(mgr); err != nil {` to `}).SetupWithManager(mgr, mcpSafetyRelistCh); err != nil {`:

```go
	mcpSafetyRelistCh := make(chan reconcile.Request, 256)
	if err := mgr.Add(&controller.SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    watchNS,
		Interval:     relistInterval,
		Log:          ctrl.Log.WithName("mcpserver-safety-relist"),
		RequeueCh:    mcpSafetyRelistCh,
		ListRequests: controller.ListMCPServerRequests,
		LogLabel:     "mcpservers",
	}); err != nil {
		setupLog.Error(err, "unable to add mcpserver SafetyRelistRunnable")
		os.Exit(1)
	}
```

- [ ] **Step 3: Add the A2AAgent Runnable**

Insert immediately before `if err := (&controller.A2AAgentReconciler{` (line 366), and change that literal's terminator at line 376 from `}).SetupWithManager(mgr); err != nil {` to `}).SetupWithManager(mgr, a2aSafetyRelistCh); err != nil {`:

```go
	a2aSafetyRelistCh := make(chan reconcile.Request, 256)
	if err := mgr.Add(&controller.SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    watchNS,
		Interval:     relistInterval,
		Log:          ctrl.Log.WithName("a2aagent-safety-relist"),
		RequeueCh:    a2aSafetyRelistCh,
		ListRequests: controller.ListA2AAgentRequests,
		LogLabel:     "a2aagents",
	}); err != nil {
		setupLog.Error(err, "unable to add a2aagent SafetyRelistRunnable")
		os.Exit(1)
	}
```

- [ ] **Step 4: Fix the stale comment at `cmd/main.go:140-145`**

Replace `the four reconciler package vars (via SetSafetyRelistIntervals) AND the three relist Runnables (Model, Team, GuardRail) below` with:

```go
	// H5: parse LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL EXACTLY ONCE here and
	// thread the resolved value to every consumer — the five relist Runnables
	// (Model, Team, MCPServer, A2AAgent, GuardRail) and TeamDefaultRunnable
	// below. Default 10m (DefaultSafetyRelistInterval); 5s floor; invalid
	// input aborts startup. Accepts any time.ParseDuration string. Reasoning +
	// floor justification in internal/controller/safety_relist.go.
```

- [ ] **Step 5: Verify build + full envtest**

Run: `make build-operator && make test-envtest-pkg PKG=./internal/controller/...`

Expected: build succeeds, envtest passes. (`SetSafetyRelistIntervals` is still called and the `RequeueAfter` paths still exist — both mechanisms coexist at this commit. That is intentional: this commit is additive and independently revertable.)

- [ ] **Step 6: Commit**

```bash
git add cmd/main.go
git commit -m "feat(relist): wire Team, MCPServer, A2AAgent SafetyRelistRunnables"
```

---

### Task 5: Wire the three Runnables in the envtest suite

The suite gates its Runnables OFF by default via `suiteRelistEnabled` (#74 contention fix); only drift-recovery tests opt in with `enableSuiteRelist(t)`. The three new Runnables **must** carry the same `Gate` or they reintroduce the flake.

**Files:**
- Modify: `internal/controller/suite_test.go` (package-var block ~line 118/192; wiring before each `SetupWithManager`)

**Interfaces:**
- Consumes: `ListTeamRequests` / `ListMCPServerRequests` / `ListA2AAgentRequests`; `suiteRelistEnabled` (existing `atomic.Bool`).
- Produces: package vars `mcpSafetyRelistCh`, `a2aSafetyRelistCh` (`chan reconcile.Request`). Team reuses the existing `teamDefaultRequeueCh` (line 512, `cap=16`).

- [ ] **Step 1: Declare the two new channel vars**

Next to the existing `modelSafetyRelistCh chan reconcile.Request` (line 118) and `guardrailSafetyRelistCh chan reconcile.Request` (line 192), add:

```go
	mcpSafetyRelistCh chan reconcile.Request
	a2aSafetyRelistCh chan reconcile.Request
```

- [ ] **Step 2: Wire the MCPServer Runnable**

Immediately before `if err := mcpServerReconciler.SetupWithManager(mgr); err != nil {` (line 457), insert, and change that call to `mcpServerReconciler.SetupWithManager(mgr, mcpSafetyRelistCh)`:

```go
	mcpSafetyRelistCh = make(chan reconcile.Request, 256)
	if err := mgr.Add(&SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    WatchNamespace,
		Interval:     100 * time.Millisecond,
		Log:          logr.Discard(),
		RequeueCh:    mcpSafetyRelistCh,
		ListRequests: ListMCPServerRequests,
		LogLabel:     "mcpservers",
		Gate:         suiteRelistEnabled.Load,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(mcpserver SafetyRelistRunnable): %v\n", err)
		return 1
	}
```

- [ ] **Step 3: Wire the A2AAgent Runnable**

Immediately before `if err := a2aAgentReconciler.SetupWithManager(mgr); err != nil {` (line 482), insert, and change that call to `a2aAgentReconciler.SetupWithManager(mgr, a2aSafetyRelistCh)`:

```go
	a2aSafetyRelistCh = make(chan reconcile.Request, 256)
	if err := mgr.Add(&SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    WatchNamespace,
		Interval:     100 * time.Millisecond,
		Log:          logr.Discard(),
		RequeueCh:    a2aSafetyRelistCh,
		ListRequests: ListA2AAgentRequests,
		LogLabel:     "a2aagents",
		Gate:         suiteRelistEnabled.Load,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(a2aagent SafetyRelistRunnable): %v\n", err)
		return 1
	}
```

- [ ] **Step 4: Wire the Team Runnable**

Immediately before `if err := teamReconciler.SetupWithManager(mgr, teamDefaultRequeueCh); err != nil {` (line 526), insert (no call change — the channel is already passed):

```go
	if err := mgr.Add(&SafetyRelistRunnable{
		Client:       mgr.GetClient(),
		Namespace:    WatchNamespace,
		Interval:     100 * time.Millisecond,
		Log:          logr.Discard(),
		RequeueCh:    teamDefaultRequeueCh,
		ListRequests: ListTeamRequests,
		LogLabel:     "teams",
		Gate:         suiteRelistEnabled.Load,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "mgr.Add(team SafetyRelistRunnable): %v\n", err)
		return 1
	}
```

> `teamDefaultRequeueCh` is `cap=16` here (line 512) vs 256 in production. With Task 1's blocking send and the `Gate` off for most tests this is fine; if a Team drift test with >16 Teams ever wedges, raise the cap rather than restoring the drop.

- [ ] **Step 5: Verify the suite still passes**

Run: `make test-envtest-pkg PKG=./internal/controller/...`

Expected: `PASS` — unchanged, because the new Runnables are gated OFF for every test that does not call `enableSuiteRelist(t)`.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/suite_test.go
git commit -m "test(relist): wire gated Team/MCPServer/A2AAgent Runnables into the envtest suite"
```

---

### Task 6: Delete every `RequeueAfter` safety-relist path

This is the load-bearing commit: after it, the Runnable is the only periodic mechanism. Ten sites across four controllers.

**Files:**
- Modify: `internal/controller/model_controller.go` (lines 390, 664, 920 + the `modelSafetyRelistInterval` var at line 57 and its doc at ~55)
- Modify: `internal/controller/mcpserver_controller.go` (lines 289, 568, 743 + var at 63, doc at ~57)
- Modify: `internal/controller/team_controller.go` (lines 552, 700 + var at 66, doc at ~64)
- Modify: `internal/controller/a2aagent_controller.go` (lines 482, 583 + var at 57, doc at ~55)
- Modify: `internal/controller/predicates.go` (delete `withJitter`, lines 77-86)
- Delete: `internal/controller/predicates_withjitter_test.go`
- Modify: `internal/controller/safety_relist.go` (delete `SetSafetyRelistIntervals`, lines 73-95; fix docs)
- Modify: `cmd/main.go` (delete the `controller.SetSafetyRelistIntervals(relistInterval)` call, line 152)

**Interfaces:**
- Consumes: all Runnables wired (Tasks 4, 5).
- Produces: `SetSafetyRelistIntervals` and `withJitter` **no longer exist**. The four `*SafetyRelistInterval` package vars no longer exist. `EnvSafetyRelistInterval`, `SafetyRelistFloor`, `DefaultSafetyRelistInterval`, `ResolvedSafetyRelistInterval`, `ParseSafetyRelistInterval` all **remain** (main.go still parses the env var to set every `Runnable.Interval`).

> **Line numbers drift as you edit.** Work **bottom-up within each file** (highest line first), or re-grep between edits:
> `grep -n "withJitter(" internal/controller/*.go`

- [ ] **Step 1: Replace all 10 return sites**

Each site is one of two shapes. Replace every occurrence of

```go
	return ctrl.Result{RequeueAfter: withJitter(<kind>SafetyRelistInterval)}, nil
```

with

```go
	return ctrl.Result{}, nil
```

and delete the `// Periodic safety-relist requeue ...` / `// Periodic safety relist on soft-fail path: ...` / `// v0.4.5: periodic safety-relist requeue ...` comment directly above it. At the **steady-state** site in each of the four controllers, replace that comment block with:

```go
		// No RequeueAfter: the per-kind SafetyRelistRunnable owns the periodic
		// vanish-probe tick (cmd/main.go). A requeue here would only fire on
		// the paths that happen to carry one — the Runnable fires regardless
		// of which branch the reconcile took, which is the point of a safety
		// net (issue #102).
```

Verify you got all ten:

```bash
grep -rn "withJitter\|SafetyRelistInterval)" internal/controller/*.go | grep -v safety_relist
```

Expected: no output.

- [ ] **Step 2: Delete the four interval package vars**

Delete these lines and their preceding doc comments:
- `internal/controller/model_controller.go:57` — `var modelSafetyRelistInterval = 10 * time.Minute`
- `internal/controller/mcpserver_controller.go:63` — `var mcpSafetyRelistInterval = 10 * time.Minute`
- `internal/controller/team_controller.go:66` — `var teamSafetyRelistInterval = 10 * time.Minute`
- `internal/controller/a2aagent_controller.go:57` — `var a2aAgentSafetyRelistInterval = 10 * time.Minute`

- [ ] **Step 3: Delete `withJitter` and its test**

Delete `internal/controller/predicates.go:77-86` (the whole `func withJitter`) and its doc comment.

```bash
rm /home/coder/workspace/local/alitellm-operator/internal/controller/predicates_withjitter_test.go
```

If `math/rand` becomes an unused import in `predicates.go`, remove it.

- [ ] **Step 4: Delete `SetSafetyRelistIntervals` and its caller**

Delete `internal/controller/safety_relist.go:73-95` (doc comment + `func SetSafetyRelistIntervals`). Delete `cmd/main.go:152` (`controller.SetSafetyRelistIntervals(relistInterval)`).

In `internal/controller/safety_relist.go`, fix the two now-wrong doc blocks:
- On `EnvSafetyRelistInterval` (line ~10): replace `shared by the MCPServer, Model, Team, and A2AAgent reconcilers AND the three relist Runnables (Model, Team, GuardRail)` with `shared by the five per-kind relist Runnables (Model, Team, MCPServer, A2AAgent, GuardRail) and TeamDefaultRunnable`.
- On `DefaultSafetyRelistInterval` (line ~30): replace `Shared by the four reconciler RequeueAfter paths AND the three relist Runnables (Model, Team, GuardRail) so one env knob yields one cadence everywhere. Matches the *_controller.go package-var defaults.` with `Shared by all five per-kind relist Runnables so one env knob yields one cadence everywhere. There are no longer any RequeueAfter safety-relist paths — the Runnable is the single mechanism (issue #102 follow-up).`
- On `ResolvedSafetyRelistInterval` (line ~37): replace `Use the single returned value for BOTH SetSafetyRelistIntervals and every Runnable.Interval` with `Use the single returned value for every Runnable.Interval`.

- [ ] **Step 5: Delete the `SetSafetyRelistIntervals` test if present**

```bash
grep -n "SetSafetyRelistIntervals" internal/controller/safety_relist_test.go
```

If any test references it, delete that test function. Then:

```bash
grep -rn "SetSafetyRelistIntervals\|withJitter" --include='*.go' /home/coder/workspace/local/alitellm-operator
```

Expected: no output.

- [ ] **Step 6: Run the full controller suite**

Run: `make test-envtest-pkg PKG=./internal/controller/...`

Expected: `PASS`.

**If a test fails with a ~30s poll timeout**, it was leaning on `RequeueAfter` as its periodic channel. The fix is to opt that test into the suite Runnables — add `enableSuiteRelist(t)` as its first line (mirroring `model_controller_test.go:1962`). Do **not** restore the `RequeueAfter`. Record each test you touch in the commit body.

- [ ] **Step 7: Run the relist package + unit + lint**

Run: `make test-envtest-pkg PKG=./internal/controller/relist/... && make test-unit && make qa-lint`

Expected: all pass. `qa-lint` catches any now-unused import or var left behind.

- [ ] **Step 8: Commit**

```bash
git add -A internal/controller cmd/main.go
git commit -m "refactor(relist): drop all RequeueAfter safety-relist paths

The SafetyRelistRunnable is now the single periodic drift-detection
mechanism for all five domain reconcilers. RequeueAfter only fires from
return sites that carry it, so every early return was a hole in the
safety net — the failure family of #102. The Runnable ticks from outside
Reconcile and cannot be defeated by a bad branch.

Removes 10 return sites, four *SafetyRelistInterval package vars,
SetSafetyRelistIntervals, and withJitter (all now dead).
LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL keeps identical user-facing
semantics — it now sets every Runnable.Interval."
```

---

### Task 7: Prove the convergence end-to-end, then document it

A green suite proves nothing was broken. It does not prove the Runnable actually recovers an out-of-band delete for the three newly-converged kinds — that is the whole point of the change and needs its own test.

**Files:**
- Create: `internal/controller/relist/converged_relist_test.go`
- Modify: `CLAUDE.md` ("Repository-specific patterns")
- Modify: `docs/developer-guide/architecture.md:123` area

**Interfaces:**
- Consumes: everything above.
- Produces: no new exported symbols.

- [ ] **Step 1: Write the failing test**

Read `internal/controller/relist/guardrail_relist_test.go:139` (`TestGuardRail_SafetyRelist_CreateMissing`) **in full** first — it is the working reference for this package's bootstrap, mock, and 30s recovery deadline. Mirror its structure for MCPServer:

```go
// TestMCPServer_SafetyRelist_CreateMissing — with all RequeueAfter paths
// removed, the MCPServer's SafetyRelistRunnable is the only thing that can
// notice an out-of-band delete in LiteLLM and re-create the entry. Mirrors
// TestGuardRail_SafetyRelist_CreateMissing.
func TestMCPServer_SafetyRelist_CreateMissing(t *testing.T) {
	// 1. Create an MCPServer CR; poll until Ready=Synced and the mock holds it.
	// 2. Delete the entry from the mock out-of-band (no CR change, so no
	//    watch event fires — only the relist tick can catch this).
	// 3. Assert the mock regains the entry within 30s, driven purely by the
	//    Runnable tick.
}
```

Fill the body from the guardrail reference, substituting the MCPServer CR type, the mock's MCP helpers, and `mcpServerReconciler`. The relist suite's `TestMain` (`internal/controller/relist/suite_test.go:72`) must wire an MCPServer `SafetyRelistRunnable` the same way it wires the guardrail one — add it if absent.

- [ ] **Step 2: Run it to verify it fails on a reverted controller**

Temporarily re-add the steady-state `RequeueAfter` in `mcpserver_controller.go` and comment out the MCPServer Runnable in the relist suite's `TestMain`, then:

Run: `make test-envtest-pkg PKG=./internal/controller/relist/... FOCUS=TestMCPServer_SafetyRelist_CreateMissing`

Expected: FAIL (no mechanism re-creates the entry). Revert both temporary edits.

- [ ] **Step 3: Run it against the real implementation**

Run: `make test-envtest-pkg PKG=./internal/controller/relist/... FOCUS=TestMCPServer_SafetyRelist_CreateMissing`

Expected: `PASS`.

- [ ] **Step 4: Update `CLAUDE.md`**

Add under "Repository-specific patterns":

```markdown
- **Periodic drift detection = `SafetyRelistRunnable`, never `RequeueAfter`.**
  Each of the five domain reconcilers (Model, Team, MCPServer, A2AAgent,
  GuardRail) has exactly one `SafetyRelistRunnable` in `cmd/main.go`, ticking
  at `LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL` (default 10m). Reconcilers
  return bare `ctrl.Result{}, nil` — there are no `RequeueAfter` safety-relist
  paths and no `withJitter`. WHY: a `RequeueAfter` only fires from the return
  site that carries it, so any early return silently loses the CR's periodic
  tick (the #102 failure family). The Runnable ticks from outside `Reconcile`
  and cannot be defeated by a branch nobody anticipated. Adding a new domain
  reconciler? Add its `List<Kind>Requests` + a Runnable in `cmd/main.go` + a
  **gated** one in `suite_test.go` (`Gate: suiteRelistEnabled.Load` — ungated
  suite runnables are the #74 contention flake). Do NOT reach for
  `RequeueAfter`.
```

- [ ] **Step 5: Update the public docs**

In `docs/developer-guide/architecture.md` (~line 123), update the prose around `EnvSafetyRelistInterval` to say the interval drives the five per-kind relist Runnables. Confirm these two rows still read true (they should — the knob's meaning is unchanged) and fix them if not:
- `docs/getting-started/installation.md:66` (`safetyRelistInterval` Helm value)
- `docs/getting-started/installation.md:95` (env var table)
- `docs/developer-guide/development.md:163`

- [ ] **Step 6: Full gate**

```bash
make test-full
make e2e-full
```

Expected: `test-full` (unit + envtest, race) passes. `e2e-full` reports `SUCCESS! -- 28 Passed | 0 Failed`.

The e2e cluster sets `LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL=10s` (`test/e2e/cluster/02-operator/operator.values.yaml:20`), so any e2e spec that relied on a `RequeueAfter` tick now exercises the Runnable at 10s. A failure here is a real regression, not a flake — investigate, do not retry-and-hope.

- [ ] **Step 7: Commit and open the PR**

```bash
git add internal/controller/relist CLAUDE.md docs/
git commit -m "test(relist): prove Runnable-only drift recovery; document the single mechanism"
git push -u origin refactor/relist-runnable-convergence
```

PR body must state: the mechanism rationale (safety net must not depend on the reconcile's return path), the ten removed sites, the lossless-enqueue fix, the accepted herd trade-off from Non-Goals, and that the env knob's user-facing meaning is unchanged.

---

## Verification Checklist

Before requesting review:

- [ ] `grep -rn "withJitter\|SetSafetyRelistIntervals\|SafetyRelistInterval)" --include='*.go' .` → only `safety_relist.go` const/func definitions
- [ ] `grep -c "SafetyRelistRunnable{" cmd/main.go` → `5`
- [ ] Every `SafetyRelistRunnable` in `suite_test.go` carries `Gate: suiteRelistEnabled.Load`
- [ ] `make test-full` green
- [ ] `make e2e-full` green (28/28)
- [ ] `make qa-lint` green
- [ ] Pre-push gate green (runs automatically on `git push` with `make hooks` installed)
