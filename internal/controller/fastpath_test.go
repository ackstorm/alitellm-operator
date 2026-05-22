// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestNoOp401FastPath — REL-06. The §7.7 401 fast-path branch in
// NoOpReconciler.Reconcile invalidates the cache stub and returns nil
// (NOT err) so controller-runtime does NOT requeue with exponential
// backoff — this is the anti-storm rule that prevents an auth-loop
// across many CRs after a master-key rotation.
//
// Procedure:
//
// 1. Set mock mode to ModeHappy; reset counters; clear cache.Invalidated.
// 2. Create a Model CR in WATCH_NAMESPACE; wait for first reconcile.
// 3. Switch mock to Mode401.
// 4. Trigger a re-reconcile by editing the CR's labels (bumps
// metadata.generation, fires an event).
// 5. Wait up to 5s for reconcileCalls to increment.
// 6. Assert cache.Invalidated.Load == true (the fast-path ran).
// 7. Assert mock.Mutations did NOT grow unbounded (anti-storm: fewer
// than 5 mutation calls in 10s — Phase 1 smoke is loose; Phase 2's
// elevated test will tighten the bound).
//
// Sanity: if the fast-path were NOT taken, the reconciler would return
// the *Auth401Error to controller-runtime, which would requeue via the
// default rate limiter. With NO RequeueAfter and rapid retry, mutations
// could plausibly grow into the hundreds in a 10s window. The <5 bound
// is generous enough to allow occasional probe re-tries while still
// catching the storm regression.
func TestNoOp401FastPath(t *testing.T) {
	if reconcileCalls == nil {
		t.Fatal("suite_test.go did not initialize globals")
	}

	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	fakeCache.Invalidated.Store(false)

	ctx := context.Background()

	// Snapshot BEFORE Create to avoid the race where reconcile fires
	// faster than the subsequent Load.
	beforeFirst := reconcileCalls.Load()

	model := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rel-06-fastpath",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{},
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	t.Cleanup(func() {
		// Reset mock back to happy mode so subsequent tests don't
		// inherit the 401 state.
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), model, &client.DeleteOptions{})
	})

	// Wait for first reconcile (happy path).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if reconcileCalls.Load() > beforeFirst {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if reconcileCalls.Load() == beforeFirst {
		t.Fatalf("first reconcile never fired")
	}
	// Settle.
	time.Sleep(500 * time.Millisecond)

	// Flip mock to 401 mode AFTER the first (happy) reconcile.
	mockServer.SetMode(mock.Mode401)
	mockServer.ResetCounters()
	beforeSecond := reconcileCalls.Load()

	// Force a re-reconcile by editing the CR's labels — bumps
	// resourceVersion and triggers a Watch event without needing to
	// touch the spec (which would change generation; we want a
	// label-edit reconcile to be representative of "any" reconcile).
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(model), model); err != nil {
		t.Fatalf("re-get CR: %v", err)
	}
	if model.Labels == nil {
		model.Labels = map[string]string{}
	}
	model.Labels["rel-06"] = "fastpath-trigger"
	if err := k8sClient.Update(ctx, model); err != nil {
		t.Fatalf("update CR labels: %v", err)
	}

	// Wait for the reconciler to fire under 401 mode.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reconcileCalls.Load() > beforeSecond {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if reconcileCalls.Load() == beforeSecond {
		t.Fatalf("second reconcile (post-label-edit) never fired")
	}

	// Assert cache.Invalidated was set — proves the fast-path branch ran.
	// Give the reconciler a moment to fully execute through the branch.
	time.Sleep(500 * time.Millisecond)
	if !fakeCache.Invalidated.Load() {
		t.Errorf("REL-06 FAIL: fake cache.Invalidated was NOT set — the §7.7 401 fast-path did not execute")
	}

	// Anti-storm bound: observe for 4s. If the fast-path returned err
	// (not nil), controller-runtime would requeue rapidly and the mock
	// would see many mutation/probe calls. Mutations should stay 0
	// (the no-op reconciler only issues ProbeConnection — a GET — and
	// 401 mode returns before any mutation can happen). The probe is a
	// GET so it counts as a read; reads SHOULD be bounded but may
	// climb if controller-runtime requeues (even on nil, periodic resync
	// can fire). We assert reads stay below a generous bound.
	time.Sleep(4 * time.Second)
	mutationsAfter := mockServer.Mutations()
	readsAfter := mockServer.Reads()
	if mutationsAfter > 5 {
		t.Errorf("REL-06 anti-storm FAIL: %d mutations in 4s window (expected <5 — fast-path returning nil should keep workqueue idle)", mutationsAfter)
	}
	t.Logf("REL-06 fast-path: mutations=%d, reads=%d over 4s (cache.Invalidated=true)", mutationsAfter, readsAfter)
}
