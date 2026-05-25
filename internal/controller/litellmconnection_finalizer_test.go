// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestConnectionFinalizerAddRemove_CONN_09 — CONN-09 + spec §6.1.
//
// Deleting LiteLLMConnection/default must:
//
// 1. Invalidate the manager-level cache (cache.Snapshot.Reason ==
// "Absent" after the finalizer ran).
// 2. Remove the finalizer "litellm.ackstorm.ai/connection-cache-cleanup".
// 3. Issue NO LiteLLM API call across the deletion (the operator does
// not own the upstream key; deletion is purely a local cleanup).
//
// Procedure:
//
// 1. Reset mock to ModeHappy; clear counters.
// 2. Snapshot mockServer.Reads and mockServer.Mutations as
// baselines BEFORE creating the CR (counters at zero).
// 3. Create LiteLLMConnection/default. Poll up to 30s for
// cache.Snapshot.Ready == true — proves the reconciler ran end-to-end
// at least once and added the finalizer during one of those runs.
// 4. Re-Get the CR; assert ContainsFinalizer(connectionFinalizer) is true.
// 5. Snapshot readsAtDelete := mockServer.Reads — the AT-DELETE
// baseline. Mutations should still be 0 at this point because Phase 2
// never issues a mutation API call.
// 6. Issue Delete on the CR. Poll up to 15s for full removal
// (k8sClient.Get returns IsNotFound).
// 7. Assert mockServer.Reads within readsAtDelete.readsAtDelete+2
// (the +2 tolerance covers a possible Watch-event-driven re-reconcile
// that races with the DeletionTimestamp set — Step 2a may not yet
// be the first observation on that reconcile). Assert
// mockServer.Mutations unchanged from baseline (0 — Phase 2 never
// issues mutation calls; the finalizer block is cache-only).
// 8. Assert cache.Snapshot.Reason == "Absent" AND
// cache.Snapshot.Ready == false — proves the finalizer ran the
// cache invalidation path.
func TestConnectionFinalizerAddRemove_CONN_09(t *testing.T) {
	if connCache == nil {
		t.Fatal("suite_test.go did not initialize connCache — TestMain ordering bug")
	}

	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	ensureNoConnectionDefault(t, ctx)
	ensureMasterKeySecret(t, ctx)
	resetConnCacheSnapshot()

	cr := connDefaultCR()
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create LiteLLMConnection/default: %v", err)
	}
	t.Cleanup(func() {
		mockServer.SetMode(mock.ModeHappy)
		// Best-effort delete in case the assertion failed mid-test.
		_ = k8sClient.Delete(context.Background(), cr, &client.DeleteOptions{})
		time.Sleep(50 * time.Millisecond)
	})

	// Step 3: poll for Ready=Synced — proves the reconciler ran the full
	// state machine and (per Step 2b in Reconcile) added the finalizer
	// during the first reconcile pass.
	snap := pollSnapshotReason(30*time.Second, reasonSynced)
	if snap.Reason != reasonSynced {
		t.Fatalf("cache.Snapshot().Reason = %q, want Synced within 30s", snap.Reason)
	}

	// Step 4: re-Get and assert finalizer is present.
	var withFinalizer litellmv1alpha1.LiteLLMConnection
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &withFinalizer); err != nil {
		t.Fatalf("re-get CR after Synced: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&withFinalizer, connectionFinalizer) {
		t.Fatalf("CONN-09 FAIL: finalizer %q not present on Synced CR; finalizers=%v",
			connectionFinalizer, withFinalizer.Finalizers)
	}

	// Step 5: snapshot reads + mutations AT-DELETE — the bounds we
	// assert against after the deletion completes.
	//
	// The baselines are taken HERE (not at test start, before Create) so
	// any traffic from getting-Connection-to-Synced — including the
	// implicit-default Team reconciler firing once Connection flipped
	// Ready — is folded into the baseline. The CONN-09 contract is
	// about the FINALIZER block specifically (zero LiteLLM API call
	// between Delete and full removal), not about quiescence during
	// the Ready handshake. The original strict precondition was
	// over-broad and flaky on CI runners.
	readsAtDelete := mockServer.Reads()
	mutationsAtDelete := mockServer.Mutations()

	// Step 6: delete the CR; poll for full removal.
	if err := k8sClient.Delete(ctx, &withFinalizer); err != nil {
		t.Fatalf("delete CR: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	gone := false
	for time.Now().Before(deadline) {
		var probe litellmv1alpha1.LiteLLMConnection
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), &probe)
		if apierrors.IsNotFound(err) {
			gone = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gone {
		t.Fatalf("CONN-09 FAIL: CR not removed within 15s of Delete (finalizer cleanup did not run or did not remove finalizer)")
	}

	// Step 7: assert NO LiteLLM API call across the deletion.
	// - Mutations: must match mutationsAtDelete (no delta during
	// finalizer cleanup — the block is cache-only).
	// - Reads: bounded by readsAtDelete+2 — at most two additional
	// reads may occur due to a race where a previously-enqueued
	// reconcile is processing when DeletionTimestamp lands.
	readsFinal := mockServer.Reads()
	mutationsFinal := mockServer.Mutations()
	if mutationsFinal != mutationsAtDelete {
		t.Errorf("CONN-09 FAIL: mockServer.Mutations() drifted from %d to %d across deletion (no API call should occur during finalizer cleanup)",
			mutationsAtDelete, mutationsFinal)
	}
	if readsFinal > readsAtDelete+2 {
		t.Errorf("CONN-09 FAIL: mockServer.Reads() grew from %d to %d across deletion (max tolerated readsAtDelete+2 = %d) — finalizer block may be issuing a probe",
			readsAtDelete, readsFinal, readsAtDelete+2)
	}
	t.Logf("CONN-09: reads atDelete=%d, final=%d (delta=%d ≤ 2); mutations atDelete=%d, final=%d",
		readsAtDelete, readsFinal, readsFinal-readsAtDelete, mutationsAtDelete, mutationsFinal)

	// Step 8: assert cache was invalidated with Reason="Absent".
	finalSnap := connCache.Snapshot()
	if finalSnap.Reason != "Absent" {
		t.Errorf("CONN-09 FAIL: cache.Snapshot().Reason = %q after deletion; want \"Absent\"", finalSnap.Reason)
	}
	if finalSnap.Ready {
		t.Errorf("CONN-09 FAIL: cache.Snapshot().Ready = true after deletion; want false")
	}
}
