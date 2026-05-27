// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestModelDeletionPolicy_DeleteRetainsFinalizerWhenLiteLLMUnavailable
// — Issue #23 envtest. With spec.deletionPolicy=Delete and the connection
// cache not Ready, the model reconciler must:
//
//  1. Refuse to remove the finalizer (CR stays in Terminating).
//  2. Emit a LiteLLMDeleteBlocked Warning Event.
//  3. Once the user flips the annotation override to Orphan, finalizer
//     is removed and the CR is garbage-collected — even while LiteLLM
//     is still unavailable.
//
// The shape of the test exercises the resolver precedence chain
// (annotation > spec) end-to-end.
func TestModelDeletionPolicy_DeleteRetainsFinalizerWhenLiteLLMUnavailable(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()

	const name = "strict-delete-model"
	ensureNoModel(t, ctx, name)
	resetConnCacheSnapshot()

	// Phase 1: connection Ready so finalizer is added on first reconcile.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		ensureNoConnectionDefault(t, context.Background())
	})

	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
		},
		Spec: litellmv1alpha1.ModelSpec{
			DeletionPolicy: string(deletionpolicy.Delete),
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create Model: %v", err)
	}
	t.Cleanup(func() {
		// Force-cleanup: annotation override → Orphan + cache Ready so
		// finalizer can drain even if the test asserted mid-block.
		connCache.Rebuild(connection.ConnectionSnapshot{Ready: true, Reason: "Synced"})
		ensureNoModel(t, context.Background(), name)
	})

	// Wait until finalizer has been added.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var got litellmv1alpha1.LiteLLMModel
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &got); err == nil {
			for _, f := range got.Finalizers {
				if f == modelFinalizer {
					goto FINALIZED
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("finalizer %q never added", modelFinalizer)
FINALIZED:

	// Phase 2: flip connection cache to NotReady so the deletion path
	// hits the `!snap.Ready` ack-missing branch.
	connCache.Rebuild(connection.ConnectionSnapshot{Ready: false, Reason: "Unreachable"})

	// Phase 3: delete the CR.
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete Model: %v", err)
	}

	// Phase 4: assert the CR stays in Terminating (finalizer NOT removed)
	// for at least 2s of reconcile attempts. Controller-runtime backoff
	// will requeue on the returned error.
	time.Sleep(2 * time.Second)
	if err := k8sClient.Get(ctx, key, &got); err != nil {
		t.Fatalf("CR vanished prematurely while deletionPolicy=Delete + cache NotReady: %v", err)
	}
	if got.DeletionTimestamp.IsZero() {
		t.Fatalf("DeletionTimestamp is zero — Delete did not register?")
	}
	found := false
	for _, f := range got.Finalizers {
		if f == modelFinalizer {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("finalizer was removed despite deletionPolicy=Delete + cache NotReady (Issue #23 regression)")
	}

	// Phase 5: flip annotation override to Orphan — finalizer should drain.
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	got.Annotations[deletionpolicy.AnnotationOverride] = string(deletionpolicy.Orphan)
	if err := k8sClient.Update(ctx, &got); err != nil {
		t.Fatalf("annotate CR with override=Orphan: %v", err)
	}

	// Phase 6: poll until CR is gone (finalizer drained on Orphan path).
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &got); apierrors.IsNotFound(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CR did not vanish within 10s after annotation override to Orphan")
}

// TestModel_FinalizerNameResolve404RemovesFinalizer asserts that when
// status.lastRendered.modelID is empty and LiteLLM returns 404 to
// GET /model/info?model_name=<name>, the finalizer is removed (the entry
// is treated as already-absent), instead of leaving the CR stranded in
// Terminating with controller-runtime backoff retrying forever.
//
// Post-2026-05-26 review finding F4.
//
// Unit-level coverage is the source of truth for the contract:
//
//   - internal/litellm/errors_test.go:TestIsNotFound_RejectedError404 +
//     TestIsNotFound_WrappedRejectedError404 prove the helper unwraps
//     *RejectedError{Status:404} regardless of wrapping.
//   - internal/litellm/model_test.go:TestGetModelInfoByName_404ReturnsNilNil
//     proves the helper honors the documented (nil, nil) contract on 404.
//   - internal/controller/model_controller.go switch on (err, resolved)
//     routes (nil, nil) → onAckMissing(...) (Orphan: removes finalizer;
//     Delete: gates per policy) — same contract as the direct-ID path.
//
// This envtest skip exists because the existing mock returns empty
// `data:[]` (200) for unknown model names, not a raw 404. Forcing a 404
// would require either:
//   - A new mock mode (ModeForceNotFound) that flips /model/info to 404,
//     or
//   - A handler hook keyed by model name to selectively return 404.
//
// Both options pollute the mock for a single regression — the unit-level
// coverage above already proves the wire-level contract end-to-end.
// E2E suites continue to exercise the empty-data[] path (which routes
// through the same switch case post-fix).
func TestModel_FinalizerNameResolve404RemovesFinalizer(t *testing.T) {
	t.Skip("contract proven by unit tests: " +
		"internal/litellm/errors_test.go::TestIsNotFound_RejectedError404 + " +
		"internal/litellm/model_test.go::TestGetModelInfoByName_404ReturnsNilNil. " +
		"Adding a 404-mode to the shared mock would pollute it for one regression; " +
		"empty-data[] coverage (TestModelStaleStatusDeletion in model_controller_test.go) " +
		"exercises the same switch case end-to-end.")
}
