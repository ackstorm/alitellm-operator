// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	litellmv1alpha1 "github.com/ackstorm/alitellm-operator/api/litellm/v1alpha1"
	"github.com/ackstorm/alitellm-operator/internal/connection"
	"github.com/ackstorm/alitellm-operator/internal/controller/deletionpolicy"
	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// TestModelDiscoveryChildForcedOrphan — Issue #23 envtest, plus the
// outage-leak fix (openrouter.ai21-jamba-large-1.7, 2026-08-19).
//
// A Model CR owned by a LiteLLMModelDiscovery parent (controller:true)
// must always resolve to Orphan regardless of spec.deletionPolicy=Delete
// or annotation override=Delete. Orphan gives up on the LiteLLM delete
// only for a PERMANENT cause:
//   - Unreachable (transient): the child stays Terminating with its
//     finalizer and no LiteLLM call; once the connection is Synced again
//     POST /model/delete reaches LiteLLM and the CR goes away.
//   - Absent (LiteLLMConnection deleted): the finalizer drains at once.
//
// The child is created with a synthetic ownerReference (no live Discovery
// parent needed); a vanish-delete by the Discovery is the same Delete call.
func TestModelDiscoveryChildForcedOrphan(t *testing.T) {
	t.Run("Unreachable defers until Synced", func(t *testing.T) {
		got, key := createForcedOrphanChild(t, "discovery-child-orphan-deferred")
		deleteChildWhileUnavailable(t, got, "Unreachable")

		// Transient outage: still Terminating with the finalizer, LiteLLM untouched.
		time.Sleep(3 * time.Second)
		var cur litellmv1alpha1.LiteLLMModel
		if err := k8sClient.Get(context.Background(), key, &cur); err != nil {
			t.Fatalf("child must stay Terminating while LiteLLM is Unreachable: %v", err)
		}
		if !controllerutil.ContainsFinalizer(&cur, modelFinalizer) {
			t.Fatalf("finalizer dropped during a transient outage — LiteLLM row would leak")
		}
		if n := countModelDeletes(); n != 0 {
			t.Fatalf("POST /model/delete issued %d time(s) while not usable; want 0", n)
		}

		// Recovery: the deferred delete lands and the CR disappears.
		setConnCacheReady()
		if !waitModelGone(key, 45*time.Second) {
			t.Fatalf("child not deleted within 45s of connection recovery")
		}
		if countModelDeletes() == 0 {
			t.Fatalf("CR gone but POST /model/delete never reached LiteLLM")
		}
	})

	t.Run("Absent drains immediately", func(t *testing.T) {
		got, key := createForcedOrphanChild(t, "discovery-child-orphan-absent")
		deleteChildWhileUnavailable(t, got, reasonAbsent)
		if !waitModelGone(key, 10*time.Second) {
			t.Fatalf("Discovery-owned child did not drain within 10s with connection Absent")
		}
		if n := countModelDeletes(); n != 0 {
			t.Fatalf("POST /model/delete issued %d time(s) while Absent; want 0", n)
		}
	})
}

// createForcedOrphanChild creates a Discovery-owned Model whose spec and
// annotation both say Delete, and waits for its finalizer.
func createForcedOrphanChild(t *testing.T, name string) (*litellmv1alpha1.LiteLLMModel, client.ObjectKey) {
	t.Helper()
	ctx := context.Background()
	mockServer.SetMode(mock.ModeHappy)
	mockServer.ResetCounters()
	mockServer.ResetRecorded()
	mockServer.ResetModels()
	ensureNoModel(t, ctx, name)
	resetConnCacheSnapshot()

	// Connection Ready so the create + finalizer-add path runs cleanly.
	ensureNoConnectionDefault(t, ctx)
	if err := k8sClient.Create(ctx, connDefaultCR()); err != nil {
		t.Fatalf("create LiteLLMConnection: %v", err)
	}
	t.Cleanup(func() {
		// Restore a VALID Ready snapshot (Client-backed) so a leftover
		// finalizer can drain. A Ready+nil-Client snapshot poisons the
		// shared singleton and panics the next reconcile (issue #74).
		setConnCacheReady()
		ensureNoModel(t, context.Background(), name)
		ensureNoConnectionDefault(t, context.Background())
		// The connection CR is usually already gone, so no Absent rebuild
		// fires: reset explicitly or the Ready snapshot bleeds into later
		// tests (AC-N4 counts implicit Team/default reads).
		resetConnCacheSnapshot()
	})

	ctrlTrue := true
	cr := &litellmv1alpha1.LiteLLMModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: WatchNamespace,
			Annotations: map[string]string{
				// Annotation says Delete — resolver MUST still force Orphan
				// because of the Discovery owner.
				deletionpolicy.AnnotationOverride: string(deletionpolicy.Delete),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: litellmv1alpha1.GroupVersion.String(),
				Kind:       "LiteLLMModelDiscovery",
				Name:       "synthetic-parent",
				// Stable synthetic UID. The reconciler only inspects
				// ref.Controller + ref.Kind.
				UID:        types.UID("00000000-0000-0000-0000-000000000023"),
				Controller: &ctrlTrue,
			}},
		},
		Spec: litellmv1alpha1.ModelSpec{
			// Spec also says Delete — Discovery rule must still win.
			DeletionPolicy: string(deletionpolicy.Delete),
			Params: runtime.RawExtension{
				Raw: []byte(`{"model":"openai/gpt-4o-mini","rpm":100}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatalf("create child Model: %v", err)
	}

	// Wait until the row exists in LiteLLM (modelID persisted) so the
	// deferred delete has something to delete.
	key := client.ObjectKey{Name: name, Namespace: WatchNamespace}
	var got litellmv1alpha1.LiteLLMModel
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(ctx, key, &got); err == nil &&
			controllerutil.ContainsFinalizer(&got, modelFinalizer) && got.Status.LastRendered.ModelID != "" {
			return &got, key
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child never reached finalizer + modelID")
	return nil, key
}

// deleteChildWhileUnavailable removes the LiteLLMConnection (so its probe
// cannot flip the cache back), pins the snapshot to reason, resets the mock
// recorder and deletes the child.
func deleteChildWhileUnavailable(t *testing.T, got *litellmv1alpha1.LiteLLMModel, reason string) {
	t.Helper()
	ctx := context.Background()
	ensureNoConnectionDefault(t, ctx)
	connCache.Rebuild(connection.ConnectionSnapshot{Ready: false, Reason: reason})
	mockServer.ResetRecorded()
	if err := k8sClient.Delete(ctx, got); err != nil {
		t.Fatalf("delete child: %v", err)
	}
}

func countModelDeletes() int {
	n := 0
	for _, c := range mockServer.Recorded() {
		if c.Method == http.MethodPost && c.Path == pathModelDelete {
			n++
		}
	}
	return n
}

func waitModelGone(key client.ObjectKey, timeout time.Duration) bool {
	var m litellmv1alpha1.LiteLLMModel
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := k8sClient.Get(context.Background(), key, &m); apierrors.IsNotFound(err) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
