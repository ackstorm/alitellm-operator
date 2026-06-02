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
		// Force-cleanup: restore the cache to a VALID Ready snapshot so the
		// finalizer can drain even if the test asserted mid-block. Must be a
		// Client-backed Ready snapshot — a Ready+nil-Client snapshot poisons
		// the shared singleton and panics the next reconcile (issue #74).
		setConnCacheReady()
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
	//
	// First REMOVE the LiteLLMConnection. Under `-shuffle=on` a live
	// connCR + happy mock lets the connection reconciler asynchronously
	// re-probe and Rebuild the cache back to Ready during the 2s
	// assertion window below — the model's direct-ID delete then succeeds
	// and the CR vanishes, failing the "finalizer retained" assertion.
	// With the connCR gone, nothing can flip the cache back to Ready, so
	// the pinned NotReady snapshot sticks deterministically.
	ensureNoConnectionDefault(t, ctx)
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

// TestModel_DeletionPath_ConfirmedAbsent_DeletePolicyDrains is the
// regression for a model that LiteLLM REJECTED on create (HTTP 422, e.g.
// missing the required `model` field) and so never got a
// status.lastRendered.modelID. With spec.deletionPolicy=Delete and the
// connection cache Ready, deleting the CR must drain the finalizer — the
// entry is CONFIRMED absent in LiteLLM (name-resolve returns empty
// data[]), so the Delete goal is already satisfied.
//
// Pre-fix, the (err==nil && resolved==nil) confirmed-absent case was
// routed through onAckMissing, which gates on policy and therefore left
// deletionPolicy=Delete CRs stranded in Terminating with controller-
// runtime backoff retrying forever. onAckMissing is now reserved for
// *cannot-confirm* conditions (LiteLLM unavailable, 401); a confirmed
// 404/empty drains the finalizer regardless of policy, matching the
// sibling controllers (a2aagent/mcpserver).
//
// This reproduces the exact user-reported scenario (CR
// `uat-invalid-no-model-field` stuck Terminating) WITHOUT the annotation
// override break-glass. Mode422 forces create rejection, so the entry is
// genuinely absent from the mock's model store — the same empty-data[]
// path real LiteLLM serves for an unknown model_name.
func TestModel_DeletionPath_ConfirmedAbsent_DeletePolicyDrains(t *testing.T) {
	ctx := context.Background()
	mockServer.SetMode(mock.Mode422)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()

	const name = "confirmed-absent-delete-model"
	ensureNoModel(t, ctx, name)
	resetConnCacheSnapshot()

	// Connection Ready (Mode422 serves /key/health as happy; it rejects
	// only POST /model/new) so the deletion path enters the snap.Ready
	// branch and reaches the name-resolve fallback rather than the
	// !snap.Ready ack-missing gate.
	ensureNoConnectionDefault(t, ctx)
	connCR := connDefaultCR()
	if err := k8sClient.Create(ctx, connCR); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		_ = k8sClient.Delete(context.Background(), connCR, &client.DeleteOptions{})
		ensureNoModel(t, context.Background(), name)
		time.Sleep(50 * time.Millisecond)
	})
	connSnap := pollSnapshotReason(30*time.Second, reasonSynced)
	if connSnap.Reason != reasonSynced {
		t.Fatalf("LiteLLMConnection not Synced within 30s; reason=%q", connSnap.Reason)
	}

	// Create a Model whose create LiteLLM rejects (Mode422). The finalizer
	// is still added (Step 2b runs independent of the create outcome), but
	// status.lastRendered.modelID stays empty and nothing lands in the
	// mock's model store.
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

	// Wait until the finalizer has been added.
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

	// Sanity: ModelID must be empty (create was rejected) so the deletion
	// path takes the name-resolve fallback, not the direct-ID DELETE.
	if got.Status.LastRendered.ModelID != "" {
		t.Fatalf("precondition violated: ModelID=%q, expected empty after 422 create",
			got.Status.LastRendered.ModelID)
	}

	// Delete the CR. deletionPolicy=Delete + cache Ready + entry absent →
	// confirmed-absent → finalizer drains WITHOUT any annotation override.
	if err := k8sClient.Delete(ctx, &got); err != nil {
		t.Fatalf("delete Model: %v", err)
	}

	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &got); apierrors.IsNotFound(err) {
			return // drained — pass
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("CR stuck Terminating: finalizer not drained within 15s despite " +
		"deletionPolicy=Delete + cache Ready + entry confirmed absent in LiteLLM " +
		"(regression — confirmed-absent must not gate on policy)")
}
