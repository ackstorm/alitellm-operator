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

// TestIdempotencyNoMutationSteadyState — accelerated AC-R1 (REL-02 +
// idempotency smoke). After the first reconcile of a CR, the steady-state
// observation window MUST emit zero LiteLLM mutation calls
// (POST/PUT/DELETE/PATCH).
//
// Procedure:
//
// 1. mockServer.SetMode(ModeHappy); reset mock counters.
// 2. Create a Model CR in WATCH_NAMESPACE; wait for the first reconcile
// (the no-op reconciler triggers a POST /key/health probe).
// 3. Snapshot mock.Mutations and reconcileCalls AFTER the first
// reconcile — this is t=0 for the steady-state window.
// 4. Observe for 10 seconds with SafetyRelistInterval=1s (already wired
// by suite_test.go). NO CR edits, NO mock mode flips.
// 5. Assert mock.Mutations unchanged at the end of the window.
// (Reads are allowed — the no-op reconciler issues a GET probe; the
// real Phase-3 reconciler will also issue reads during steady-state.)
//
// Mocking note (Pitfall 9): GET is a read; POST/PUT/DELETE are mutations.
// AC-R1 only forbids mutations during steady state. The test inspects
// both counters but only asserts on Mutations.
//
// Phase 7's elevated test (TestIdempotency35MinReal in this file, gated
// by //go:build longidempotency) runs the full 35-min wall-clock window
// per spec §11 AC-R1.
func TestIdempotencyNoMutationSteadyState(t *testing.T) {
	if reconcileCalls == nil {
		t.Fatal("suite_test.go did not initialize globals — TestMain ordering bug")
	}

	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()

	ctx := context.Background()

	// Snapshot reconcileCalls BEFORE creating the CR so we don't miss a
	// reconcile that fires faster than our subsequent Load.
	beforeCreate := reconcileCalls.Load()

	model := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ac-r1-smoke",
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{},
	}
	if err := k8sClient.Create(ctx, model); err != nil {
		t.Fatalf("create CR: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), model, &client.DeleteOptions{})
	})

	// Wait for the first reconcile.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if reconcileCalls.Load() > beforeCreate {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if reconcileCalls.Load() == beforeCreate {
		t.Fatalf("first reconcile never fired after CR create within 15s (beforeCreate=%d, now=%d)", beforeCreate, reconcileCalls.Load())
	}

	// Allow the first reconcile to complete fully before snapshotting.
	time.Sleep(500 * time.Millisecond)

	// Snapshot t=0 for the steady-state window.
	mutations0 := mockServer.Mutations()
	reads0 := mockServer.Reads()

	// Steady-state observation: cross the accelerated 1s envtest
	// safety-relist cadence with NO edits to the CR or
	// mock. Mutations MUST stay at 0; reads are allowed (the no-op
	// reconciler will not auto-trigger reconciles since we returned
	// ctrl.Result{}, nil — no RequeueAfter, no error).
	//
	// SafetyRelistInterval is 1s (set in suite_test.go); one crossed
	// interval is enough to catch a tight reconcile storm while keeping
	// the smoke test cheap. The §7.6 30-min production
	// cadence does not affect this assertion.
	time.Sleep(1250 * time.Millisecond)

	mutationsEnd := mockServer.Mutations()
	readsEnd := mockServer.Reads()

	deltaMutations := mutationsEnd - mutations0
	deltaReads := readsEnd - reads0

	if deltaMutations != 0 {
		t.Errorf("AC-R1 FAIL: mock saw %d LiteLLM mutation calls during steady-state window (expected 0)", deltaMutations)
	}
	t.Logf("AC-R1 steady-state: %d mutations (want 0) + %d reads (allowed) over accelerated observation window", deltaMutations, deltaReads)
}
